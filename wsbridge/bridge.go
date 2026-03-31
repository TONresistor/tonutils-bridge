package wsbridge

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/adnl/dht"
	"github.com/xssnick/tonutils-go/adnl/overlay"
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/dns"
)

const (
	writeWait  = 10 * time.Second   // time allowed to write a message
	pongWait   = 60 * time.Second   // time allowed to read the next pong
	pingPeriod = (pongWait * 9) / 10 // send pings at this interval (must be < pongWait)
)

const (
	maxWSClients              = 100
	maxWSMessageSize          = 1 << 20 // 1 MB
	maxPeersPerClient         = 20
	maxOverlaysPerClient      = 10
	maxSubscriptionsPerClient = 50
	maxPendingQueryTTL        = 30 * time.Second
	pendingQuerySweepInterval = 10 * time.Second
	maxInflightRequests       = 100
)

// RawMessage is a simple TL type for sending raw bytes over ADNL
type RawMessage struct {
	Data []byte `tl:"bytes"`
}

// OverlayKey matches adnlTunnel.overlayKey from adnl-tunnel
type OverlayKey struct {
	PaymentNode []byte `tl:"int256"`
}

func init() {
	tl.Register(RawMessage{}, "ws.rawMessage data:bytes = ws.RawMessage")
}

// RegisterOverlayKey registers the OverlayKey TL type. Call this only when
// the adnl-tunnel package is NOT imported (it registers the same schema).
// When adnl-tunnel IS imported (e.g. in main.go), skip this call.
func RegisterOverlayKey() {
	tl.Register(OverlayKey{}, "adnlTunnel.overlayKey paymentNode:int256 = adnlTunnel.OverlayKey")
}

// WSRequest is a JSON-RPC 2.0 request from the WebSocket client
type WSRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// WSResponse is a JSON-RPC 2.0 response to the WebSocket client
type WSResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      string      `json:"id"`
	Result  any `json:"result,omitempty"`
	Error   *RPCError   `json:"error,omitempty"`
}

// RPCError follows JSON-RPC 2.0 error format
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// wsClient wraps a WebSocket connection with a write mutex for safe concurrent writes.
type wsClient struct {
	conn       *websocket.Conn
	mu         sync.Mutex       // write mutex — gorilla/websocket does not support concurrent writers
	peersMu    sync.Mutex       // guards peers and overlays slices
	ctx        context.Context  // cancelled when handleWS exits; used to stop subscriptions
	peers      []string         // ADNL peer IDs created by this client (hex-encoded)
	overlays   []string         // overlay IDs joined by this client (hex-encoded, for cleanup)
	wg         sync.WaitGroup   // tracks in-flight handleRequest goroutines
	activeSubs int32            // number of active subscriptions (accessed via sync/atomic)

	subscriptionsMu sync.Mutex                    // guards subscriptions map
	subscriptions   map[string]context.CancelFunc  // subscription ID → cancel function
	subCounter      uint64                         // monotonic counter for subscription IDs
	inflightSem chan struct{} // limits concurrent in-flight requests
}

// WSBridge exposes ADNL/DHT/Lite/DNS operations via WebSocket
type WSBridge struct {
	dht      *dht.Client
	api      ton.APIClientWrapped
	dns      *dns.Client
	gate     *adnl.Gateway
	key      ed25519.PrivateKey
	upgrader websocket.Upgrader
	clients  map[*wsClient]bool
	mu       sync.RWMutex

	activePeers   map[string]adnl.Peer
	activePeersMu sync.RWMutex

	activeOverlays   map[string]*overlay.ADNLOverlayWrapper
	activeOverlaysMu sync.RWMutex

	pendingQueries   map[string]pendingQuery // query ID (hex) → peer + deadline
	pendingQueriesMu sync.RWMutex

	overlayToPeer   map[string]string // overlay hex → peer hex (moved from overlay.go global)
	overlayToPeerMu sync.Mutex
}

// pendingQuery holds the peer that sent an inbound ADNL/overlay query together
// with the absolute deadline after which the entry is considered stale.
type pendingQuery struct {
	peer     adnl.Peer
	deadline time.Time
}

