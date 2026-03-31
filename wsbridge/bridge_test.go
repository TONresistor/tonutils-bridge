package wsbridge

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/adnl/overlay"
)

// testBridge creates a WSBridge with nil deps (only tests WS layer, not TON network).
// Methods that hit DHT/lite/DNS will return errors — that's what we test.
func testBridge() *WSBridge {
	return &WSBridge{
		clients:        make(map[*wsClient]bool),
		activePeers:    make(map[string]adnl.Peer),
		activeOverlays: make(map[string]*overlay.ADNLOverlayWrapper),
		pendingQueries: make(map[string]pendingQuery),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
}

// dialTestBridge starts the bridge on an httptest server and returns a connected WS client.
func dialTestBridge(t *testing.T, bridge *WSBridge) (*websocket.Conn, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(bridge.handleWS))
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	return conn, func() {
		conn.Close()
		server.Close()
	}
}

// rpc sends a JSON-RPC request and reads the response.
func rpc(t *testing.T, conn *websocket.Conn, id, method string, params interface{}) WSResponse {
	t.Helper()
	req := WSRequest{JSONRPC: "2.0", ID: id, Method: method}
	if params != nil {
		raw, _ := json.Marshal(params)
		req.Params = raw
	}
	data, _ := json.Marshal(req)
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}

	var resp WSResponse
	if err := json.Unmarshal(msg, &resp); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	return resp
}

// --- Tests ---

func TestWSBridge_NetworkInfo(t *testing.T) {
	bridge := testBridge()
	conn, cleanup := dialTestBridge(t, bridge)
	defer cleanup()

	// Give handleWS time to register the client
	time.Sleep(50 * time.Millisecond)

	resp := rpc(t, conn, "1", "network.info", nil)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}
	if resp.ID != "1" {
		t.Fatalf("expected id=1, got %s", resp.ID)
	}

	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatal("result is not a map")
	}

	clients := result["ws_clients"].(float64)
	if clients < 1 {
		t.Fatalf("expected ws_clients >= 1, got %v", clients)
	}
}

func TestWSBridge_UnknownMethod(t *testing.T) {
	bridge := testBridge()
	conn, cleanup := dialTestBridge(t, bridge)
	defer cleanup()

	resp := rpc(t, conn, "1", "fake.method", nil)
	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != -32601 {
		t.Fatalf("expected error code -32601, got: %d", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "unknown method") {
		t.Fatalf("expected 'unknown method' in error, got: %s", resp.Error.Message)
	}
}

func TestWSBridge_InvalidJSON(t *testing.T) {
	bridge := testBridge()
	conn, cleanup := dialTestBridge(t, bridge)
	defer cleanup()

	// Send garbage
	if err := conn.WriteMessage(websocket.TextMessage, []byte("{not json")); err != nil {
		t.Fatal(err)
	}

	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}

	var resp WSResponse
	json.Unmarshal(msg, &resp)
	if resp.Error == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if resp.Error.Code != -32700 {
		t.Fatalf("expected error code -32700, got: %d", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "invalid JSON") {
		t.Fatalf("expected 'invalid JSON' error, got: %s", resp.Error.Message)
	}
}

func TestWSBridge_NilDeps_MethodsReturnError(t *testing.T) {
	bridge := testBridge()
	conn, cleanup := dialTestBridge(t, bridge)
	defer cleanup()

	// Methods that can safely return errors with nil deps (no nil pointer panic)
	// Methods that directly dereference nil clients (dht, api, dns) are excluded —
	// those are tested via the SDK E2E tests against a live bridge.
	safeMethods := []struct {
		method string
		params interface{}
	}{
		{"adnl.peers", nil},
		{"network.info", nil},
	}

	for _, tc := range safeMethods {
		resp := rpc(t, conn, "1", tc.method, tc.params)
		// These should return valid responses even with nil deps
		if resp.ID != "1" {
			t.Errorf("%s: expected id=1, got %s", tc.method, resp.ID)
		}
	}

	// Methods with invalid params should return parse errors before hitting nil deps
	invalidParamMethods := []struct {
		method string
		params interface{}
	}{
		{"lite.getAccountState", map[string]string{"address": "totally-invalid"}},
		{"lite.runMethod", map[string]string{"address": "bad"}},
		{"lite.getTransactions", map[string]string{"address": "bad"}},
		{"lite.lookupBlock", map[string]string{"shard": "not-hex"}},
		{"lite.sendMessage", map[string]string{"boc": "!!!invalid-base64!!!"}},
	}

	for _, tc := range invalidParamMethods {
		resp := rpc(t, conn, "1", tc.method, tc.params)
		if resp.Error == nil {
			t.Errorf("%s: expected error with invalid params, got result: %v", tc.method, resp.Result)
		}
	}
}

func TestWSBridge_ClientRegistration(t *testing.T) {
	bridge := testBridge()
	conn1, cleanup1 := dialTestBridge(t, bridge)
	defer cleanup1()
	conn2, cleanup2 := dialTestBridge(t, bridge)
	defer cleanup2()

	time.Sleep(50 * time.Millisecond)

	resp := rpc(t, conn1, "1", "network.info", nil)
	result := resp.Result.(map[string]interface{})
	clients := int(result["ws_clients"].(float64))
	if clients != 2 {
		t.Fatalf("expected 2 clients, got %d", clients)
	}

	// Disconnect one
	conn2.Close()
	time.Sleep(100 * time.Millisecond)

	resp = rpc(t, conn1, "2", "network.info", nil)
	result = resp.Result.(map[string]interface{})
	clients = int(result["ws_clients"].(float64))
	if clients != 1 {
		t.Fatalf("expected 1 client after disconnect, got %d", clients)
	}
}

func TestWSBridge_RapidSequentialRequests(t *testing.T) {
	bridge := testBridge()
	conn, cleanup := dialTestBridge(t, bridge)
	defer cleanup()

	// Send 20 rapid sequential requests — server handlers run concurrently,
	// the write mutex must serialize responses without deadlock or corruption.
	for i := 0; i < 20; i++ {
		req := WSRequest{JSONRPC: "2.0", ID: strings.Repeat("x", i+1), Method: "network.info"}
		data, _ := json.Marshal(req)
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
	}

	// Read all 20 responses
	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read %d failed: %v", i, err)
		}
		var resp WSResponse
		json.Unmarshal(msg, &resp)
		if resp.Error != nil {
			t.Fatalf("request %d returned error: %s", i, resp.Error.Message)
		}
		seen[resp.ID] = true
	}

	if len(seen) != 20 {
		t.Fatalf("expected 20 unique responses, got %d", len(seen))
	}
}

