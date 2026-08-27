package wsbridge

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/adnl/address"
	"github.com/xssnick/tonutils-go/adnl/overlay"
	"github.com/xssnick/tonutils-go/tl"
)

type lifecycleTestPeer struct {
	id  []byte
	key ed25519.PublicKey

	mu                sync.Mutex
	customHandler     func(msg *adnl.MessageCustom) error
	queryHandler      func(msg *adnl.MessageQuery) error
	disconnectHandler func(addr string, key ed25519.PublicKey)

	ctx            context.Context
	cancel         context.CancelFunc
	closeOnce      sync.Once
	disconnectGate chan struct{}
	disconnectDone chan struct{}
}

func newLifecycleTestPeer(id []byte, delayDisconnect bool) *lifecycleTestPeer {
	ctx, cancel := context.WithCancel(context.Background())
	peer := &lifecycleTestPeer{
		id:             append([]byte(nil), id...),
		key:            make(ed25519.PublicKey, ed25519.PublicKeySize),
		ctx:            ctx,
		cancel:         cancel,
		disconnectDone: make(chan struct{}),
	}
	if delayDisconnect {
		peer.disconnectGate = make(chan struct{})
	}
	return peer
}

func (p *lifecycleTestPeer) SetCustomMessageHandler(handler func(msg *adnl.MessageCustom) error) {
	p.mu.Lock()
	p.customHandler = handler
	p.mu.Unlock()
}

func (p *lifecycleTestPeer) SetQueryHandler(handler func(msg *adnl.MessageQuery) error) {
	p.mu.Lock()
	p.queryHandler = handler
	p.mu.Unlock()
}

func (p *lifecycleTestPeer) GetDisconnectHandler() func(addr string, key ed25519.PublicKey) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.disconnectHandler
}

func (p *lifecycleTestPeer) SetDisconnectHandler(handler func(addr string, key ed25519.PublicKey)) {
	p.mu.Lock()
	p.disconnectHandler = handler
	p.mu.Unlock()
}

func (p *lifecycleTestPeer) SendCustomMessage(context.Context, tl.Serializable) error { return nil }
func (p *lifecycleTestPeer) SendNop(context.Context) error                            { return nil }
func (p *lifecycleTestPeer) Query(context.Context, tl.Serializable, tl.Serializable) error {
	return nil
}
func (p *lifecycleTestPeer) Answer(context.Context, []byte, tl.Serializable) error { return nil }
func (p *lifecycleTestPeer) Ping(context.Context) (time.Duration, error)           { return 0, nil }

func (p *lifecycleTestPeer) GetQueryHandler() func(msg *adnl.MessageQuery) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.queryHandler
}

func (p *lifecycleTestPeer) GetCloserCtx() context.Context { return p.ctx }
func (p *lifecycleTestPeer) SetAddresses(address.List)     {}
func (p *lifecycleTestPeer) RemoteAddr() string            { return "127.0.0.1:1" }
func (p *lifecycleTestPeer) GetID() []byte                 { return p.id }
func (p *lifecycleTestPeer) GetPubKey() ed25519.PublicKey  { return p.key }
func (p *lifecycleTestPeer) Stats() adnl.PeerStats         { return adnl.PeerStats{} }
func (p *lifecycleTestPeer) Reinit()                       {}

func (p *lifecycleTestPeer) Close() {
	p.closeOnce.Do(func() {
		p.cancel()
		handler := p.GetDisconnectHandler()
		go func() {
			defer close(p.disconnectDone)
			if p.disconnectGate != nil {
				<-p.disconnectGate
			}
			if handler != nil {
				handler(p.RemoteAddr(), p.key)
			}
		}()
	})
}

func closeLifecycleTestPeer(t *testing.T, peer *lifecycleTestPeer) {
	t.Helper()
	peer.Close()
	select {
	case <-peer.disconnectDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for test peer disconnect callback")
	}
}

func attachLifecycleTestOverlay(t *testing.T, manager *overlay.ADNLWrapper, overlayID []byte) *overlay.ADNLOverlayWrapper {
	t.Helper()
	receiver, err := overlay.NewBroadcastReceiver(overlayID, maxOverlayBroadcastWireSize, true, false)
	if err != nil {
		t.Fatal(err)
	}
	ow, err := manager.AttachOverlay(receiver)
	if err != nil {
		receiver.Close()
		t.Fatal(err)
	}
	return ow
}

