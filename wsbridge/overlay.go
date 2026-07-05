package wsbridge

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/adnl/keys"
	"github.com/xssnick/tonutils-go/adnl/overlay"
	"github.com/xssnick/tonutils-go/tl"
)

// maxOverlayBroadcastSize bounds the payload accepted by overlay.broadcast to
// protect the node from oversized fan-out requests coming over the local API.
const maxOverlayBroadcastSize = 1 << 20 // 1 MiB

// clientOwnsOverlay checks if the given overlay hex ID belongs to this client.
func clientOwnsOverlay(client *wsClient, overlayHex string) bool {
	client.peersMu.Lock()
	defer client.peersMu.Unlock()
	for _, o := range client.overlays {
		if o == overlayHex {
			return true
		}
	}
	return false
}

func (b *WSBridge) handleOverlayJoin(client *wsClient, req *WSRequest) {
	var params struct {
		OverlayID string `json:"overlay_id"` // base64
		PeerID    string `json:"peer_id"`    // base64
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	// B5: Limit overlays per client
	client.peersMu.Lock()
	overlayCount := len(client.overlays)
	client.peersMu.Unlock()
	if overlayCount >= b.cfg.Namespaces.Overlay.MaxOverlays {
		b.sendError(client, req.ID, fmt.Sprintf("max overlays limit reached (%d)", b.cfg.Namespaces.Overlay.MaxOverlays), -32602)
		return
	}

	overlayID, err := decodeBase64(params.OverlayID)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 overlay_id: "+err.Error(), -32602)
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
		b.sendError(client, req.ID, "peer not found — connect first via adnl.connect", -32602)
		return
	}

	overlayHex := hex.EncodeToString(overlayID)

	// F3: Single Lock for both check and insert to avoid TOCTOU race
	b.activeOverlaysMu.Lock()
	if _, exists := b.activeOverlays[overlayHex]; exists {
		b.activeOverlaysMu.Unlock()
		b.sendResult(client, req.ID, map[string]interface{}{
			"joined":     true,
			"overlay_id": params.OverlayID,
		})
		return
	}

	// Wrap the peer with overlay support and create the overlay scope
	adnlWrapper := overlay.CreateExtendedADNL(peer)
	overlayWrapper := adnlWrapper.WithOverlay(overlayID)

	b.activeOverlays[overlayHex] = overlayWrapper
	b.activeOverlaysMu.Unlock()

	// Track overlay→peer mapping for disconnect cleanup
	b.overlayToPeerMu.Lock()
	b.overlayToPeer[overlayHex] = peerHex
	b.overlayToPeerMu.Unlock()

	// Set broadcast handler — pushes events to the owning client
	overlayWrapper.SetBroadcastHandler(func(msg tl.Serializable, trusted bool) error {
		var msgBytes []byte
		serialized, err := tl.Serialize(msg, true)
		if err != nil {
			log.Warn().Err(err).Msg("failed to serialize overlay broadcast")
			return nil
		}
		msgBytes = serialized

		b.sendEvent(client, "overlay.broadcast", map[string]interface{}{
			"overlay_id": params.OverlayID,
			"message":    base64.StdEncoding.EncodeToString(msgBytes),
			"trusted":    trusted,
		})
		return nil
	})

	overlayWrapper.SetCustomMessageHandler(func(msg *adnl.MessageCustom) error {
		var msgBytes []byte
		switch v := msg.Data.(type) {
		case RawMessage:
			msgBytes = v.Data
		default:
			serialized, err := tl.Serialize(v, true)
			if err != nil {
				log.Warn().Err(err).Msg("failed to serialize overlay custom message")
				return nil
			}
			msgBytes = serialized
		}
		b.sendEvent(client, "overlay.message", map[string]interface{}{
			"overlay_id": params.OverlayID,
			"message":    base64.StdEncoding.EncodeToString(msgBytes),
		})
		return nil
	})

	// Track overlay for cleanup on WS disconnect
	client.peersMu.Lock()
	client.overlays = append(client.overlays, overlayHex)
	client.peersMu.Unlock()

	b.sendResult(client, req.ID, map[string]interface{}{
		"joined":     true,
		"overlay_id": params.OverlayID,
	})
}