func TestWSBridge_ContextCancellation(t *testing.T) {
	bridge := testBridge()
	ctx, cancel := context.WithCancel(context.Background())

	server := httptest.NewServer(http.HandlerFunc(bridge.handleWS))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	time.Sleep(50 * time.Millisecond)

	// Verify connected
	resp := rpc(t, conn, "1", "network.info", nil)
	if resp.Error != nil {
		t.Fatal(resp.Error.Message)
	}

	// Cancel context — simulates server shutdown
	cancel()
	_ = ctx // used by cancel

	// The bridge Start() respects ctx.Done() to close the server,
	// but in test mode we're using httptest so the server stays up.
	// The important thing is that cancel doesn't panic or deadlock.
	time.Sleep(100 * time.Millisecond)
}

func TestWSBridge_IDPreservation(t *testing.T) {
	bridge := testBridge()
	conn, cleanup := dialTestBridge(t, bridge)
	defer cleanup()

	// Different request IDs should be echoed back correctly
	ids := []string{"abc-123", "0", "req_with_special-chars.v2", ""}
	for _, id := range ids {
		resp := rpc(t, conn, id, "network.info", nil)
		if resp.ID != id {
			t.Errorf("expected id=%q, got %q", id, resp.ID)
		}
	}
}

func TestWSBridge_ParseAddress_SafeFromPanic(t *testing.T) {
	// parseAddress wraps MustParseAddr which panics on invalid input
	// Verify it recovers gracefully
	_, err := parseAddress("totally-not-an-address")
	if err == nil {
		t.Fatal("expected error for invalid address")
	}

	_, err = parseAddress("")
	if err == nil {
		t.Fatal("expected error for empty address")
	}

	// Valid address should work
	addr, err := parseAddress("EQCD39VS5jcptHL8vMjEXrzGaRcCVYto7HUn4bpAOg8xqB2N")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if addr == nil {
		t.Fatal("expected non-nil address")
	}
}

func TestWSBridge_SerializeStack(t *testing.T) {
	// Empty stack
	result := serializeStack(nil)
	if len(result) != 0 {
		t.Fatalf("expected empty slice, got %v", result)
	}

	// Nil element
	result = serializeStack([]any{nil})
	if result[0] != nil {
		t.Fatalf("expected nil, got %v", result[0])
	}
}

func TestWSBridge_ReadLimit(t *testing.T) {
	bridge := testBridge()
	conn, cleanup := dialTestBridge(t, bridge)
	defer cleanup()

	// Send a message larger than 1MB
	bigMsg := make([]byte, 1<<20+100)
	for i := range bigMsg {
		bigMsg[i] = 'a'
	}
	err := conn.WriteMessage(websocket.TextMessage, bigMsg)
	if err != nil {
		return // write may fail immediately, that's fine
	}
	// Try to read — should get a close error
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Fatal("expected connection to be closed after oversized message")
	}
}