func TestInstallPeerRejectsLiveReplacement(t *testing.T) {
	bridge := testBridge()
	peerID := make([]byte, 32)
	peerID[0] = 0x11
	first := newLifecycleTestPeer(peerID, false)
	replacement := newLifecycleTestPeer(peerID, false)
	owner := &wsClient{}

	if err := bridge.installPeer(first, nil); err != nil {
		t.Fatalf("install first peer: %v", err)
	}
	if err := bridge.installPeer(first, owner); err != nil {
		t.Fatalf("claim unowned live peer: %v", err)
	}
	bridge.activePeersMu.RLock()
	installedOwner := bridge.peerOwners[hex.EncodeToString(peerID)]
	bridge.activePeersMu.RUnlock()
	if installedOwner != owner {
		t.Fatal("claiming an unowned peer did not record its websocket owner")
	}
	if err := bridge.installPeer(replacement, nil); err == nil {
		t.Fatal("live peer replacement must be rejected until exact cleanup completes")
	}

	peerHex := hex.EncodeToString(peerID)
	bridge.peerLifecycleMu.Lock()
	if _, removed := bridge.detachPeerLocked(peerHex, first); !removed {
		bridge.peerLifecycleMu.Unlock()
		t.Fatal("first peer cleanup did not remove the exact generation")
	}
	bridge.peerLifecycleMu.Unlock()
	closeLifecycleTestPeer(t, first)

	if err := bridge.installPeer(replacement, nil); err != nil {
		t.Fatalf("install replacement after cleanup: %v", err)
	}
	bridge.peerLifecycleMu.Lock()
	bridge.detachPeerLocked(peerHex, replacement)
	bridge.peerLifecycleMu.Unlock()
	closeLifecycleTestPeer(t, replacement)
}

func TestStaleDisconnectCannotRemoveReplacement(t *testing.T) {
	bridge := testBridge()
	peerID := make([]byte, 32)
	peerID[0] = 0x21
	peerHex := hex.EncodeToString(peerID)
	overlayID := make([]byte, 32)
	overlayID[0] = 0x31
	overlayHex := hex.EncodeToString(overlayID)

	oldOwner := &wsClient{}
	oldPeer := newLifecycleTestPeer(peerID, true)
	if err := bridge.installPeer(oldPeer, oldOwner); err != nil {
		t.Fatalf("install old peer: %v", err)
	}
	oldOwner.peers = append(oldOwner.peers, peerHex)
	bridge.activePeersMu.RLock()
	oldManager := bridge.peerWrappers[peerHex]
	bridge.activePeersMu.RUnlock()
	oldOverlay := attachLifecycleTestOverlay(t, oldManager, overlayID)
	bridge.activeOverlays[overlayHex] = oldOverlay
	bridge.overlayToPeer[overlayHex] = peerHex
	oldOwner.overlays = append(oldOwner.overlays, overlayHex)

	bridge.peerLifecycleMu.Lock()
	if _, removed := bridge.detachPeerLocked(peerHex, oldPeer); !removed {
		bridge.peerLifecycleMu.Unlock()
		t.Fatal("old peer cleanup did not run")
	}
	oldPeer.Close()
	bridge.peerLifecycleMu.Unlock()

	replacementOwner := &wsClient{}
	replacement := newLifecycleTestPeer(peerID, false)
	if err := bridge.installPeer(replacement, replacementOwner); err != nil {
		t.Fatalf("install replacement: %v", err)
	}
	replacementOwner.peers = append(replacementOwner.peers, peerHex)
	bridge.activePeersMu.RLock()
	replacementManager := bridge.peerWrappers[peerHex]
	bridge.activePeersMu.RUnlock()
	replacementOverlay := attachLifecycleTestOverlay(t, replacementManager, overlayID)
	bridge.activeOverlays[overlayHex] = replacementOverlay
	bridge.overlayToPeer[overlayHex] = peerHex
	replacementOwner.overlays = append(replacementOwner.overlays, overlayHex)

	close(oldPeer.disconnectGate)
	select {
	case <-oldPeer.disconnectDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for delayed disconnect callback")
	}

	bridge.activePeersMu.RLock()
	currentPeer := bridge.activePeers[peerHex]
	currentManager := bridge.peerWrappers[peerHex]
	currentOwner := bridge.peerOwners[peerHex]
	bridge.activePeersMu.RUnlock()
	if currentPeer != replacement || currentManager != replacementManager || currentOwner != replacementOwner {
		t.Fatal("stale disconnect callback removed or corrupted the replacement peer")
	}

	bridge.peerLifecycleMu.Lock()
	bridge.detachPeerLocked(peerHex, replacement)
	bridge.peerLifecycleMu.Unlock()
	closeLifecycleTestPeer(t, replacement)
}