func (b *WSBridge) handleOverlayLeave(client *wsClient, req *WSRequest) {
	var params struct {
		OverlayID string `json:"overlay_id"` // base64
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	overlayID, err := decodeBase64(params.OverlayID)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 overlay_id: "+err.Error(), -32602)
		return
	}

	overlayHex := hex.EncodeToString(overlayID)

	b.activeOverlaysMu.Lock()
	ow, ok := b.activeOverlays[overlayHex]
	if ok {
		delete(b.activeOverlays, overlayHex)
	}
	b.activeOverlaysMu.Unlock()

	if !ok {
		b.sendError(client, req.ID, "overlay not found", -32602)
		return
	}

	if !clientOwnsOverlay(client, overlayHex) {
		b.sendError(client, req.ID, "overlay not owned by this client", -32602)
		return
	}

	// M7: Remove from client's tracked overlays
	client.peersMu.Lock()
	for i, o := range client.overlays {
		if o == overlayHex {
			client.overlays = append(client.overlays[:i], client.overlays[i+1:]...)
			break
		}
	}
	client.peersMu.Unlock()

	// Clean overlay→peer mapping
	b.overlayToPeerMu.Lock()
	delete(b.overlayToPeer, overlayHex)
	b.overlayToPeerMu.Unlock()

	ow.Close()

	b.sendResult(client, req.ID, map[string]interface{}{
		"left": true,
	})
}

