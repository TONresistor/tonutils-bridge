package wsbridge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"math/big"
	"testing"
	"time"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/adnl/keys"
	"github.com/xssnick/tonutils-go/adnl/overlay"
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"github.com/xssnick/tonutils-go/tvm/tuple"
)

type protocolBroadcastPeer struct{ id []byte }

func (p protocolBroadcastPeer) ID() []byte { return p.id }
func (p protocolBroadcastPeer) SendCustomMessage(context.Context, tl.Serializable) error {
	return nil
}

func TestDecodeDNSTextMultiChunk(t *testing.T) {
	chunks := [][]byte{[]byte("hello "), []byte("from "), []byte("TON DNS")}
	var next *cell.Cell
	for i := len(chunks) - 1; i >= 1; i-- {
		builder := cell.BeginCell().MustStoreUInt(uint64(len(chunks[i])), 8).MustStoreSlice(chunks[i], uint(len(chunks[i])*8))
		if next != nil {
			builder.MustStoreRef(next)
		}
		next = builder.EndCell()
	}
	top := cell.BeginCell().MustStoreUInt(0x1eda, 16).
		MustStoreUInt(uint64(len(chunks)), 8).
		MustStoreUInt(uint64(len(chunks[0])), 8).
		MustStoreSlice(chunks[0], uint(len(chunks[0])*8))
	top.MustStoreRef(next)

	slice := top.EndCell().MustBeginParse()
	if tag := slice.MustLoadUInt(16); tag != 0x1eda {
		t.Fatalf("unexpected tag: %x", tag)
	}
	got, err := decodeDNSText(slice)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello from TON DNS" {
		t.Fatalf("decoded text = %q", got)
	}
}

func TestParseBoxedTLRejectsTrailingBytes(t *testing.T) {
	raw, err := tl.Serialize(TonnetGetTime{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseBoxedTL(append(raw, 0)); err == nil {
		t.Fatal("boxed TL with trailing bytes must be rejected")
	}
	obj, err := parseBoxedTL(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := obj.(TonnetGetTime); !ok {
		t.Fatalf("parsed %T, want TonnetGetTime", obj)
	}
}

func TestParseTonnetSessionChallenge(t *testing.T) {
	raw := []byte{0x29, 0x3e, 0x72, 0xb3}
	obj, err := parseBoxedTL(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := obj.(TonnetGetSessionChallenge); !ok {
		t.Fatalf("parsed %T, want TonnetGetSessionChallenge", obj)
	}
}

func TestRawRPCPayloadRoundTrip(t *testing.T) {
	want := []byte{1, 2, 3}
	payload, err := parseRPCPayload(want, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := payload.(RawMessage)
	if !ok {
		t.Fatalf("parsed %T, want RawMessage", payload)
	}
	got, err := serializeRPCPayload(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("raw payload = %x, want %x", got, want)
	}
}

func TestAddressStringOrNil(t *testing.T) {
	if got := addressStringOrNil(address.NewAddressNone()); got != nil {
		t.Fatalf("NONE address encoded as %v", got)
	}
	addr := address.NewAddress(0, 0, bytes.Repeat([]byte{1}, 32))
	if got := addressStringOrNil(addr); got != addr.String() {
		t.Fatalf("address encoded as %v", got)
	}
}

func TestBridgeBroadcastReceiverAcceptsNonEmptyAnySender(t *testing.T) {
	overlayID := bytes.Repeat([]byte{0x44}, 32)
	receiver, err := overlay.NewBroadcastReceiver(overlayID, maxOverlayBroadcastWireSize, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := tl.Serialize(RawMessage{Data: []byte("hello")}, true)
	if err != nil {
		t.Fatal(err)
	}
	broadcast := overlay.Broadcast{
		Source:      keys.PublicKeyED25519{Key: pub},
		Certificate: overlay.CertificateEmpty{},
		Flags:       overlay.BroadcastFlagAnySender,
		Data:        payload,
		Date:        int32(time.Now().Unix()),
	}
	if err := broadcast.Sign(priv); err != nil {
		t.Fatal(err)
	}

	delivered := false
	receiver.SetBroadcastHandlerWithInfo(func(tl.Serializable, overlay.BroadcastInfo) overlay.BroadcastDisposition {
		delivered = true
		return overlay.BroadcastDispositionIgnore
	})
	if err := receiver.HandleMessage(protocolBroadcastPeer{id: bytes.Repeat([]byte{0x55}, 32)}, broadcast); err != nil {
		t.Fatalf("broadcast rejected: %v", err)
	}
	if !delivered {
		t.Fatal("broadcast handler was not called")
	}
}

func TestBridgeOnlyRelaysTrustedBroadcasts(t *testing.T) {
	if got := bridgeBroadcastDisposition(overlay.BroadcastInfo{Trusted: false}); got != overlay.BroadcastDispositionIgnore {
		t.Fatalf("untrusted disposition = %v", got)
	}
	if got := bridgeBroadcastDisposition(overlay.BroadcastInfo{Trusted: true}); got != overlay.BroadcastDispositionAcceptAndRelay {
		t.Fatalf("trusted disposition = %v", got)
	}
}

func TestRunMethodTupleConversionIsRecursive(t *testing.T) {
	converted, err := convertRunMethodParam([]any{"123", nil, []any{float64(7)}})
	if err != nil {
		t.Fatal(err)
	}
	items := converted.([]any)
	if items[0].(*big.Int).String() != "123" || items[1] != nil || items[2].([]any)[0].(*big.Int).Int64() != 7 {
		t.Fatalf("unexpected converted tuple: %#v", items)
	}
	if _, err := convertRunMethodParam(1.5); err == nil {
		t.Fatal("fractional JSON number must be rejected")
	}

	nested := tuple.NewTupleValue(big.NewInt(9), []any{big.NewInt(10)})
	serialized := serializeStack([]any{nested})
	want := serialized[0].([]any)
	if want[0] != "9" || want[1].([]any)[0] != "10" {
		t.Fatalf("unexpected serialized tuple: %#v", serialized)
	}
}
