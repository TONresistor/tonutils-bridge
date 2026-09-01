package wsbridge

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/adnl/address"
	"github.com/xssnick/tonutils-go/tl"
)

// lifecycleTestPeer models tonutils-go's asynchronous disconnect callback:
// Close cancels the peer synchronously, then invokes the registered handler in
// a separate goroutine. disconnectGate lets tests delay that callback until a
// replacement with the same ADNL ID has been installed.
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

func (p *lifecycleTestPeer) SendCustomMessage(context.Context, tl.Serializable) error {
	return nil
}

func (p *lifecycleTestPeer) SendNop(context.Context) error { return nil }

func (p *lifecycleTestPeer) Query(context.Context, tl.Serializable, tl.Serializable) error {
	return nil
}

func (p *lifecycleTestPeer) Answer(context.Context, []byte, tl.Serializable) error {
	return nil
}

func (p *lifecycleTestPeer) Ping(context.Context) (time.Duration, error) {
	return 0, nil
}

func (p *lifecycleTestPeer) GetQueryHandler() func(msg *adnl.MessageQuery) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.queryHandler
}

func (p *lifecycleTestPeer) GetCloserCtx() context.Context {
	return p.ctx
}

func (p *lifecycleTestPeer) SetAddresses(address.List) {}

func (p *lifecycleTestPeer) RemoteAddr() string {
	return "127.0.0.1:1"
}

func (p *lifecycleTestPeer) GetID() []byte {
	return p.id
}

func (p *lifecycleTestPeer) GetPubKey() ed25519.PublicKey {
	return p.key
}

func (p *lifecycleTestPeer) Stats() adnl.PeerStats { return adnl.PeerStats{} }

func (p *lifecycleTestPeer) Reinit() {}

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

func TestPeerCleanupPrecedesAsyncDisconnectCallback(t *testing.T) {
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
	oldOverlay := oldManager.WithOverlay(overlayID)
	bridge.activeOverlays[overlayHex] = oldOverlay
	bridge.overlayToPeer[overlayHex] = peerHex
	oldOwner.overlays = append(oldOwner.overlays, overlayHex)
	bridge.pendingQueries["old-query"] = pendingQuery{
		peer:     oldPeer,
		deadline: time.Now().Add(time.Minute),
	}

	// This is the production order: remove all bridge state synchronously,
	// then close ADNL while still holding the lifecycle boundary.
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
	replacementOverlay := replacementManager.WithOverlay(overlayID)
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

	bridge.activeOverlaysMu.RLock()
	currentOverlay := bridge.activeOverlays[overlayHex]
	bridge.activeOverlaysMu.RUnlock()
	if currentOverlay != replacementOverlay {
		t.Fatal("stale disconnect callback removed the replacement overlay")
	}

	bridge.overlayToPeerMu.Lock()
	currentOverlayPeer := bridge.overlayToPeer[overlayHex]
	bridge.overlayToPeerMu.Unlock()
	if currentOverlayPeer != peerHex {
		t.Fatal("stale disconnect callback removed the replacement overlay mapping")
	}

	bridge.pendingQueriesMu.RLock()
	_, oldQueryPresent := bridge.pendingQueries["old-query"]
	bridge.pendingQueriesMu.RUnlock()
	if oldQueryPresent {
		t.Fatal("old peer pending query survived synchronous cleanup")
	}
	if len(oldOwner.peers) != 0 || len(oldOwner.overlays) != 0 {
		t.Fatal("old owner retained peer or overlay ownership after cleanup")
	}

	bridge.peerLifecycleMu.Lock()
	bridge.detachPeerLocked(peerHex, replacement)
	bridge.peerLifecycleMu.Unlock()
	closeLifecycleTestPeer(t, replacement)
}
