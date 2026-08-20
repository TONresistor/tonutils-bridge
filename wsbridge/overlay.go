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
const maxOverlayBroadcastWireSize = maxOverlayBroadcastSize + 4096
const maxOverlayQuerySize = 8*1024 - 128
const maxConcurrentOverlayBroadcasts = 4

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

func closeOverlay(overlayWrapper *overlay.ADNLOverlayWrapper) {
	if overlayWrapper == nil {
		return
	}
	overlayWrapper.Close()
	overlayWrapper.BroadcastReceiver.Close()
}

func bridgeBroadcastDisposition(info overlay.BroadcastInfo) overlay.BroadcastDisposition {
	if info.Trusted {
		return overlay.BroadcastDispositionAcceptAndRelay
	}
	return overlay.BroadcastDispositionIgnore
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

	b.peerLifecycleMu.Lock()
	client.peersMu.Lock()
	overlayCount := len(client.overlays)
	client.peersMu.Unlock()
	if overlayCount >= b.cfg.Namespaces.Overlay.MaxOverlays {
		b.peerLifecycleMu.Unlock()
		b.sendError(client, req.ID, fmt.Sprintf("max overlays limit reached (%d)", b.cfg.Namespaces.Overlay.MaxOverlays), -32602)
		return
	}

	peerHex := hex.EncodeToString(peerIDBytes)
	b.activePeersMu.RLock()
	_, ok := b.activePeers[peerHex]
	manager := b.peerWrappers[peerHex]
	b.activePeersMu.RUnlock()
	if !ok || manager == nil {
		b.peerLifecycleMu.Unlock()
		b.sendError(client, req.ID, "peer not found — connect first via adnl.connect", -32602)
		return
	}
	if !clientOwnsPeer(client, peerHex) {
		b.peerLifecycleMu.Unlock()
		b.sendError(client, req.ID, "peer not owned by this client", -32602)
		return
	}

	overlayHex := hex.EncodeToString(overlayID)

	// F3: Single Lock for both check and insert to avoid TOCTOU race
	b.activeOverlaysMu.Lock()
	if _, exists := b.activeOverlays[overlayHex]; exists {
		b.activeOverlaysMu.Unlock()
		if !clientOwnsOverlay(client, overlayHex) {
			b.peerLifecycleMu.Unlock()
			b.sendError(client, req.ID, "overlay is already joined by another client", -32602)
			return
		}
		b.overlayToPeerMu.Lock()
		existingPeerHex := b.overlayToPeer[overlayHex]
		b.overlayToPeerMu.Unlock()
		if existingPeerHex != peerHex {
			b.peerLifecycleMu.Unlock()
			b.sendError(client, req.ID, "overlay is already attached to another peer", -32602)
			return
		}
		b.peerLifecycleMu.Unlock()
		b.sendResult(client, req.ID, map[string]interface{}{
			"joined":     true,
			"overlay_id": params.OverlayID,
		})
		return
	}

	receiver, err := overlay.NewBroadcastReceiver(overlayID, maxOverlayBroadcastWireSize, true, false)
	if err != nil {
		b.activeOverlaysMu.Unlock()
		b.peerLifecycleMu.Unlock()
		b.sendError(client, req.ID, "failed to create overlay receiver: "+err.Error(), -32602)
		return
	}
	overlayWrapper, err := manager.AttachOverlay(receiver)
	if err != nil {
		receiver.Close()
		b.activeOverlaysMu.Unlock()
		b.peerLifecycleMu.Unlock()
		b.sendError(client, req.ID, "failed to attach overlay: "+err.Error())
		return
	}

	b.activeOverlays[overlayHex] = overlayWrapper
	b.activeOverlaysMu.Unlock()

	// Track overlay→peer mapping for disconnect cleanup
	b.overlayToPeerMu.Lock()
	b.overlayToPeer[overlayHex] = peerHex
	b.overlayToPeerMu.Unlock()

	// Set broadcast handler — pushes events to the owning client
	overlayWrapper.SetBroadcastHandlerWithInfo(func(msg tl.Serializable, info overlay.BroadcastInfo) overlay.BroadcastDisposition {
		var msgBytes []byte
		serialized, err := tl.Serialize(msg, true)
		if err != nil {
			log.Warn().Err(err).Msg("failed to serialize overlay broadcast")
			return overlay.BroadcastDispositionIgnore
		}
		msgBytes = serialized

		if !b.sendEvent(client, "overlay.broadcast", map[string]interface{}{
			"overlay_id": params.OverlayID,
			"message":    base64.StdEncoding.EncodeToString(msgBytes),
			"trusted":    info.Trusted,
		}) {
			return overlay.BroadcastDispositionIgnore
		}
		return bridgeBroadcastDisposition(info)
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
	b.peerLifecycleMu.Unlock()

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

	b.peerLifecycleMu.Lock()
	if !clientOwnsOverlay(client, overlayHex) {
		b.peerLifecycleMu.Unlock()
		b.sendError(client, req.ID, "overlay not owned by this client", -32602)
		return
	}

	b.activeOverlaysMu.Lock()
	ow, ok := b.activeOverlays[overlayHex]
	if ok {
		delete(b.activeOverlays, overlayHex)
	}
	b.activeOverlaysMu.Unlock()

	if !ok {
		b.peerLifecycleMu.Unlock()
		b.sendError(client, req.ID, "overlay not found", -32602)
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

	closeOverlay(ow)
	b.peerLifecycleMu.Unlock()

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
		ADNLID  string `json:"adnl_id"`
		Overlay string `json:"overlay"`
	}

	var peers []peerInfo
	for _, node := range nodes {
		id, ok := node.ID.(keys.PublicKeyED25519)
		if !ok {
			continue
		}
		adnlID, err := tl.Hash(node.ID)
		if err != nil {
			continue
		}
		peers = append(peers, peerInfo{
			ID:      base64.StdEncoding.EncodeToString(id.Key),
			ADNLID:  base64.StdEncoding.EncodeToString(adnlID),
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

	obj, err := parseBoxedTL(data)
	if err != nil {
		b.sendError(client, req.ID, "invalid boxed TL payload: "+err.Error(), -32602)
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
	select {
	case client.broadcastSem <- struct{}{}:
	default:
		b.sendError(client, req.ID, fmt.Sprintf("too many active overlay broadcasts (max %d)", maxConcurrentOverlayBroadcasts), -32603)
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
		<-client.broadcastSem
		b.sendError(client, req.ID, "broadcast init failed: "+err.Error())
		return
	}

	broadcaster, err := overlay.NewBroadcastFECBroadcaster(sender, overlay.StaticBroadcastPeerSet{ow})
	if err != nil {
		<-client.broadcastSem
		b.sendError(client, req.ID, "broadcaster init failed: "+err.Error())
		return
	}

	// Run() pumps repair symbols until neighbours acknowledge or the broadcast
	// TTL (~60s) elapses; it is blocking, so drive it in the background on the
	// connection-scoped context and return the broadcast id immediately.
	go func() {
		defer func() { <-client.broadcastSem }()
		broadcastCtx, cancel := context.WithTimeout(client.ctx, 65*time.Second)
		defer cancel()
		if err := broadcaster.Run(broadcastCtx); err != nil && broadcastCtx.Err() == nil {
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
		Raw       bool   `json:"raw"`        // preserve legacy ws.rawMessage transport
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
	if len(data) == 0 || len(data) > maxOverlayQuerySize {
		b.sendError(client, req.ID, fmt.Sprintf("query data must be between 1 and %d bytes", maxOverlayQuerySize), -32602)
		return
	}
	query, err := parseRPCPayload(data, params.Raw)
	if err != nil {
		b.sendError(client, req.ID, "invalid query payload: "+err.Error(), -32602)
		return
	}

	overlayHex := hex.EncodeToString(overlayID)
	b.peerLifecycleMu.Lock()
	b.activeOverlaysMu.RLock()
	ow, ok := b.activeOverlays[overlayHex]
	b.activeOverlaysMu.RUnlock()
	if !ok {
		b.peerLifecycleMu.Unlock()
		b.sendError(client, req.ID, "overlay not found — join first via overlay.join", -32602)
		return
	}

	if !clientOwnsOverlay(client, overlayHex) {
		b.peerLifecycleMu.Unlock()
		b.sendError(client, req.ID, "overlay not owned by this client", -32602)
		return
	}
	b.peerLifecycleMu.Unlock()

	if params.Timeout <= 0 {
		params.Timeout = int(b.cfg.Namespaces.Overlay.Timeout.Seconds())
	}
	if params.Timeout > int(b.cfg.Namespaces.Overlay.QueryMaxTimeout.Seconds()) {
		params.Timeout = int(b.cfg.Namespaces.Overlay.QueryMaxTimeout.Seconds())
	}

	ctx, cancel := context.WithTimeout(client.ctx, time.Duration(params.Timeout)*time.Second)
	defer cancel()

	var result any
	if err := ow.Query(ctx, query, &result); err != nil {
		b.sendError(client, req.ID, "query failed: "+err.Error())
		return
	}

	if result == nil {
		b.sendResult(client, req.ID, map[string]interface{}{
			"data": "",
		})
		return
	}

	resultBytes, err := serializeRPCPayload(result, params.Raw)
	if err != nil {
		b.sendError(client, req.ID, "query returned a non-serializable TL response: "+err.Error())
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
	b.peerLifecycleMu.Lock()
	b.activeOverlaysMu.RLock()
	ow, ok := b.activeOverlays[overlayHex]
	b.activeOverlaysMu.RUnlock()
	if !ok {
		b.peerLifecycleMu.Unlock()
		b.sendError(client, req.ID, "overlay not found — join first via overlay.join", -32602)
		return
	}

	if !clientOwnsOverlay(client, overlayHex) {
		b.peerLifecycleMu.Unlock()
		b.sendError(client, req.ID, "overlay not owned by this client", -32602)
		return
	}

	peerHex := hex.EncodeToString(peerIDBytes)
	b.activePeersMu.RLock()
	peer, ok := b.activePeers[peerHex]
	b.activePeersMu.RUnlock()
	if !ok {
		b.peerLifecycleMu.Unlock()
		b.sendError(client, req.ID, "peer not found — connect first via adnl.connect", -32602)
		return
	}
	if !clientOwnsPeer(client, peerHex) {
		b.peerLifecycleMu.Unlock()
		b.sendError(client, req.ID, "peer not owned by this client", -32602)
		return
	}
	b.overlayToPeerMu.Lock()
	overlayPeerHex := b.overlayToPeer[overlayHex]
	b.overlayToPeerMu.Unlock()
	if overlayPeerHex != peerHex {
		b.peerLifecycleMu.Unlock()
		b.sendError(client, req.ID, "overlay is not attached to the requested peer", -32602)
		return
	}

	ow.SetQueryHandler(func(msg *adnl.MessageQuery) error {
		queryID := hex.EncodeToString(msg.ID)

		var msgData []byte
		raw := false
		switch v := msg.Data.(type) {
		case RawMessage:
			msgData = v.Data
			raw = true
		default:
			serialized, err := tl.Serialize(v, true)
			if err != nil {
				return nil
			}
			msgData = serialized
		}

		b.peerLifecycleMu.Lock()
		b.activePeersMu.RLock()
		current := b.activePeers[peerHex]
		b.activePeersMu.RUnlock()
		if current != peer {
			b.peerLifecycleMu.Unlock()
			return nil
		}
		b.pendingQueriesMu.Lock()
		b.pendingQueries[queryID] = pendingQuery{peer: peer, owner: client, deadline: time.Now().Add(maxPendingQueryTTL)}
		b.pendingQueriesMu.Unlock()
		b.peerLifecycleMu.Unlock()

		b.sendEvent(client, "overlay.queryReceived", map[string]interface{}{
			"overlay_id": params.OverlayID,
			"query_id":   queryID,
			"data":       base64.StdEncoding.EncodeToString(msgData),
			"raw":        raw,
		})
		return nil
	})
	b.peerLifecycleMu.Unlock()

	b.sendResult(client, req.ID, map[string]interface{}{
		"enabled": true,
	})
}

func (b *WSBridge) handleOverlayAnswer(client *wsClient, req *WSRequest) {
	var params struct {
		QueryID string `json:"query_id"` // hex
		Data    string `json:"data"`     // base64
		Raw     bool   `json:"raw"`      // answer a legacy ws.rawMessage query
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
	if len(dataBytes) == 0 || len(dataBytes) > maxOverlayQuerySize {
		b.sendError(client, req.ID, fmt.Sprintf("answer data must be between 1 and %d bytes", maxOverlayQuerySize), -32602)
		return
	}

	queryIDBytes, err := hex.DecodeString(params.QueryID)
	if err != nil {
		b.sendError(client, req.ID, "invalid query_id hex: "+err.Error(), -32602)
		return
	}
	if len(queryIDBytes) != 32 {
		b.sendError(client, req.ID, "query_id must be 32 bytes", -32602)
		return
	}
	answer, err := parseRPCPayload(dataBytes, params.Raw)
	if err != nil {
		b.sendError(client, req.ID, "invalid answer payload: "+err.Error(), -32602)
		return
	}

	b.pendingQueriesMu.Lock()
	pq, ok := b.pendingQueries[params.QueryID]
	if !ok || time.Now().After(pq.deadline) {
		if ok {
			delete(b.pendingQueries, params.QueryID)
		}
		b.pendingQueriesMu.Unlock()
		b.sendError(client, req.ID, "query not found or expired", -32602)
		return
	}
	if pq.owner != nil && pq.owner != client {
		b.pendingQueriesMu.Unlock()
		b.sendError(client, req.ID, "query not owned by this client", -32602)
		return
	}
	b.pendingQueriesMu.Unlock()

	b.activePeersMu.RLock()
	current := b.activePeers[hex.EncodeToString(pq.peer.GetID())]
	b.activePeersMu.RUnlock()
	if current != pq.peer {
		b.sendError(client, req.ID, "peer disconnected before answer could be sent", -32602)
		return
	}

	peer := pq.peer

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Overlay.Timeout)
	defer cancel()

	if err := peer.Answer(ctx, queryIDBytes, answer); err != nil {
		b.sendError(client, req.ID, "answer failed: "+err.Error())
		return
	}
	b.pendingQueriesMu.Lock()
	if currentPending, exists := b.pendingQueries[params.QueryID]; exists && currentPending.peer == pq.peer && currentPending.owner == pq.owner && currentPending.deadline.Equal(pq.deadline) {
		delete(b.pendingQueries, params.QueryID)
	}
	b.pendingQueriesMu.Unlock()

	b.sendResult(client, req.ID, map[string]interface{}{
		"answered": true,
	})
}
