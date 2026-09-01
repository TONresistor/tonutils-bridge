package wsbridge

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"testing"
	"time"

	"github.com/xssnick/tonutils-go/adnl/address"
	"github.com/xssnick/tonutils-go/adnl/keys"
	"github.com/xssnick/tonutils-go/adnl/overlay"
	"github.com/xssnick/tonutils-go/tl"
)

func TestOverlayDiscoveryRejectsPrivateAddresses(t *testing.T) {
	for _, raw := range []string{
		"0.0.0.1",
		"10.0.0.1",
		"100.64.0.1",
		"127.0.0.1",
		"169.254.1.1",
		"192.0.2.1",
		"192.88.99.2",
		"198.51.100.1",
		"203.0.113.1",
		"240.0.0.1",
		"::1",
		"100::1",
		"100:0:0:1::1",
		"2001:db8::1",
		"2002::1",
		"3fff::1",
		"5f00::1",
		"fd00::1",
		"ff02::1",
	} {
		if !isPrivateIP(net.ParseIP(raw)) {
			t.Errorf("isPrivateIP(%q) = false", raw)
		}
		if isPublicUnicastIP(net.ParseIP(raw)) {
			t.Errorf("isPublicUnicastIP(%q) = true", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "2606:4700:4700::1111"} {
		if isPrivateIP(net.ParseIP(raw)) || !isPublicUnicastIP(net.ParseIP(raw)) {
			t.Fatalf("public address %q was rejected", raw)
		}
	}
}

func TestPublicADNLEndpointsFiltersBeforeLimit(t *testing.T) {
	var candidates []address.Address
	for i := 1; i <= 9; i++ {
		candidates = append(candidates, &address.UDP{IP: net.IPv4(10, 0, 0, byte(i)), Port: 30303})
	}
	candidates = append(candidates,
		&address.UDP{IP: net.ParseIP("1.1.1.1"), Port: 30303},
		&address.UDP6{IP: net.ParseIP("2606:4700:4700::1111"), Port: 40404},
	)

	got := publicADNLEndpoints(candidates, 8)
	want := []string{"1.1.1.1:30303", "[2606:4700:4700::1111]:40404"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("endpoint %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestPublicADNLEndpointsRejectsInvalidPortsAndCapsPublicAttempts(t *testing.T) {
	candidates := []address.Address{
		&address.UDP{IP: net.ParseIP("1.1.1.1"), Port: 0},
		&address.UDP{IP: net.ParseIP("1.0.0.1"), Port: -1},
		&address.UDP6{IP: net.ParseIP("2606:4700:4700::1111"), Port: 65536},
	}
	for i := 1; i <= 10; i++ {
		candidates = append(candidates, &address.UDP{IP: net.IPv4(8, 8, 8, byte(i)), Port: int32(30000 + i)})
	}

	got := publicADNLEndpoints(candidates, 8)
	if len(got) != 8 {
		t.Fatalf("got %d endpoints, want 8: %v", len(got), got)
	}
	if got[0] != "8.8.8.1:30001" || got[7] != "8.8.8.8:30008" {
		t.Fatalf("unexpected source order or cap: %v", got)
	}
}

func signedOverlayNode(t *testing.T, room []byte, at time.Time) overlay.Node {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	overlayID, err := tl.Hash(keys.PublicKeyOverlay{Key: room})
	if err != nil {
		t.Fatal(err)
	}
	node := overlay.Node{
		ID:      keys.PublicKeyED25519{Key: private.Public().(ed25519.PublicKey)},
		Overlay: overlayID,
		Version: int32(at.Unix()),
	}
	if err := node.Sign(private); err != nil {
		t.Fatal(err)
	}
	return node
}

func TestValidOverlayNode(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	room := []byte("tonnet:test")
	node := signedOverlayNode(t, room, now)
	if !validOverlayNode(node, room, now) {
		t.Fatal("fresh signed node for the derived overlay must pass")
	}

	stale := signedOverlayNode(t, room, now.Add(-overlayNodeMaxAge-time.Second))
	if validOverlayNode(stale, room, now) {
		t.Fatal("stale node must fail")
	}

	wrongRoom := signedOverlayNode(t, []byte("tonnet:other"), now)
	if bytes.Equal(wrongRoom.Overlay, node.Overlay) || validOverlayNode(wrongRoom, room, now) {
		t.Fatal("node from another overlay must fail")
	}
}