func (b *WSBridge) handleOverlayGetPeers(client *wsClient, req *WSRequest) {
	var params struct {
		OverlayID string `json:"overlay_id"` // base64
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	overlayID, err := decodeBase64(params.OverlayID)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 overlay_id: "+err.Error(), -32602)
		return
	}

	overlayHex := hex.EncodeToString(overlayID)

	b.activeOverlaysMu.RLock()
	ow, ok := b.activeOverlays[overlayHex]
	b.activeOverlaysMu.RUnlock()
	if !ok {
		b.sendError(client, req.ID, "overlay not found — join first via overlay.join", -32602)
		return
	}

	if !clientOwnsOverlay(client, overlayHex) {
		b.sendError(client, req.ID, "overlay not owned by this client", -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Overlay.Timeout)
	defer cancel()

	nodes, err := ow.GetRandomPeers(ctx)
	if err != nil {
		b.sendError(client, req.ID, "get peers failed: "+err.Error())
		return
	}

	type peerInfo struct {
		ID      string `json:"id"`
		Overlay string `json:"overlay"`
	}

	var peers []peerInfo
	for _, node := range nodes {
		id, ok := node.ID.(keys.PublicKeyED25519)
		if !ok {
			continue
		}
		peers = append(peers, peerInfo{
			ID:      base64.StdEncoding.EncodeToString(id.Key),
			Overlay: base64.StdEncoding.EncodeToString(node.Overlay),
		})
	}

	if peers == nil {
		peers = []peerInfo{}
	}

	b.sendResult(client, req.ID, map[string]interface{}{
		"peers": peers,
	})
}

func (b *WSBridge) handleOverlaySendMessage(client *wsClient, req *WSRequest) {
	var params struct {
		OverlayID string `json:"overlay_id"` // base64
		Data      string `json:"data"`       // base64
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	overlayID, err := decodeBase64(params.OverlayID)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 overlay_id: "+err.Error(), -32602)
		return
	}

	data, err := decodeBase64(params.Data)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 data: "+err.Error(), -32602)
		return
	}

	overlayHex := hex.EncodeToString(overlayID)

	b.activeOverlaysMu.RLock()
	ow, ok := b.activeOverlays[overlayHex]
	b.activeOverlaysMu.RUnlock()
	if !ok {
		b.sendError(client, req.ID, "overlay not found — join first via overlay.join", -32602)
		return
	}

	if !clientOwnsOverlay(client, overlayHex) {
		b.sendError(client, req.ID, "overlay not owned by this client", -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Overlay.Timeout)
	defer cancel()

	if err := ow.SendCustomMessage(ctx, RawMessage{Data: data}); err != nil {
		b.sendError(client, req.ID, "send failed: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]interface{}{
		"sent": true,
	})
}

// handleOverlaySendRaw sends a client-built custom message (a full TL object
// such as a tonnet.broadcast) without wrapping it in ws.rawMessage. The bytes
// are parsed back into their registered TL type and re-sent, so the message
// travels under its own constructor rather than ws.rawMessage. Used by the
// tonnet chat protocol v0.2, whose wire unit is a client-signed broadcast.
func (b *WSBridge) handleOverlaySendRaw(client *wsClient, req *WSRequest) {
	var params struct {
		OverlayID string `json:"overlay_id"` // base64
		Data      string `json:"data"`       // base64 of the boxed TL object (e.g. tonnet.broadcast)
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	overlayID, err := decodeBase64(params.OverlayID)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 overlay_id: "+err.Error(), -32602)
		return
	}

	data, err := decodeBase64(params.Data)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 data: "+err.Error(), -32602)
		return
	}

	overlayHex := hex.EncodeToString(overlayID)

	b.activeOverlaysMu.RLock()
	ow, ok := b.activeOverlays[overlayHex]
	b.activeOverlaysMu.RUnlock()
	if !ok {
		b.sendError(client, req.ID, "overlay not found — join first via overlay.join", -32602)
		return
	}

	if !clientOwnsOverlay(client, overlayHex) {
		b.sendError(client, req.ID, "overlay not owned by this client", -32602)
		return
	}

	var obj any
	if _, err := tl.Parse(&obj, data, true); err != nil {
		b.sendError(client, req.ID, "invalid tonnet broadcast payload: "+err.Error(), -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Overlay.Timeout)
	defer cancel()

	if err := ow.SendCustomMessage(ctx, obj); err != nil {
		b.sendError(client, req.ID, "send failed: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]interface{}{
		"sent": true,
	})
}

// handleOverlayBroadcast fans a signed message out to the ENTIRE overlay using
// TON's FEC broadcast, unlike overlay.sendMessage which unicasts to the single
// joined peer. The payload is signed with the bridge node key (b.key), wrapped
// as a ws.rawMessage TL object, and pumped as RaptorQ repair symbols to the
// overlay peer set; neighbours re-gossip it so it reaches every member.
//
// Receiving nodes surface it through the broadcast handler installed in
// handleOverlayJoin, i.e. as an "overlay.broadcast" push event carrying the same
// ws.rawMessage. The sender does NOT receive an echo of its own broadcast, so a
// UI should optimistically render locally-sent messages.
func (b *WSBridge) handleOverlayBroadcast(client *wsClient, req *WSRequest) {
	var params struct {
		OverlayID string `json:"overlay_id"` // base64
		Data      string `json:"data"`       // base64 payload
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	overlayID, err := decodeBase64(params.OverlayID)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 overlay_id: "+err.Error(), -32602)
		return
	}

	data, err := decodeBase64(params.Data)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 data: "+err.Error(), -32602)
		return
	}
	if len(data) == 0 {
		b.sendError(client, req.ID, "data is empty", -32602)
		return
	}
	if len(data) > maxOverlayBroadcastSize {
		b.sendError(client, req.ID, fmt.Sprintf("data too large (%d bytes, max %d)", len(data), maxOverlayBroadcastSize), -32602)
		return
	}

	overlayHex := hex.EncodeToString(overlayID)

	b.activeOverlaysMu.RLock()
	ow, ok := b.activeOverlays[overlayHex]
	b.activeOverlaysMu.RUnlock()
	if !ok {
		b.sendError(client, req.ID, "overlay not found — join first via overlay.join", -32602)
		return
	}

	if !clientOwnsOverlay(client, overlayHex) {
		b.sendError(client, req.ID, "overlay not owned by this client", -32602)
		return
	}

	// Wrap in ws.rawMessage so receivers (which tl.Parse the reassembled payload)
	// decode it symmetrically with the overlay.sendMessage / overlay.message path.
	sender, err := overlay.NewBroadcastFECSenderFromTL(
		b.key,
		overlay.CertificateEmpty{},
		RawMessage{Data: data},
		overlay.BroadcastFlagAnySender,
	)
	if err != nil {
		b.sendError(client, req.ID, "broadcast init failed: "+err.Error())
		return
	}

	broadcaster, err := overlay.NewBroadcastFECBroadcaster(sender, overlay.StaticBroadcastPeerSet{ow})
	if err != nil {
		b.sendError(client, req.ID, "broadcaster init failed: "+err.Error())
		return
	}

	// Run() pumps repair symbols until neighbours acknowledge or the broadcast
	// TTL (~60s) elapses; it is blocking, so drive it in the background on the
	// connection-scoped context and return the broadcast id immediately.
	go func() {
		if err := broadcaster.Run(client.ctx); err != nil && client.ctx.Err() == nil {
			log.Warn().Err(err).Str("overlay", overlayHex).Msg("overlay broadcast run ended with error")
		}
	}()

	b.sendResult(client, req.ID, map[string]interface{}{
		"broadcast_id": hex.EncodeToString(sender.BroadcastHash()),
	})
}

