package wsbridge

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/tl"
)

// isPrivateIP returns true if the IP is in a private, loopback, or link-local range.
// Uses pre-parsed privateNets from helpers.go for efficiency.
func isPrivateIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range privateNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientOwnsPeer checks if the given peer hex ID belongs to this client.
func clientOwnsPeer(client *wsClient, peerHex string) bool {
	client.peersMu.Lock()
	defer client.peersMu.Unlock()
	for _, p := range client.peers {
		if p == peerHex {
			return true
		}
	}
	return false
}

// setupPeerHandlers configures message and disconnect handlers for a connected peer.
// If owner is non-nil, events are sent only to that client; otherwise they are broadcast.
func (b *WSBridge) setupPeerHandlers(peer adnl.Peer, owner *wsClient) {
	peerID := base64.StdEncoding.EncodeToString(peer.GetID())

	peer.SetCustomMessageHandler(func(msg *adnl.MessageCustom) error {
		var msgData []byte
		switch v := msg.Data.(type) {
		case RawMessage:
			msgData = v.Data
		default:
			// Serialize the TL object back to bytes for forwarding
			serialized, err := tl.Serialize(v, true)
			if err != nil {
				log.Warn().Err(err).Msg("failed to serialize incoming ADNL message")
				return nil
			}
			msgData = serialized
		}
		event := map[string]interface{}{
			"from":    peerID,
			"message": base64.StdEncoding.EncodeToString(msgData),
		}
		if owner != nil {
			b.sendEvent(owner, "adnl.message", event)
		} else {
			b.broadcastToClients("adnl.message", event)
		}
		return nil
	})

	peer.SetDisconnectHandler(func(addr string, key ed25519.PublicKey) {
		disconnectedPeerHex := hex.EncodeToString(peer.GetID())

		b.activePeersMu.Lock()
		delete(b.activePeers, disconnectedPeerHex)
		b.activePeersMu.Unlock()

		// F7: Close and remove any overlays that were built on this peer
		b.overlayToPeerMu.Lock()
		var orphanedOverlays []string
		for overlayHex, ownerPeerHex := range b.overlayToPeer {
			if ownerPeerHex == disconnectedPeerHex {
				orphanedOverlays = append(orphanedOverlays, overlayHex)
				delete(b.overlayToPeer, overlayHex)
			}
		}
		b.overlayToPeerMu.Unlock()

		for _, overlayHex := range orphanedOverlays {
			b.activeOverlaysMu.Lock()
			if ow, ok := b.activeOverlays[overlayHex]; ok {
				ow.Close()
				delete(b.activeOverlays, overlayHex)
			}
			b.activeOverlaysMu.Unlock()
		}

		event := map[string]interface{}{
			"peer": peerID,
		}
		if owner != nil {
			b.sendEvent(owner, "adnl.disconnected", event)
		} else {
			b.broadcastToClients("adnl.disconnected", event)
		}
	})
}

func (b *WSBridge) handleADNLConnect(client *wsClient, req *WSRequest) {
	var params struct {
		Address string `json:"address"`
		Key     string `json:"key"` // base64-encoded ed25519 public key
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	// B4: Limit peers per client
	client.peersMu.Lock()
	peerCount := len(client.peers)
	client.peersMu.Unlock()
	if peerCount >= b.cfg.Namespaces.ADNL.MaxPeers {
		b.sendError(client, req.ID, fmt.Sprintf("max peers limit reached (%d)", b.cfg.Namespaces.ADNL.MaxPeers), -32602)
		return
	}

	keyBytes, err := decodeBase64(params.Key)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 key: "+err.Error(), -32602)
		return
	}
	if len(keyBytes) != ed25519.PublicKeySize {
		b.sendError(client, req.ID, fmt.Sprintf("invalid key length: expected %d, got %d", ed25519.PublicKeySize, len(keyBytes)), -32602)
		return
	}
	pubKey := ed25519.PublicKey(keyBytes)

	// B3: Reject private/loopback addresses (SSRF protection)
	host, _, err := net.SplitHostPort(params.Address)
	if err != nil {
		b.sendError(client, req.ID, "invalid address format: "+err.Error(), -32602)
		return
	}
	ip := net.ParseIP(host)
	if ip == nil {
		b.sendError(client, req.ID, "invalid IP address", -32602)
		return
	}
	if b.cfg.Namespaces.ADNL.SSRFProtection && isPrivateIP(ip) {
		b.sendError(client, req.ID, "private/loopback addresses not allowed", -32602)
		return
	}

	peer, err := b.gate.RegisterClient(params.Address, pubKey)
	if err != nil {
		b.sendError(client, req.ID, "adnl connect failed: "+err.Error())
		return
	}

	b.setupPeerHandlers(peer, client)

	peerHex := hex.EncodeToString(peer.GetID())
	b.activePeersMu.Lock()
	b.activePeers[peerHex] = peer
	b.activePeersMu.Unlock()

	// Track peer for cleanup on WS disconnect
	client.peersMu.Lock()
	client.peers = append(client.peers, peerHex)
	client.peersMu.Unlock()

	b.sendResult(client, req.ID, map[string]interface{}{
		"connected":   true,
		"peer_id":     base64.StdEncoding.EncodeToString(peer.GetID()),
		"remote_addr": peer.RemoteAddr(),
	})
}