// NewWSBridge creates a new WebSocket-ADNL bridge
func NewWSBridge(dhtClient *dht.Client, api ton.APIClientWrapped, dnsClient *dns.Client, gate *adnl.Gateway, key ed25519.PrivateKey) *WSBridge {
	bridge := &WSBridge{
		dht:            dhtClient,
		api:            api,
		dns:            dnsClient,
		gate:           gate,
		key:            key,
		clients:        make(map[*wsClient]bool),
		activePeers:    make(map[string]adnl.Peer),
		activeOverlays: make(map[string]*overlay.ADNLOverlayWrapper),
		pendingQueries: make(map[string]pendingQuery),
		overlayToPeer:  make(map[string]string),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true // No origin header (non-browser clients, curl, etc.)
				}
				u, err := url.Parse(origin)
				if err != nil {
					return false
				}
				host := u.Hostname()
				return host == "127.0.0.1" || host == "localhost" || host == "::1"
			},
		},
	}

	// Register inbound connection handler on the ADNL gateway
	gate.SetConnectionHandler(func(peer adnl.Peer) error {
		bridge.setupPeerHandlers(peer, nil)

		peerHex := hex.EncodeToString(peer.GetID())
		bridge.activePeersMu.Lock()
		bridge.activePeers[peerHex] = peer
		bridge.activePeersMu.Unlock()

		bridge.broadcastToClients("adnl.incomingConnection", map[string]any{
			"peer_id":     base64.StdEncoding.EncodeToString(peer.GetID()),
			"remote_addr": peer.RemoteAddr(),
		})
		return nil
	})

	return bridge
}

// Start launches the WebSocket server on the given address
func (b *WSBridge) Start(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", b.handleWS)

	server := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		server.Shutdown(shutCtx)
	}()

	// Background goroutine: evict expired pendingQueries entries every 10 seconds.
	go func() {
		ticker := time.NewTicker(pendingQuerySweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := time.Now()
				b.pendingQueriesMu.Lock()
				for id, pq := range b.pendingQueries {
					if now.After(pq.deadline) {
						delete(b.pendingQueries, id)
					}
				}
				b.pendingQueriesMu.Unlock()
			}
		}
	}()

	log.Info().Str("addr", addr).Msg("WebSocket-ADNL bridge started")
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (b *WSBridge) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := b.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error().Err(err).Msg("ws upgrade failed")
		return
	}
	conn.SetReadLimit(maxWSMessageSize)

	connCtx, connCancel := context.WithCancel(r.Context())
	defer conn.Close()

	client := &wsClient{conn: conn, ctx: connCtx, subscriptions: make(map[string]context.CancelFunc), inflightSem: make(chan struct{}, maxInflightRequests)}

	// Serialize pong writes through the same mutex to prevent concurrent WriteControl/WriteMessage
	conn.SetPingHandler(func(msg string) error {
		client.mu.Lock()
		defer client.mu.Unlock()
		return conn.WriteControl(websocket.PongMessage, []byte(msg), time.Now().Add(5*time.Second))
	})

	// Server-side read deadline + pong handler: detect dead connections.
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	b.mu.Lock()
	if len(b.clients) >= maxWSClients {
		b.mu.Unlock()
		connCancel()
		conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "too many connections"))
		return
	}
	b.clients[client] = true
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.clients, client)
		b.mu.Unlock()
	}()

	// Clean up overlays joined by this client on disconnect
	defer func() {
		client.peersMu.Lock()
		defer client.peersMu.Unlock()
		for _, overlayHex := range client.overlays {
			b.activeOverlaysMu.Lock()
			if ow, ok := b.activeOverlays[overlayHex]; ok {
				ow.Close()
				delete(b.activeOverlays, overlayHex)
			}
			b.activeOverlaysMu.Unlock()
		}
	}()

	// Clean up ADNL peers created by this client on disconnect
	defer func() {
		client.peersMu.Lock()
		defer client.peersMu.Unlock()
		for _, peerID := range client.peers {
			b.activePeersMu.Lock()
			if p, ok := b.activePeers[peerID]; ok {
				p.Close()
				delete(b.activePeers, peerID)
			}
			b.activePeersMu.Unlock()
		}
	}()

	// Registered LAST so it runs FIRST in LIFO order: cancels context to stop
	// subscriptions, then waits for all in-flight request handlers to finish
	// before the cleanup defers above iterate client.peers / client.overlays.
	defer func() {
		connCancel()
		client.wg.Wait()
	}()

	// Ping pump: periodically ping clients so dead connections are detected.
	client.wg.Add(1)
	go func() {
		defer client.wg.Done()
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-ticker.C:
				client.mu.Lock()
				conn.SetWriteDeadline(time.Now().Add(writeWait))
				err := conn.WriteMessage(websocket.PingMessage, nil)
				conn.SetWriteDeadline(time.Time{}) // clear deadline
				client.mu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()

	log.Info().Str("remote", r.RemoteAddr).Msg("ws client connected")

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var req WSRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			b.sendError(client, "", "invalid JSON: "+err.Error(), -32700)
			continue
		}

		client.wg.Add(1)
		go func() {
			defer client.wg.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Error().Interface("panic", r).Str("method", req.Method).Msg("handler panic recovered")
					b.sendError(client, req.ID, "internal error", -32603)
				}
			}()
			select {
			case client.inflightSem <- struct{}{}:
				defer func() { <-client.inflightSem }()
				b.handleRequest(client, &req)
			default:
				b.sendError(client, req.ID, "too many concurrent requests", -32603)
			}
		}()
	}
}