func (b *WSBridge) handleOverlayQuery(client *wsClient, req *WSRequest) {
	var params struct {
		OverlayID string `json:"overlay_id"` // base64
		Data      string `json:"data"`       // base64 TL request
		Timeout   int    `json:"timeout"`    // optional seconds (default 15)
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	overlayID, err := decodeBase64(params.OverlayID)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 overlay_id: "+err.Error(), -32602)
		return
	}

	data, err := decodeBase64(params.Data)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 data: "+err.Error(), -32602)
		return
	}

	overlayHex := hex.EncodeToString(overlayID)
	b.activeOverlaysMu.RLock()
	ow, ok := b.activeOverlays[overlayHex]
	b.activeOverlaysMu.RUnlock()
	if !ok {
		b.sendError(client, req.ID, "overlay not found — join first via overlay.join", -32602)
		return
	}

	if !clientOwnsOverlay(client, overlayHex) {
		b.sendError(client, req.ID, "overlay not owned by this client", -32602)
		return
	}

	if params.Timeout <= 0 {
		params.Timeout = int(b.cfg.Namespaces.Overlay.Timeout.Seconds())
	}
	if params.Timeout > int(b.cfg.Namespaces.Overlay.QueryMaxTimeout.Seconds()) {
		params.Timeout = int(b.cfg.Namespaces.Overlay.QueryMaxTimeout.Seconds())
	}

	ctx, cancel := context.WithTimeout(client.ctx, time.Duration(params.Timeout)*time.Second)
	defer cancel()

	var result any
	if err := ow.Query(ctx, RawMessage{Data: data}, &result); err != nil {
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

func (b *WSBridge) handleOverlaySetQueryHandler(client *wsClient, req *WSRequest) {
	var params struct {
		OverlayID string `json:"overlay_id"` // base64
		PeerID    string `json:"peer_id"`    // base64 — ADNL peer that owns this overlay
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	overlayID, err := decodeBase64(params.OverlayID)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 overlay_id: "+err.Error(), -32602)
		return
	}

	peerIDBytes, err := decodeBase64(params.PeerID)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 peer_id: "+err.Error(), -32602)
		return
	}

	overlayHex := hex.EncodeToString(overlayID)
	b.activeOverlaysMu.RLock()
	ow, ok := b.activeOverlays[overlayHex]
	b.activeOverlaysMu.RUnlock()
	if !ok {
		b.sendError(client, req.ID, "overlay not found — join first via overlay.join", -32602)
		return
	}

	if !clientOwnsOverlay(client, overlayHex) {
		b.sendError(client, req.ID, "overlay not owned by this client", -32602)
		return
	}

	peerHex := hex.EncodeToString(peerIDBytes)
	b.activePeersMu.RLock()
	peer, ok := b.activePeers[peerHex]
	b.activePeersMu.RUnlock()
	if !ok {
		b.sendError(client, req.ID, "peer not found — connect first via adnl.connect", -32602)
		return
	}

	ow.SetQueryHandler(func(msg *adnl.MessageQuery) error {
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
		b.pendingQueries[queryID] = pendingQuery{peer: peer, deadline: time.Now().Add(maxPendingQueryTTL)}
		b.pendingQueriesMu.Unlock()

		b.sendEvent(client, "overlay.queryReceived", map[string]interface{}{
			"overlay_id": params.OverlayID,
			"query_id":   queryID,
			"data":       base64.StdEncoding.EncodeToString(msgData),
		})
		return nil
	})

	b.sendResult(client, req.ID, map[string]interface{}{
		"enabled": true,
	})
}

func (b *WSBridge) handleOverlayAnswer(client *wsClient, req *WSRequest) {
	var params struct {
		QueryID string `json:"query_id"` // hex
		Data    string `json:"data"`     // base64
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

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Overlay.Timeout)
	defer cancel()

	if err := peer.Answer(ctx, queryIDBytes, RawMessage{Data: dataBytes}); err != nil {
		b.sendError(client, req.ID, "answer failed: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]interface{}{
		"answered": true,
	})
}