func (b *WSBridge) handleADNLConnectByADNL(client *wsClient, req *WSRequest) {
	var params struct {
		ADNLID string `json:"adnl_id"` // base64-encoded ADNL ID
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	// B4: Limit peers per client
	client.peersMu.Lock()
	peerCount := len(client.peers)
	client.peersMu.Unlock()
	if peerCount >= b.cfg.Namespaces.ADNL.MaxPeers {
		b.sendError(client, req.ID, fmt.Sprintf("max peers limit reached (%d)", b.cfg.Namespaces.ADNL.MaxPeers), -32602)
		return
	}

	adnlID, err := decodeBase64(params.ADNLID)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 adnl_id: "+err.Error(), -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.DHT.Timeout)
	defer cancel()

	addrs, pubKey, err := b.dht.FindAddresses(ctx, adnlID)
	if err != nil {
		b.sendError(client, req.ID, "dht resolve failed: "+err.Error())
		return
	}

	if len(addrs.Addresses) == 0 {
		b.sendError(client, req.ID, "no addresses found for ADNL ID", -32602)
		return
	}

	// C8: SSRF protection — reject private/loopback addresses resolved via DHT
	if b.cfg.Namespaces.ADNL.SSRFProtection && isPrivateIP(addrs.Addresses[0].IP) {
		b.sendError(client, req.ID, "private/loopback addresses not allowed", -32602)
		return
	}

	addr := fmt.Sprintf("%s:%d", addrs.Addresses[0].IP.String(), addrs.Addresses[0].Port)

	peer, err := b.gate.RegisterClient(addr, pubKey)
	if err != nil {
		b.sendError(client, req.ID, "adnl connect failed: "+err.Error())
		return
	}

	b.setupPeerHandlers(peer, client)

	peerHex := hex.EncodeToString(peer.GetID())
	b.activePeersMu.Lock()
	b.activePeers[peerHex] = peer
	b.activePeersMu.Unlock()

	// Track peer for cleanup on WS disconnect
	client.peersMu.Lock()
	client.peers = append(client.peers, peerHex)
	client.peersMu.Unlock()

	b.sendResult(client, req.ID, map[string]interface{}{
		"connected":   true,
		"peer_id":     base64.StdEncoding.EncodeToString(peer.GetID()),
		"remote_addr": peer.RemoteAddr(),
	})
}

func (b *WSBridge) handleADNLSendMessage(client *wsClient, req *WSRequest) {
	var params struct {
		PeerID string `json:"peer_id"` // base64
		Data   string `json:"data"`    // base64
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	peerIDBytes, err := decodeBase64(params.PeerID)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 peer_id: "+err.Error(), -32602)
		return
	}

	data, err := decodeBase64(params.Data)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 data: "+err.Error(), -32602)
		return
	}

	peerHex := hex.EncodeToString(peerIDBytes)
	b.activePeersMu.RLock()
	peer, ok := b.activePeers[peerHex]
	b.activePeersMu.RUnlock()
	if !ok {
		b.sendError(client, req.ID, "peer not found", -32602)
		return
	}

	if !clientOwnsPeer(client, peerHex) {
		b.sendError(client, req.ID, "peer not owned by this client", -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.ADNL.Timeout)
	defer cancel()

	if err := peer.SendCustomMessage(ctx, RawMessage{Data: data}); err != nil {
		b.sendError(client, req.ID, "send failed: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]interface{}{
		"sent": true,
	})
}

func (b *WSBridge) handleADNLPing(client *wsClient, req *WSRequest) {
	var params struct {
		PeerID string `json:"peer_id"` // base64
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	peerIDBytes, err := decodeBase64(params.PeerID)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 peer_id: "+err.Error(), -32602)
		return
	}

	peerHex := hex.EncodeToString(peerIDBytes)
	b.activePeersMu.RLock()
	peer, ok := b.activePeers[peerHex]
	b.activePeersMu.RUnlock()
	if !ok {
		b.sendError(client, req.ID, "peer not found", -32602)
		return
	}

	if !clientOwnsPeer(client, peerHex) {
		b.sendError(client, req.ID, "peer not owned by this client", -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.ADNL.Timeout)
	defer cancel()

	latency, err := peer.Ping(ctx)
	if err != nil {
		b.sendError(client, req.ID, "ping failed: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]interface{}{
		"latency_ms": latency.Milliseconds(),
	})
}

func (b *WSBridge) handleADNLDisconnect(client *wsClient, req *WSRequest) {
	var params struct {
		PeerID string `json:"peer_id"` // base64
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	peerIDBytes, err := decodeBase64(params.PeerID)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 peer_id: "+err.Error(), -32602)
		return
	}

	peerHex := hex.EncodeToString(peerIDBytes)
	b.activePeersMu.Lock()
	peer, ok := b.activePeers[peerHex]
	if ok {
		delete(b.activePeers, peerHex)
	}
	b.activePeersMu.Unlock()

	if !ok {
		b.sendError(client, req.ID, "peer not found", -32602)
		return
	}

	if !clientOwnsPeer(client, peerHex) {
		b.sendError(client, req.ID, "peer not owned by this client", -32602)
		return
	}

	// M6: Remove from client's tracked peers
	client.peersMu.Lock()
	for i, p := range client.peers {
		if p == peerHex {
			client.peers = append(client.peers[:i], client.peers[i+1:]...)
			break
		}
	}
	client.peersMu.Unlock()

	peer.Close()

	b.sendResult(client, req.ID, map[string]interface{}{
		"disconnected": true,
	})
}

func (b *WSBridge) handleADNLPeers(client *wsClient, req *WSRequest) {
	client.peersMu.Lock()
	peerIDs := make([]string, len(client.peers))
	copy(peerIDs, client.peers)
	client.peersMu.Unlock()

	type peerInfo struct {
		ID   string `json:"id"`
		Addr string `json:"addr"`
	}

	peers := make([]peerInfo, 0, len(peerIDs))
	b.activePeersMu.RLock()
	for _, peerHex := range peerIDs {
		if peer, ok := b.activePeers[peerHex]; ok {
			peers = append(peers, peerInfo{
				ID:   base64.StdEncoding.EncodeToString(peer.GetID()),
				Addr: peer.RemoteAddr(),
			})
		}
	}
	b.activePeersMu.RUnlock()

	b.sendResult(client, req.ID, map[string]interface{}{
		"peers": peers,
	})
}

func (b *WSBridge) handleADNLQuery(client *wsClient, req *WSRequest) {
	var params struct {
		PeerID  string `json:"peer_id"`  // base64
		Data    string `json:"data"`     // base64 TL-serialized request
		Timeout int    `json:"timeout"`  // optional, seconds (default 15)
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	peerIDBytes, err := decodeBase64(params.PeerID)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 peer_id: "+err.Error(), -32602)
		return
	}

	data, err := decodeBase64(params.Data)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 data: "+err.Error(), -32602)
		return
	}

	peerHex := hex.EncodeToString(peerIDBytes)
	b.activePeersMu.RLock()
	peer, ok := b.activePeers[peerHex]
	b.activePeersMu.RUnlock()
	if !ok {
		b.sendError(client, req.ID, "peer not found", -32602)
		return
	}

	if !clientOwnsPeer(client, peerHex) {
		b.sendError(client, req.ID, "peer not owned by this client", -32602)
		return
	}

	if params.Timeout <= 0 {
		params.Timeout = int(b.cfg.Namespaces.ADNL.Timeout.Seconds())
	}
	if params.Timeout > int(b.cfg.Namespaces.ADNL.QueryMaxTimeout.Seconds()) {
		params.Timeout = int(b.cfg.Namespaces.ADNL.QueryMaxTimeout.Seconds())
	}

	ctx, cancel := context.WithTimeout(client.ctx, time.Duration(params.Timeout)*time.Second)
	defer cancel()

	var result any
	if err := peer.Query(ctx, RawMessage{Data: data}, &result); err != nil {
		b.sendError(client, req.ID, "query failed: "+err.Error())
		return
	}

	if result == nil {
		b.sendResult(client, req.ID, map[string]interface{}{
			"data": "",
		})
		return
	}

	resultBytes, err := tl.Serialize(result, true)
	if err != nil {
		b.sendResult(client, req.ID, map[string]interface{}{
			"data": base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%v", result))),
		})
		return
	}

	b.sendResult(client, req.ID, map[string]interface{}{
		"data": base64.StdEncoding.EncodeToString(resultBytes),
	})
}

func (b *WSBridge) handleADNLSetQueryHandler(client *wsClient, req *WSRequest) {
	var params struct {
		PeerID string `json:"peer_id"` // base64
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	peerIDBytes, err := decodeBase64(params.PeerID)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 peer_id: "+err.Error(), -32602)
		return
	}

	peerHex := hex.EncodeToString(peerIDBytes)
	b.activePeersMu.RLock()
	peer, ok := b.activePeers[peerHex]
	b.activePeersMu.RUnlock()
	if !ok {
		b.sendError(client, req.ID, "peer not found", -32602)
		return
	}

	if !clientOwnsPeer(client, peerHex) {
		b.sendError(client, req.ID, "peer not owned by this client", -32602)
		return
	}

	base64PeerID := base64.StdEncoding.EncodeToString(peer.GetID())

	peer.SetQueryHandler(func(msg *adnl.MessageQuery) error {
		queryID := hex.EncodeToString(msg.ID)

		var msgData []byte
		switch v := msg.Data.(type) {
		case RawMessage:
			msgData = v.Data
		default:
			serialized, err := tl.Serialize(v, true)
			if err != nil {
				return nil
			}
			msgData = serialized
		}

		b.pendingQueriesMu.Lock()
		b.pendingQueries[queryID] = pendingQuery{peer: peer, deadline: time.Now().Add(30 * time.Second)}
		b.pendingQueriesMu.Unlock()

		b.sendEvent(client, "adnl.queryReceived", map[string]interface{}{
			"peer_id":  base64PeerID,
			"query_id": queryID,
			"data":     base64.StdEncoding.EncodeToString(msgData),
		})
		return nil
	})

	b.sendResult(client, req.ID, map[string]interface{}{
		"enabled": true,
	})
}

func (b *WSBridge) handleADNLAnswer(client *wsClient, req *WSRequest) {
	var params struct {
		QueryID string `json:"query_id"` // hex-encoded query ID
		Data    string `json:"data"`     // base64 TL-serialized response
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	dataBytes, err := decodeBase64(params.Data)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 data: "+err.Error(), -32602)
		return
	}

	b.pendingQueriesMu.Lock()
	pq, ok := b.pendingQueries[params.QueryID]
	if ok {
		delete(b.pendingQueries, params.QueryID)
	}
	b.pendingQueriesMu.Unlock()

	if !ok || time.Now().After(pq.deadline) {
		b.sendError(client, req.ID, "query not found or expired", -32602)
		return
	}

	b.activePeersMu.RLock()
	_, peerAlive := b.activePeers[hex.EncodeToString(pq.peer.GetID())]
	b.activePeersMu.RUnlock()
	if !peerAlive {
		b.sendError(client, req.ID, "peer disconnected before answer could be sent", -32602)
		return
	}

	peer := pq.peer
	queryIDBytes, err := hex.DecodeString(params.QueryID)
	if err != nil {
		b.sendError(client, req.ID, "invalid query_id hex: "+err.Error(), -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.ADNL.Timeout)
	defer cancel()

	if err := peer.Answer(ctx, queryIDBytes, RawMessage{Data: dataBytes}); err != nil {
		b.sendError(client, req.ID, "answer failed: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]interface{}{
		"answered": true,
	})
}
