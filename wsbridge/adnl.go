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
	"github.com/xssnick/tonutils-go/adnl/address"
	"github.com/xssnick/tonutils-go/adnl/keys"
	"github.com/xssnick/tonutils-go/adnl/overlay"
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

func publicADNLEndpoints(candidates []address.Address, max int) []string {
	if max <= 0 {
		return nil
	}
	endpoints := make([]string, 0, max)
	for _, candidate := range candidates {
		ip := address.IPValue(candidate)
		port := address.PortValue(candidate)
		if !isPublicUnicastIP(ip) || port < 1 || port > 65535 {
			continue
		}
		endpoints = append(endpoints, net.JoinHostPort(ip.String(), fmt.Sprintf("%d", port)))
		if len(endpoints) == max {
			break
		}
	}
	return endpoints
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

func addClientPeer(client *wsClient, peerHex string) {
	client.peersMu.Lock()
	defer client.peersMu.Unlock()
	for _, ownedPeer := range client.peers {
		if ownedPeer == peerHex {
			return
		}
	}
	client.peers = append(client.peers, peerHex)
}

// installPeer atomically installs one raw peer and one shared overlay manager
// for its ADNL identity. tonutils-go calls the gateway connection handler
// synchronously from RegisterClient, so this function must not acquire
// peerLifecycleMu; activePeersMu provides the compare-and-install boundary.
func (b *WSBridge) installPeer(peer adnl.Peer, owner *wsClient) error {
	peerHex := hex.EncodeToString(peer.GetID())

	b.activePeersMu.Lock()
	defer b.activePeersMu.Unlock()
	if current := b.activePeers[peerHex]; current != nil {
		if current != peer {
			return fmt.Errorf("ADNL peer replacement is waiting for cleanup")
		}
		currentOwner := b.peerOwners[peerHex]
		if owner != nil && currentOwner != nil && currentOwner != owner {
			return fmt.Errorf("ADNL peer is already connected by another client")
		}
		if owner != nil {
			b.peerOwners[peerHex] = owner
		}
		return nil
	}

	manager := overlay.CreateExtendedADNL(peer)
	manager.SetCustomMessageHandler(b.peerCustomHandler(peer))
	manager.SetQueryHandler(b.peerQueryHandler(peer))
	manager.SetDisconnectHandler(b.peerDisconnectHandler(peer))
	if err := peer.GetCloserCtx().Err(); err != nil {
		return fmt.Errorf("ADNL peer closed during registration: %w", err)
	}

	b.activePeers[peerHex] = peer
	b.peerWrappers[peerHex] = manager
	b.peerOwners[peerHex] = owner
	return nil
}

func (b *WSBridge) peerCustomHandler(peer adnl.Peer) func(msg *adnl.MessageCustom) error {
	peerID := base64.StdEncoding.EncodeToString(peer.GetID())
	peerHex := hex.EncodeToString(peer.GetID())
	return func(msg *adnl.MessageCustom) error {
		b.activePeersMu.RLock()
		current := b.activePeers[peerHex]
		owner := b.peerOwners[peerHex]
		b.activePeersMu.RUnlock()
		if current != peer {
			return nil
		}

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
	}
}

func (b *WSBridge) peerQueryHandler(peer adnl.Peer) func(msg *adnl.MessageQuery) error {
	peerHex := hex.EncodeToString(peer.GetID())
	return func(msg *adnl.MessageQuery) error {
		b.activePeersMu.RLock()
		current := b.activePeers[peerHex]
		handler := b.peerQueries[peerHex]
		b.activePeersMu.RUnlock()
		if current != peer || handler == nil {
			return nil
		}
		return handler(msg)
	}
}

func (b *WSBridge) peerDisconnectHandler(peer adnl.Peer) func(addr string, key ed25519.PublicKey) {
	peerID := base64.StdEncoding.EncodeToString(peer.GetID())
	peerHex := hex.EncodeToString(peer.GetID())
	return func(addr string, key ed25519.PublicKey) {
		b.peerLifecycleMu.Lock()
		owner, removed := b.detachPeerLocked(peerHex, peer)
		b.peerLifecycleMu.Unlock()
		if !removed {
			return
		}

		event := map[string]interface{}{"peer": peerID}
		if owner != nil {
			b.sendEvent(owner, "adnl.disconnected", event)
		} else {
			b.broadcastToClients("adnl.disconnected", event)
		}
	}
}

// detachPeerLocked synchronously removes all application state for one exact
// peer generation. The caller must hold peerLifecycleMu. activePeers is deleted
// last, so a gateway callback cannot install a replacement before cleanup ends.
func (b *WSBridge) detachPeerLocked(peerHex string, expected adnl.Peer) (*wsClient, bool) {
	b.activePeersMu.RLock()
	current, ok := b.activePeers[peerHex]
	owner := b.peerOwners[peerHex]
	b.activePeersMu.RUnlock()
	if !ok || current != expected {
		return nil, false
	}

	b.overlayToPeerMu.Lock()
	var overlayIDs []string
	for overlayHex, ownerPeerHex := range b.overlayToPeer {
		if ownerPeerHex == peerHex {
			overlayIDs = append(overlayIDs, overlayHex)
			delete(b.overlayToPeer, overlayHex)
		}
	}
	b.overlayToPeerMu.Unlock()

	var overlays []*overlay.ADNLOverlayWrapper
	b.activeOverlaysMu.Lock()
	for _, overlayHex := range overlayIDs {
		if ow, exists := b.activeOverlays[overlayHex]; exists {
			delete(b.activeOverlays, overlayHex)
			if ow != nil {
				overlays = append(overlays, ow)
			}
		}
	}
	b.activeOverlaysMu.Unlock()

	b.pendingQueriesMu.Lock()
	for queryID, pending := range b.pendingQueries {
		if pending.peer == expected {
			delete(b.pendingQueries, queryID)
		}
	}
	b.pendingQueriesMu.Unlock()

	if owner != nil {
		removedOverlays := make(map[string]struct{}, len(overlayIDs))
		for _, overlayHex := range overlayIDs {
			removedOverlays[overlayHex] = struct{}{}
		}
		owner.peersMu.Lock()
		peers := owner.peers[:0]
		for _, ownedPeer := range owner.peers {
			if ownedPeer != peerHex {
				peers = append(peers, ownedPeer)
			}
		}
		owner.peers = peers
		ownedOverlays := owner.overlays[:0]
		for _, ownedOverlay := range owner.overlays {
			if _, remove := removedOverlays[ownedOverlay]; !remove {
				ownedOverlays = append(ownedOverlays, ownedOverlay)
			}
		}
		owner.overlays = ownedOverlays
		owner.peersMu.Unlock()
	}

	for _, ow := range overlays {
		ow.Close()
	}

	b.activePeersMu.Lock()
	if current := b.activePeers[peerHex]; current != expected {
		b.activePeersMu.Unlock()
		return nil, false
	}
	delete(b.activePeers, peerHex)
	delete(b.peerWrappers, peerHex)
	delete(b.peerOwners, peerHex)
	delete(b.peerQueries, peerHex)
	b.activePeersMu.Unlock()
	return owner, true
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

	peerID, err := tl.Hash(keys.PublicKeyED25519{Key: keyBytes})
	if err != nil {
		b.sendError(client, req.ID, "invalid ADNL public key", -32602)
		return
	}
	peerHex := hex.EncodeToString(peerID)

	b.peerLifecycleMu.Lock()
	defer b.peerLifecycleMu.Unlock()

	if existing, existingOwner := func() (adnl.Peer, *wsClient) {
		b.activePeersMu.RLock()
		defer b.activePeersMu.RUnlock()
		return b.activePeers[peerHex], b.peerOwners[peerHex]
	}(); existing != nil {
		if existingOwner != nil && existingOwner != client {
			b.sendError(client, req.ID, "ADNL peer is already connected by another client", -32602)
			return
		}
		if existingOwner == nil {
			client.peersMu.Lock()
			peerCount := len(client.peers)
			client.peersMu.Unlock()
			if peerCount >= b.cfg.Namespaces.ADNL.MaxPeers {
				b.sendError(client, req.ID, fmt.Sprintf("max peers limit reached (%d)", b.cfg.Namespaces.ADNL.MaxPeers), -32602)
				return
			}
		}
		pingCtx, pingCancel := context.WithTimeout(client.ctx, 3*time.Second)
		_, pingErr := existing.Ping(pingCtx)
		pingCancel()
		if pingErr != nil {
			b.sendError(client, req.ID, "existing ADNL peer is not live; disconnect it before retrying")
			return
		}
		if existingOwner == nil {
			if err := b.installPeer(existing, client); err != nil {
				b.sendError(client, req.ID, "adnl connect ownership failed: "+err.Error())
				return
			}
			addClientPeer(client, peerHex)
		}
		b.sendResult(client, req.ID, map[string]interface{}{
			"connected":   true,
			"peer_id":     base64.StdEncoding.EncodeToString(existing.GetID()),
			"remote_addr": existing.RemoteAddr(),
		})
		return
	}

	client.peersMu.Lock()
	peerCount := len(client.peers)
	client.peersMu.Unlock()
	if peerCount >= b.cfg.Namespaces.ADNL.MaxPeers {
		b.sendError(client, req.ID, fmt.Sprintf("max peers limit reached (%d)", b.cfg.Namespaces.ADNL.MaxPeers), -32602)
		return
	}

	peer, err := b.gate.RegisterClient(params.Address, pubKey)
	if err != nil {
		b.sendError(client, req.ID, "adnl connect failed: "+err.Error())
		return
	}
	peerHex = hex.EncodeToString(peer.GetID())
	if err := b.installPeer(peer, client); err != nil {
		b.sendError(client, req.ID, "adnl connect ownership failed: "+err.Error())
		return
	}

	// Track peer for cleanup on WS disconnect
	addClientPeer(client, peerHex)

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

	adnlID, err := decodeBase64(params.ADNLID)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 adnl_id: "+err.Error(), -32602)
		return
	}
	if len(adnlID) != 32 {
		b.sendError(client, req.ID, "adnl_id must be 32 bytes", -32602)
		return
	}

	// tonutils-go caches one mutable peer per ADNL id. Serialize discovery
	// connects so parallel failover attempts cannot retarget or close the same
	// underlying peer.
	b.peerLifecycleMu.Lock()
	defer b.peerLifecycleMu.Unlock()

	peerHex := hex.EncodeToString(adnlID)
	b.activePeersMu.RLock()
	existing := b.activePeers[peerHex]
	existingOwner := b.peerOwners[peerHex]
	b.activePeersMu.RUnlock()
	if existing != nil {
		if existingOwner != nil && existingOwner != client {
			b.sendError(client, req.ID, "ADNL peer is already connected by another client", -32602)
			return
		}
		if existingOwner == nil {
			client.peersMu.Lock()
			peerCount := len(client.peers)
			client.peersMu.Unlock()
			if peerCount >= b.cfg.Namespaces.ADNL.MaxPeers {
				b.sendError(client, req.ID, fmt.Sprintf("max peers limit reached (%d)", b.cfg.Namespaces.ADNL.MaxPeers), -32602)
				return
			}
		}
		pingCtx, pingCancel := context.WithTimeout(client.ctx, 3*time.Second)
		_, pingErr := existing.Ping(pingCtx)
		pingCancel()
		if pingErr != nil {
			b.sendError(client, req.ID, "existing ADNL peer is not live; disconnect it before retrying")
			return
		}
		if existingOwner == nil {
			if err := b.installPeer(existing, client); err != nil {
				b.sendError(client, req.ID, "adnl connect ownership failed: "+err.Error())
				return
			}
			addClientPeer(client, peerHex)
		}
		b.sendResult(client, req.ID, map[string]interface{}{
			"connected":   true,
			"peer_id":     base64.StdEncoding.EncodeToString(existing.GetID()),
			"remote_addr": existing.RemoteAddr(),
		})
		return
	}

	client.peersMu.Lock()
	peerCount := len(client.peers)
	client.peersMu.Unlock()
	if peerCount >= b.cfg.Namespaces.ADNL.MaxPeers {
		b.sendError(client, req.ID, fmt.Sprintf("max peers limit reached (%d)", b.cfg.Namespaces.ADNL.MaxPeers), -32602)
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

	var peer adnl.Peer
	for _, candidateAddr := range publicADNLEndpoints(addrs.Addresses, 8) {
		// DHT records are untrusted. This discovery path is public-network only
		// even if direct adnl.connect SSRF protection was explicitly disabled.
		candidatePeer, connectErr := b.gate.RegisterClient(candidateAddr, pubKey)
		if connectErr != nil {
			continue
		}
		pingCtx, pingCancel := context.WithTimeout(ctx, 3*time.Second)
		_, pingErr := candidatePeer.Ping(pingCtx)
		pingCancel()
		if pingErr != nil {
			candidateHex := hex.EncodeToString(candidatePeer.GetID())
			b.detachPeerLocked(candidateHex, candidatePeer)
			candidatePeer.Close()
			continue
		}
		peer = candidatePeer
		break
	}
	if peer == nil {
		b.sendError(client, req.ID, "no live public ADNL address found")
		return
	}

	peerHex = hex.EncodeToString(peer.GetID())
	if err := b.installPeer(peer, client); err != nil {
		b.detachPeerLocked(peerHex, peer)
		peer.Close()
		b.sendError(client, req.ID, "adnl connect ownership failed: "+err.Error())
		return
	}

	// Track peer for cleanup on WS disconnect
	addClientPeer(client, peerHex)

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
	b.peerLifecycleMu.Lock()
	defer b.peerLifecycleMu.Unlock()

	if !clientOwnsPeer(client, peerHex) {
		b.sendError(client, req.ID, "peer not owned by this client", -32602)
		return
	}

	b.activePeersMu.RLock()
	peer, ok := b.activePeers[peerHex]
	b.activePeersMu.RUnlock()
	if !ok {
		b.sendError(client, req.ID, "peer not found", -32602)
		return
	}

	if _, removed := b.detachPeerLocked(peerHex, peer); !removed {
		b.sendError(client, req.ID, "peer changed during disconnect", -32602)
		return
	}
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
		PeerID  string `json:"peer_id"` // base64
		Data    string `json:"data"`    // base64 TL-serialized request
		Timeout int    `json:"timeout"` // optional, seconds (default 15)
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
	b.peerLifecycleMu.Lock()
	b.activePeersMu.RLock()
	peer, ok := b.activePeers[peerHex]
	b.activePeersMu.RUnlock()
	if !ok {
		b.peerLifecycleMu.Unlock()
		b.sendError(client, req.ID, "peer not found", -32602)
		return
	}

	if !clientOwnsPeer(client, peerHex) {
		b.peerLifecycleMu.Unlock()
		b.sendError(client, req.ID, "peer not owned by this client", -32602)
		return
	}

	base64PeerID := base64.StdEncoding.EncodeToString(peer.GetID())

	handler := func(msg *adnl.MessageQuery) error {
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

		b.peerLifecycleMu.Lock()
		b.activePeersMu.RLock()
		current := b.activePeers[peerHex]
		b.activePeersMu.RUnlock()
		if current != peer {
			b.peerLifecycleMu.Unlock()
			return nil
		}
		b.pendingQueriesMu.Lock()
		b.pendingQueries[queryID] = pendingQuery{peer: peer, deadline: time.Now().Add(maxPendingQueryTTL)}
		b.pendingQueriesMu.Unlock()
		b.peerLifecycleMu.Unlock()

		b.sendEvent(client, "adnl.queryReceived", map[string]interface{}{
			"peer_id":  base64PeerID,
			"query_id": queryID,
			"data":     base64.StdEncoding.EncodeToString(msgData),
		})
		return nil
	}
	b.activePeersMu.Lock()
	if b.activePeers[peerHex] != peer {
		b.activePeersMu.Unlock()
		b.peerLifecycleMu.Unlock()
		b.sendError(client, req.ID, "peer changed while enabling queries", -32602)
		return
	}
	b.peerQueries[peerHex] = handler
	b.activePeersMu.Unlock()
	b.peerLifecycleMu.Unlock()

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
	current := b.activePeers[hex.EncodeToString(pq.peer.GetID())]
	b.activePeersMu.RUnlock()
	if current != pq.peer {
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
