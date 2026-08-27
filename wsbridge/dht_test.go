package wsbridge

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/xssnick/tonutils-go/adnl/keys"
	"github.com/xssnick/tonutils-go/adnl/overlay"
	"github.com/xssnick/tonutils-go/tl"
)

func TestOverlayNodeToTunnelRelayUsesADNLHash(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	node := overlay.Node{
		ID:      keys.PublicKeyED25519{Key: pub},
		Version: 42,
	}

	relay, ok := overlayNodeToTunnelRelay(node)
	if !ok {
		t.Fatal("ed25519 overlay node must convert")
	}

	wantADNL, err := tl.Hash(keys.PublicKeyED25519{Key: pub})
	if err != nil {
		t.Fatal(err)
	}
	wantID := base64.StdEncoding.EncodeToString(pub)
	wantHash := base64.StdEncoding.EncodeToString(wantADNL)

	if relay.ID != wantID {
		t.Fatalf("id = %s, want pubkey %s", relay.ID, wantID)
	}
	if relay.ADNLID != wantHash {
		t.Fatalf("adnl_id = %s, want hash %s", relay.ADNLID, wantHash)
	}
	if relay.ID == relay.ADNLID {
		t.Fatal("id (pubkey) and adnl_id (hash) must differ")
	}
	if relay.Version != 42 {
		t.Fatalf("version = %d, want 42", relay.Version)
	}

	raw, err := json.Marshal(relay)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["id"] != wantID {
		t.Fatalf("json id = %v, want %s", m["id"], wantID)
	}
	if m["adnl_id"] != wantHash {
		t.Fatalf("json adnl_id = %v, want %s", m["adnl_id"], wantHash)
	}
}

func TestOverlayNodeToInfoIncludesADNLHash(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	overlayID := []byte("overlay-id")
	node := overlay.Node{
		ID:      keys.PublicKeyED25519{Key: pub},
		Overlay: overlayID,
		Version: 42,
	}

	info, ok := overlayNodeToInfo(node)
	if !ok {
		t.Fatal("ed25519 overlay node must convert")
	}

	wantADNL, err := tl.Hash(keys.PublicKeyED25519{Key: pub})
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != base64.StdEncoding.EncodeToString(pub) {
		t.Fatalf("id = %s, want raw public key", info.ID)
	}
	if info.ADNLID != base64.StdEncoding.EncodeToString(wantADNL) {
		t.Fatalf("adnl_id = %s, want ADNL hash", info.ADNLID)
	}
	if info.Overlay != base64.StdEncoding.EncodeToString(overlayID) {
		t.Fatalf("overlay = %s, want encoded overlay ID", info.Overlay)
	}
	if info.Version != 42 {
		t.Fatalf("version = %d, want 42", info.Version)
	}
}

func TestOverlayNodeToTunnelRelayRejectsNonEd25519(t *testing.T) {
	node := overlay.Node{
		ID:      keys.PublicKeyOverlay{Key: make([]byte, 32)},
		Version: 1,
	}
	if _, ok := overlayNodeToTunnelRelay(node); ok {
		t.Fatal("non-ed25519 node must be rejected")
	}
}