func (b *WSBridge) handleRequest(client *wsClient, req *WSRequest) {
	switch req.Method {
	case "dht.findAddresses":
		b.handleFindAddresses(client, req)
	case "dht.findOverlayNodes":
		b.handleFindOverlayNodes(client, req)
	case "dht.findTunnelNodes":
		b.handleFindTunnelNodes(client, req)
	case "network.info":
		b.handleNetworkInfo(client, req)
	case "lite.getMasterchainInfo":
		b.handleGetMasterchainInfo(client, req)
	case "lite.getAccountState":
		b.handleGetAccountState(client, req)
	case "lite.runMethod":
		b.handleRunMethod(client, req)
	case "lite.sendMessage":
		b.handleSendMessage(client, req)
	case "lite.getTransactions":
		b.handleGetTransactions(client, req)
	case "lite.getTime":
		b.handleGetTime(client, req)
	case "lite.lookupBlock":
		b.handleLookupBlock(client, req)
	case "lite.getBlockTransactions":
		b.handleGetBlockTransactions(client, req)
	case "lite.getShards":
		b.handleGetShards(client, req)
	case "lite.getBlockchainConfig":
		b.handleGetBlockchainConfig(client, req)
	case "lite.getTransaction":
		b.handleGetTransaction(client, req)
	case "lite.findTxByInMsgHash":
		b.handleFindTxByInMsgHash(client, req)
	// lite.sendMessageWait — fire-and-forget with a longer timeout (60s) for the
	// liteserver query. Despite the name, it does NOT wait for on-chain confirmation;
	// polling for transaction inclusion is the client's responsibility.
	case "lite.sendMessageWait":
		b.handleSendMessageWait(client, req)
	case "subscribe.transactions":
		b.handleSubscribeTransactions(client, req)
	case "subscribe.blocks":
		b.handleSubscribeBlocks(client, req)
	case "subscribe.accountState":
		b.handleSubscribeAccountState(client, req)
	case "subscribe.newTransactions":
		b.handleSubscribeNewTransactions(client, req)
	case "jetton.getData":
		b.handleJettonGetData(client, req)
	case "jetton.getWalletAddress":
		b.handleJettonGetWalletAddress(client, req)
	case "jetton.getBalance":
		b.handleJettonGetBalance(client, req)
	case "nft.getData":
		b.handleNFTGetData(client, req)
	case "nft.getCollectionData":
		b.handleNFTGetCollectionData(client, req)
	case "nft.getAddressByIndex":
		b.handleNFTGetAddressByIndex(client, req)
	case "nft.getRoyaltyParams":
		b.handleNFTGetRoyaltyParams(client, req)
	case "nft.getContent":
		b.handleNFTGetContent(client, req)
	case "wallet.getSeqno":
		b.handleWalletGetSeqno(client, req)
	case "wallet.getPublicKey":
		b.handleWalletGetPublicKey(client, req)
	case "sbt.getAuthorityAddress":
		b.handleSBTGetAuthorityAddress(client, req)
	case "sbt.getRevokedTime":
		b.handleSBTGetRevokedTime(client, req)
	case "payment.getChannelState":
		b.handlePaymentGetChannelState(client, req)
	case "dns.resolve":
		b.handleDNSResolve(client, req)
	case "adnl.connect":
		b.handleADNLConnect(client, req)
	case "adnl.connectByADNL":
		b.handleADNLConnectByADNL(client, req)
	case "adnl.sendMessage":
		b.handleADNLSendMessage(client, req)
	case "adnl.ping":
		b.handleADNLPing(client, req)
	case "adnl.disconnect":
		b.handleADNLDisconnect(client, req)
	case "adnl.peers":
		b.handleADNLPeers(client, req)
	case "overlay.join":
		b.handleOverlayJoin(client, req)
	case "overlay.leave":
		b.handleOverlayLeave(client, req)
	case "overlay.getPeers":
		b.handleOverlayGetPeers(client, req)
	case "overlay.sendMessage":
		b.handleOverlaySendMessage(client, req)
	case "dht.findValue":
		b.handleDHTFindValue(client, req)
	case "dht.storeAddress":
		b.handleDHTStoreAddress(client, req)
	case "dht.storeOverlayNodes":
		b.handleDHTStoreOverlayNodes(client, req)
	// --- WebSocket-native methods ---
	case "subscribe.unsubscribe":
		b.handleUnsubscribe(client, req)
	case "lite.sendAndWatch":
		b.handleSendAndWatch(client, req)
	case "adnl.query":
		b.handleADNLQuery(client, req)
	case "adnl.setQueryHandler":
		b.handleADNLSetQueryHandler(client, req)
	case "adnl.answer":
		b.handleADNLAnswer(client, req)
	case "overlay.query":
		b.handleOverlayQuery(client, req)
	case "overlay.setQueryHandler":
		b.handleOverlaySetQueryHandler(client, req)
	case "overlay.answer":
		b.handleOverlayAnswer(client, req)
	case "lite.findTxByOutMsgHash":
		b.handleFindTxByOutMsgHash(client, req)
	case "lite.getBlockData":
		b.handleGetBlockData(client, req)
	case "lite.getBlockHeader":
		b.handleGetBlockHeader(client, req)
	case "lite.getLibraries":
		b.handleGetLibraries(client, req)
	case "subscribe.configChanges":
		b.handleSubscribeConfigChanges(client, req)
	case "subscribe.multiAccount":
		b.handleSubscribeMultiAccount(client, req)
	case "subscribe.trace":
		b.handleSubscribeTrace(client, req)
	default:
		b.sendError(client, req.ID, fmt.Sprintf("unknown method: %s", req.Method), -32601)
	}
}