func TestUnauthorizedLifecycleCallsDoNotMutateOwnerState(t *testing.T) {
	bridge := testBridge()
	ownerConn, ownerCleanup := dialTestBridge(t, bridge)
	defer ownerCleanup()
	time.Sleep(10 * time.Millisecond)

	bridge.mu.RLock()
	var owner *wsClient
	for client := range bridge.clients {
		owner = client
	}
	bridge.mu.RUnlock()
	if owner == nil {
		t.Fatal("owner websocket was not registered")
	}

	attackerConn, attackerCleanup := dialTestBridge(t, bridge)
	defer attackerCleanup()
	time.Sleep(10 * time.Millisecond)

	peerID := make([]byte, 32)
	peerID[0] = 0x71
	peerHex := hex.EncodeToString(peerID)
	peer := newLifecycleTestPeer(peerID, false)
	if err := bridge.installPeer(peer, owner); err != nil {
		t.Fatal(err)
	}
	addClientPeer(owner, peerHex)

	resp := rpc(t, attackerConn, "disconnect", "adnl.disconnect", map[string]string{
		"peer_id": base64.StdEncoding.EncodeToString(peerID),
	})
	if resp.Error == nil || resp.Error.Message != "peer not owned by this client" {
		t.Fatalf("unexpected disconnect response: %#v", resp.Error)
	}
	bridge.activePeersMu.RLock()
	stillInstalled := bridge.activePeers[peerHex] == peer
	bridge.activePeersMu.RUnlock()
	if !stillInstalled {
		t.Fatal("unauthorized disconnect removed the owner's peer")
	}
	joinOverlayID := make([]byte, 32)
	joinOverlayID[0] = 0x73
	resp = rpc(t, attackerConn, "join", "overlay.join", map[string]string{
		"overlay_id": base64.StdEncoding.EncodeToString(joinOverlayID),
		"peer_id":    base64.StdEncoding.EncodeToString(peerID),
	})
	if resp.Error == nil || resp.Error.Message != "peer not owned by this client" {
		t.Fatalf("unexpected join response: %#v", resp.Error)
	}
	bridge.activeOverlaysMu.RLock()
	_, attackerOverlayCreated := bridge.activeOverlays[hex.EncodeToString(joinOverlayID)]
	bridge.activeOverlaysMu.RUnlock()
	if attackerOverlayCreated {
		t.Fatal("unauthorized join created an overlay on the owner's peer")
	}
	answer, err := tl.Serialize(TonnetTime{Now: 1}, true)
	if err != nil {
		t.Fatal(err)
	}
	queryID := hex.EncodeToString(make([]byte, 32))
	bridge.pendingQueriesMu.Lock()
	bridge.pendingQueries[queryID] = pendingQuery{peer: peer, owner: owner, deadline: time.Now().Add(time.Minute)}
	bridge.pendingQueriesMu.Unlock()
	resp = rpc(t, attackerConn, "answer", "adnl.answer", map[string]string{
		"query_id": queryID,
		"data":     base64.StdEncoding.EncodeToString(answer),
	})
	if resp.Error == nil || resp.Error.Message != "query not owned by this client" {
		t.Fatalf("unexpected answer response: %#v", resp.Error)
	}
	bridge.pendingQueriesMu.RLock()
	_, queryStillPending := bridge.pendingQueries[queryID]
	bridge.pendingQueriesMu.RUnlock()
	if !queryStillPending {
		t.Fatal("unauthorized answer consumed the owner's pending query")
	}

	overlayID := make([]byte, 32)
	overlayID[0] = 0x72
	overlayHex := hex.EncodeToString(overlayID)
	bridge.activeOverlaysMu.Lock()
	bridge.activeOverlays[overlayHex] = nil
	bridge.activeOverlaysMu.Unlock()
	owner.peersMu.Lock()
	owner.overlays = append(owner.overlays, overlayHex)
	owner.peersMu.Unlock()

	resp = rpc(t, attackerConn, "leave", "overlay.leave", map[string]string{
		"overlay_id": base64.StdEncoding.EncodeToString(overlayID),
	})
	if resp.Error == nil || resp.Error.Message != "overlay not owned by this client" {
		t.Fatalf("unexpected leave response: %#v", resp.Error)
	}
	bridge.activeOverlaysMu.RLock()
	_, overlayStillInstalled := bridge.activeOverlays[overlayHex]
	bridge.activeOverlaysMu.RUnlock()
	if !overlayStillInstalled {
		t.Fatal("unauthorized leave removed the owner's overlay")
	}

	bridge.activeOverlaysMu.Lock()
	delete(bridge.activeOverlays, overlayHex)
	bridge.activeOverlaysMu.Unlock()
	owner.peersMu.Lock()
	owner.overlays = nil
	owner.peersMu.Unlock()
	bridge.peerLifecycleMu.Lock()
	bridge.detachPeerLocked(peerHex, peer)
	bridge.peerLifecycleMu.Unlock()
	closeLifecycleTestPeer(t, peer)
	_ = ownerConn
}
