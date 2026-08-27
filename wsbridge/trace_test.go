package wsbridge

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
)

type traceLookupAPI struct {
	ton.APIClientWrapped
	tx       *tlb.Transaction
	gotHash  []byte
	gotAfter time.Time
}

func (a *traceLookupAPI) FindLastTransactionByInMsgHashAfterTime(_ context.Context, _ *address.Address, hash []byte, after time.Time) (*tlb.Transaction, error) {
	a.gotHash = append([]byte(nil), hash...)
	a.gotAfter = after
	return a.tx, nil
}

func TestTraceUsesNativeFullMessageLookupWithoutTenTransactionCap(t *testing.T) {
	coins := tlb.FromNanoTONU(0)
	api := &traceLookupAPI{tx: &tlb.Transaction{
		Hash:      make([]byte, 32),
		TotalFees: tlb.CurrencyCollection{Coins: coins},
	}}
	bridge := testBridge()
	bridge.api = api
	conn, cleanup := dialTestBridge(t, bridge)
	defer cleanup()

	bridge.mu.RLock()
	var client *wsClient
	for registered := range bridge.clients {
		client = registered
	}
	bridge.mu.RUnlock()
	if client == nil {
		t.Fatal("websocket client not registered")
	}

	wantHash := make([]byte, 32)
	wantHash[0] = 0x91
	wantAfter := time.Unix(1_700_000_000, 0)
	found := bridge.scanForMatch(context.Background(), client, "trace", pendingMsg{
		destAddr: address.NewAddress(0, 0, make([]byte, 32)),
		msgHash:  wantHash,
		after:    wantAfter,
		depth:    1,
	}, &traceState{}, make(chan pendingMsg, 1))
	if !found {
		t.Fatal("native lookup result was not accepted")
	}
	if string(api.gotHash) != string(wantHash) || !api.gotAfter.Equal(wantAfter) {
		t.Fatalf("lookup args hash=%x after=%v", api.gotHash, api.gotAfter)
	}

	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	if event["event"] != "trace_tx" {
		t.Fatalf("unexpected event: %#v", event)
	}
}