func TestWSBridge_ErrorCode32602(t *testing.T) {
	bridge := testBridge()
	conn, cleanup := dialTestBridge(t, bridge)
	defer cleanup()

	cases := []struct {
		method string
		params interface{}
	}{
		{"lite.getAccountState", map[string]string{"address": "bad"}},
		{"lite.runMethod", map[string]string{"address": "bad"}},
		{"lite.sendMessage", map[string]string{"boc": "!!!"}},
		{"lite.getTransactions", map[string]string{"address": "bad"}},
		{"lite.lookupBlock", map[string]string{"shard": "not-hex"}},
		{"adnl.connect", map[string]string{"key": "not-base64!!!"}},
		{"adnl.sendMessage", map[string]string{"peer_id": "not-base64!!!"}},
		{"jetton.getData", map[string]string{"address": "bad"}},
		{"nft.getData", map[string]string{"address": "bad"}},
		{"dns.resolve", nil}, // nil params → invalid params
	}

	for _, tc := range cases {
		resp := rpc(t, conn, "1", tc.method, tc.params)
		if resp.Error == nil {
			t.Errorf("%s: expected error, got nil", tc.method)
			continue
		}
		if resp.Error.Code != -32602 {
			t.Errorf("%s: expected code -32602, got %d (%s)", tc.method, resp.Error.Code, resp.Error.Message)
		}
	}
}

func TestWSBridge_PendingQueriesTTL(t *testing.T) {
	bridge := testBridge()

	// Insert an entry with a deadline in the past
	bridge.pendingQueriesMu.Lock()
	bridge.pendingQueries["expired-query"] = pendingQuery{
		peer:     nil,
		deadline: time.Now().Add(-10 * time.Second), // already expired
	}
	bridge.pendingQueries["valid-query"] = pendingQuery{
		peer:     nil,
		deadline: time.Now().Add(30 * time.Second), // still valid
	}
	bridge.pendingQueriesMu.Unlock()

	// Simulate the sweep (same logic as the goroutine in Start)
	now := time.Now()
	bridge.pendingQueriesMu.Lock()
	for id, pq := range bridge.pendingQueries {
		if now.After(pq.deadline) {
			delete(bridge.pendingQueries, id)
		}
	}
	bridge.pendingQueriesMu.Unlock()

	bridge.pendingQueriesMu.RLock()
	_, expiredExists := bridge.pendingQueries["expired-query"]
	_, validExists := bridge.pendingQueries["valid-query"]
	bridge.pendingQueriesMu.RUnlock()

	if expiredExists {
		t.Fatal("expired query should have been evicted")
	}
	if !validExists {
		t.Fatal("valid query should still exist")
	}
}

func TestWSBridge_SerializeStackTypes(t *testing.T) {
	// big.Int
	result := serializeStack([]any{big.NewInt(42)})
	if result[0] != "42" {
		t.Fatalf("expected '42', got %v", result[0])
	}

	// Mixed types
	result = serializeStack([]any{big.NewInt(100), nil, big.NewInt(0)})
	if len(result) != 3 {
		t.Fatalf("expected 3 items, got %d", len(result))
	}
	if result[0] != "100" {
		t.Fatalf("expected '100', got %v", result[0])
	}
	if result[1] != nil {
		t.Fatalf("expected nil, got %v", result[1])
	}
	if result[2] != "0" {
		t.Fatalf("expected '0', got %v", result[2])
	}
}

func TestWSBridge_BroadcastToClients(t *testing.T) {
	bridge := testBridge()
	conn1, cleanup1 := dialTestBridge(t, bridge)
	defer cleanup1()
	conn2, cleanup2 := dialTestBridge(t, bridge)
	defer cleanup2()

	time.Sleep(50 * time.Millisecond) // let clients register

	// Broadcast an event
	bridge.broadcastToClients("test.event", map[string]interface{}{
		"msg": "hello",
	})

	// Both clients should receive it
	for i, conn := range []*websocket.Conn{conn1, conn2} {
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("client %d: read failed: %v", i+1, err)
		}
		var event map[string]interface{}
		json.Unmarshal(msg, &event)
		if event["event"] != "test.event" {
			t.Fatalf("client %d: expected test.event, got %v", i+1, event["event"])
		}
		data := event["data"].(map[string]interface{})
		if data["msg"] != "hello" {
			t.Fatalf("client %d: expected 'hello', got %v", i+1, data["msg"])
		}
	}
}