// broadcastToClients pushes an event to all connected WebSocket clients.
// Copies the client list under the lock, releases it, then writes outside the lock.
func (b *WSBridge) broadcastToClients(event string, data any) {
	msg := map[string]any{"event": event, "data": data}
	jsonData, err := json.Marshal(msg)
	if err != nil {
		log.Error().Err(err).Msg("failed to marshal broadcast")
		return
	}

	b.mu.RLock()
	clients := make([]*wsClient, 0, len(b.clients))
	for c := range b.clients {
		clients = append(clients, c)
	}
	b.mu.RUnlock()

	for _, c := range clients {
		c.mu.Lock()
		c.conn.SetWriteDeadline(time.Now().Add(writeWait))
		if err := c.conn.WriteMessage(websocket.TextMessage, jsonData); err != nil {
			log.Debug().Err(err).Msg("ws broadcast write failed")
		}
		c.conn.SetWriteDeadline(time.Time{})
		c.mu.Unlock()
	}
}

// sendEvent pushes an event to a single WebSocket client and returns true on success.
func (b *WSBridge) sendEvent(client *wsClient, event string, data any) bool {
	msg := map[string]any{"event": event, "data": data}
	jsonData, err := json.Marshal(msg)
	if err != nil {
		log.Error().Err(err).Str("event", event).Msg("failed to marshal event")
		return false
	}
	client.mu.Lock()
	client.conn.SetWriteDeadline(time.Now().Add(writeWait))
	writeErr := client.conn.WriteMessage(websocket.TextMessage, jsonData)
	client.conn.SetWriteDeadline(time.Time{})
	client.mu.Unlock()
	if writeErr != nil {
		log.Debug().Err(writeErr).Msg("ws event write failed")
		return false
	}
	return true
}

func (b *WSBridge) sendResult(client *wsClient, id string, result any) {
	resp := WSResponse{JSONRPC: "2.0", ID: id, Result: result}
	data, err := json.Marshal(resp)
	if err != nil {
		log.Error().Err(err).Msg("failed to marshal result")
		return
	}
	client.mu.Lock()
	client.conn.SetWriteDeadline(time.Now().Add(writeWait))
	if err := client.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Debug().Err(err).Msg("ws write failed")
	}
	client.conn.SetWriteDeadline(time.Time{})
	client.mu.Unlock()
}

// sendError sends a JSON-RPC 2.0 error response. An optional error code can be
// provided; if omitted it defaults to -32603 (Internal error).
func (b *WSBridge) sendError(client *wsClient, id string, errMsg string, code ...int) {
	c := -32603 // default: Internal error
	if len(code) > 0 {
		c = code[0]
	}
	resp := WSResponse{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: c, Message: errMsg}}
	data, err := json.Marshal(resp)
	if err != nil {
		log.Error().Err(err).Msg("failed to marshal error")
		return
	}
	client.mu.Lock()
	client.conn.SetWriteDeadline(time.Now().Add(writeWait))
	if err := client.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		log.Debug().Err(err).Msg("ws write failed")
	}
	client.conn.SetWriteDeadline(time.Time{})
	client.mu.Unlock()
}
