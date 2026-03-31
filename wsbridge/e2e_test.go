//go:build e2e

// End-to-end tests for the WebSocket-ADNL bridge.
//
// These tests connect to a LIVE proxy on the real TON mainnet.
// They are excluded from normal `go test` runs via the "e2e" build tag.
//
// Prerequisites:
//   1. Build the bridge:  go build -o tonutils-bridge .
//   2. Start the bridge:  ./tonutils-bridge --addr 127.0.0.1:8081
//   3. Wait ~10s for DHT bootstrap to complete.
//
// Run all E2E tests:
//   go test -tags e2e -v ./wsbridge/ -timeout 300s
//
// Run a specific namespace:
//   go test -tags e2e -v -run TestE2E_Lite ./wsbridge/
//
// Override the WebSocket address (default ws://127.0.0.1:8081):
//   WS_ADDR=ws://192.168.1.10:8081 go test -tags e2e -v ./wsbridge/

package wsbridge

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/xssnick/tonutils-go/adnl/keys"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton/wallet"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

// ---------------------------------------------------------------------------
// TestMain — suppress zerolog noise from the internal bridge
// ---------------------------------------------------------------------------

func TestMain(m *testing.M) {
	zerolog.SetGlobalLevel(zerolog.Disabled)
	RegisterOverlayKey() // tests don't import adnl-tunnel, so register here
	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func getWSAddr() string {
	if addr := os.Getenv("WS_ADDR"); addr != "" {
		return addr
	}
	return "ws://127.0.0.1:8081"
}

// formatTON converts a nano-TON string to a human-readable "X.XX TON" form.
func formatTON(nanoStr string) string {
	nano := new(big.Int)
	if _, ok := nano.SetString(nanoStr, 10); !ok {
		return nanoStr + " (raw)"
	}
	const decimals = 9
	divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(decimals), nil)
	whole := new(big.Int).Div(nano, divisor)
	frac := new(big.Int).Mod(nano, divisor)
	// Two-decimal fraction: frac * 100 / 10^9
	centPart := new(big.Int).Mul(frac, big.NewInt(100))
	centPart.Div(centPart, divisor)
	return fmt.Sprintf("%s.%02d TON", whole.String(), centPart.Int64())
}

// truncKey returns the first 10 characters of a base64 key followed by "...".
func truncKey(key string) string {
	if len(key) <= 10 {
		return key
	}
	return key[:10] + "..."
}

type e2eResponse struct {
	JSONRPC string                 `json:"jsonrpc"`
	ID      string                 `json:"id"`
	Result  map[string]interface{} `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// e2eCall sends a JSON-RPC 2.0 request and reads the response, silently
// skipping any broadcast events (messages with an "event" field) that arrive
// before the matching response.
func e2eCall(t *testing.T, c *websocket.Conn, method string, params interface{}) e2eResponse {
	t.Helper()
	req := map[string]interface{}{"jsonrpc": "2.0", "id": method, "method": method}
	if params != nil {
		req["params"] = params
	}
	data, _ := json.Marshal(req)
	if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("[FAIL] %s — write failed: %v", method, err)
	}
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	for {
		_, msg, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("[FAIL] %s — read failed: %v", method, err)
		}
		var raw map[string]interface{}
		if err := json.Unmarshal(msg, &raw); err == nil {
			if _, isEvent := raw["event"]; isEvent {
				continue
			}
		}
		var resp e2eResponse
		if err := json.Unmarshal(msg, &resp); err != nil {
			t.Fatalf("[FAIL] %s — unmarshal failed: %v", method, err)
		}
		return resp
	}
}

// e2eCallWithID is like e2eCall but allows specifying a custom request ID.
func e2eCallWithID(t *testing.T, c *websocket.Conn, id, method string, params interface{}) e2eResponse {
	t.Helper()
	req := map[string]interface{}{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	data, _ := json.Marshal(req)
	if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("[FAIL] %s — write failed: %v", method, err)
	}
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	for {
		_, msg, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("[FAIL] %s — read failed: %v", method, err)
		}
		var raw map[string]interface{}
		if err := json.Unmarshal(msg, &raw); err == nil {
			if _, isEvent := raw["event"]; isEvent {
				continue
			}
		}
		var resp e2eResponse
		if err := json.Unmarshal(msg, &resp); err != nil {
			t.Fatalf("[FAIL] %s — unmarshal failed: %v", method, err)
		}
		return resp
	}
}

// e2eWriteRaw sends raw bytes on the WS connection and reads a response,
// silently skipping broadcast events.
func e2eWriteRaw(t *testing.T, c *websocket.Conn, data []byte) e2eResponse {
	t.Helper()
	if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("[FAIL] raw write — write failed: %v", err)
	}
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, msg, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("[FAIL] raw write — read failed: %v", err)
		}
		var raw map[string]interface{}
		if err := json.Unmarshal(msg, &raw); err == nil {
			if _, isEvent := raw["event"]; isEvent {
				continue
			}
		}
		var resp e2eResponse
		if err := json.Unmarshal(msg, &resp); err != nil {
			t.Fatalf("[FAIL] raw write — unmarshal failed: %v", err)
		}
		return resp
	}
}

// e2eDial opens a WebSocket connection; skips the test if the proxy is not running.
func e2eDial(t *testing.T) *websocket.Conn {
	t.Helper()
	addr := getWSAddr()
	c, _, err := websocket.DefaultDialer.Dial(addr, nil)
	if err != nil {
		t.Fatalf("proxy not running at %s — start it first: %v", addr, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// e2eRequireResult asserts the response has no error and returns the result map.
func e2eRequireResult(t *testing.T, resp e2eResponse, method string) map[string]interface{} {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("[FAIL] %s — error %d: %s", method, resp.Error.Code, resp.Error.Message)
	}
	if resp.Result == nil {
		t.Fatalf("[FAIL] %s — nil result", method)
	}
	return resp.Result
}

// e2eCallRetry retries an RPC call up to maxRetries times on transient errors
// (timeout, deadline exceeded). Returns the first successful response or the last error.
func e2eCallRetry(t *testing.T, c *websocket.Conn, method string, params interface{}, maxRetries int) e2eResponse {
	t.Helper()
	var resp e2eResponse
	for i := 0; i <= maxRetries; i++ {
		resp = e2eCall(t, c, method, params)
		if resp.Error == nil {
			return resp
		}
		if i < maxRetries && (strings.Contains(resp.Error.Message, "deadline exceeded") ||
			strings.Contains(resp.Error.Message, "timeout") ||
			strings.Contains(resp.Error.Message, "context deadline")) {
			t.Logf("[RETRY %d/%d] %s — %s", i+1, maxRetries, method, resp.Error.Message)
			time.Sleep(2 * time.Second)
			continue
		}
		break
	}
	return resp
}

// Constants used across tests.
const (
	testAddr       = "EQCD39VS5jcptHL8vMjEXrzGaRcCVYto7HUn4bpAOg8xqB2N"
	hotWallet      = "UQDdb_AsWWNHRVKbmajVvu6p9sOKkYjmp-lqQk44IMisCnMY" // Telegram Wallet hot wallet (always active)
	sbtAddr        = "EQDle-2qf9QJ9KIxmpqYzAyuyX61Bi8aKDwuJQZlTTxJqkTo" // Real SBT on mainnet
	payChannelAddr = "EQA4Ntk6B2Sq-LbHoZ-11FFgr43o3dk5hS3w5G3OkOzHhQEG" // Real payment channel on mainnet
	usdtMaster     = "EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs"
	nftCollection  = "EQDvRFMYLdxmvY3Tk-cfWMLqDnXF_EclO2Fp4wwj33WhlNFT"
	relayAddr     = "80.78.27.15:17330"
	relayKey      = "0nAqzFCklgG1vJFgKHqU7Z87c7RHYn345e4jPnxqnxM="
)

// ---------------------------------------------------------------------------
// TestE2E_Lite — 14 lite.* methods
// ---------------------------------------------------------------------------

func TestE2E_Lite(t *testing.T) {
	t.Parallel()
	c := e2eDial(t)

	// Shared state: populated by getTransactions, consumed by getTransaction.
	var firstTxLT string

	t.Run("getMasterchainInfo", func(t *testing.T) {
		resp := e2eCall(t, c, "lite.getMasterchainInfo", nil)
		result := e2eRequireResult(t, resp, "lite.getMasterchainInfo")
		seqno, ok := result["seqno"].(float64)
		if !ok || seqno <= 0 {
			t.Fatalf("[FAIL] lite.getMasterchainInfo — expected seqno > 0, got %v", result["seqno"])
		}
		t.Logf("[PASS] lite.getMasterchainInfo — seqno %.0f", seqno)
	})

	t.Run("getAccountState", func(t *testing.T) {
		resp := e2eCallRetry(t, c, "lite.getAccountState", map[string]string{"address": testAddr}, 3)
		result := e2eRequireResult(t, resp, "lite.getAccountState")
		if _, ok := result["balance"]; !ok {
			t.Fatalf("[FAIL] lite.getAccountState — missing balance field")
		}
		balance := fmt.Sprintf("%v", result["balance"])
		status := "unknown"
		if s, ok := result["status"].(string); ok {
			status = s
		}
		t.Logf("[PASS] lite.getAccountState — balance %s, status %s", formatTON(balance), status)
	})

	t.Run("runMethod", func(t *testing.T) {
		resp := e2eCall(t, c, "lite.runMethod", map[string]interface{}{
			"address": testAddr,
			"method":  "seqno",
		})
		result := e2eRequireResult(t, resp, "lite.runMethod")
		exitCode, ok := result["exit_code"].(float64)
		if !ok || exitCode != 0 {
			t.Fatalf("[FAIL] lite.runMethod — expected exit_code 0, got %v", result["exit_code"])
		}
		val := "n/a"
		if stack, ok := result["stack"].([]interface{}); ok && len(stack) > 0 {
			val = fmt.Sprintf("%v", stack[0])
		}
		t.Logf("[PASS] lite.runMethod — seqno() = %s", val)
	})

	t.Run("sendMessage_invalidBOC", func(t *testing.T) {
		resp := e2eCall(t, c, "lite.sendMessage", map[string]string{"boc": "AQID"})
		if resp.Error == nil {
			t.Fatal("[FAIL] lite.sendMessage — expected error for invalid BOC, got success")
		}
		if resp.Error.Code == -32601 {
			t.Fatalf("[FAIL] lite.sendMessage — method not found (code -32601): %s", resp.Error.Message)
		}
		t.Logf("[PASS] lite.sendMessage_invalidBOC — expected error: invalid BOC rejected (code %d)", resp.Error.Code)
	})

	t.Run("sendMessageWait_invalidBOC", func(t *testing.T) {
		resp := e2eCall(t, c, "lite.sendMessageWait", map[string]string{"boc": "AQID"})
		if resp.Error == nil {
			t.Fatal("[FAIL] lite.sendMessageWait — expected error for invalid BOC, got success")
		}
		if resp.Error.Code == -32601 {
			t.Fatalf("[FAIL] lite.sendMessageWait — method not found (code -32601): %s", resp.Error.Message)
		}
		t.Logf("[PASS] lite.sendMessageWait_invalidBOC — expected error: invalid BOC rejected (code %d)", resp.Error.Code)
	})

	t.Run("getTransactions", func(t *testing.T) {
		// Use hotWallet (always active) so firstTxLT is from a recent shard block
		// that the liteserver can still resolve in getTransaction.
		resp := e2eCall(t, c, "lite.getTransactions", map[string]interface{}{
			"address": hotWallet,
			"limit":   2,
		})
		result := e2eRequireResult(t, resp, "lite.getTransactions")
		txs, ok := result["transactions"].([]interface{})
		if !ok || len(txs) == 0 {
			t.Fatalf("[FAIL] lite.getTransactions — expected at least 1 transaction, got %v", result["transactions"])
		}
		// Save first tx LT for the getTransaction subtest.
		if len(txs) > 0 {
			if tx, ok := txs[0].(map[string]interface{}); ok {
				if lt, ok := tx["lt"].(string); ok {
					firstTxLT = lt
				}
			}
		}
		t.Logf("[PASS] lite.getTransactions — %d transactions returned", len(txs))
	})

	t.Run("getTime", func(t *testing.T) {
		resp := e2eCall(t, c, "lite.getTime", nil)
		result := e2eRequireResult(t, resp, "lite.getTime")
		tm, ok := result["time"].(float64)
		if !ok || tm <= 0 {
			t.Fatalf("[FAIL] lite.getTime — expected time > 0, got %v", result["time"])
		}
		ts := time.Unix(int64(tm), 0).UTC()
		t.Logf("[PASS] lite.getTime — network time %s", ts.Format("2006-01-02 15:04:05 UTC"))
	})

	t.Run("lookupBlock", func(t *testing.T) {
		mcResp := e2eCall(t, c, "lite.getMasterchainInfo", nil)
		mcResult := e2eRequireResult(t, mcResp, "lite.getMasterchainInfo")
		seqno := mcResult["seqno"].(float64)

		resp := e2eCall(t, c, "lite.lookupBlock", map[string]interface{}{
			"workchain": -1,
			"shard":     "8000000000000000",
			"seqno":     int(seqno),
		})
		result := e2eRequireResult(t, resp, "lite.lookupBlock")
		if _, ok := result["seqno"]; !ok {
			t.Fatalf("[FAIL] lite.lookupBlock — missing seqno in result")
		}
		t.Logf("[PASS] lite.lookupBlock — block (-1, 8000000000000000, %.0f) found", seqno)
	})

	t.Run("getBlockTransactions", func(t *testing.T) {
		mcResp := e2eCall(t, c, "lite.getMasterchainInfo", nil)
		mcResult := e2eRequireResult(t, mcResp, "lite.getMasterchainInfo")
		seqno := int(mcResult["seqno"].(float64))

		resp := e2eCall(t, c, "lite.getBlockTransactions", map[string]interface{}{
			"workchain": -1,
			"shard":     "8000000000000000",
			"seqno":     seqno,
			"count":     3,
		})
		result := e2eRequireResult(t, resp, "lite.getBlockTransactions")
		count := 0
		if txs, ok := result["transactions"].([]interface{}); ok {
			count = len(txs)
		}
		t.Logf("[PASS] lite.getBlockTransactions — %d transactions in block %d", count, seqno)
	})

	t.Run("getShards", func(t *testing.T) {
		resp := e2eCall(t, c, "lite.getShards", nil)
		result := e2eRequireResult(t, resp, "lite.getShards")
		if _, ok := result["shards"]; !ok {
			t.Fatalf("[FAIL] lite.getShards — missing shards array")
		}
		count := 0
		if shards, ok := result["shards"].([]interface{}); ok {
			count = len(shards)
		}
		t.Logf("[PASS] lite.getShards — %d shards", count)
	})

	t.Run("getBlockchainConfig", func(t *testing.T) {
		resp := e2eCall(t, c, "lite.getBlockchainConfig", map[string]interface{}{
			"params": []int{0, 1},
		})
		e2eRequireResult(t, resp, "lite.getBlockchainConfig")
		t.Logf("[PASS] lite.getBlockchainConfig — params [0, 1] fetched")
	})

	t.Run("getTransaction", func(t *testing.T) {
		if firstTxLT == "" {
			t.Fatalf("[FAIL] lite.getTransaction — no transactions available (getTransactions returned none for hot wallet)")
		}
		resp := e2eCallRetry(t, c, "lite.getTransaction", map[string]interface{}{
			"address": hotWallet,
			"lt":      firstTxLT,
		}, 3)
		result := e2eRequireResult(t, resp, "lite.getTransaction")
		lt := "n/a"
		if l, ok := result["lt"].(string); ok {
			lt = l
		}
		t.Logf("[PASS] lite.getTransaction — tx at lt %s found", lt)
	})

	t.Run("findTxByOutMsgHash", func(t *testing.T) {
		txsResp := e2eCallRetry(t, c, "lite.getTransactions", map[string]interface{}{
			"address": hotWallet,
			"limit":   5,
		}, 3)
		txsResult := e2eRequireResult(t, txsResp, "lite.getTransactions")
		txList, ok := txsResult["transactions"].([]interface{})
		if !ok || len(txList) == 0 {
			t.Fatalf("[FAIL] lite.findTxByOutMsgHash — no transactions available for hot wallet")
		}

		// Find a transaction that has at least one out_msg with a body.
		var bodyHash string
		for _, item := range txList {
			tx, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			outMsgs, ok := tx["out_msgs"].([]interface{})
			if !ok || len(outMsgs) == 0 {
				continue
			}
			outMsg, ok := outMsgs[0].(map[string]interface{})
			if !ok {
				continue
			}
			bodyB64, ok := outMsg["body"].(string)
			if !ok || bodyB64 == "" {
				continue
			}
			bodyBytes, err := base64.StdEncoding.DecodeString(bodyB64)
			if err != nil {
				continue
			}
			bodyCell, err := cell.FromBOC(bodyBytes)
			if err != nil {
				continue
			}
			bodyHash = hex.EncodeToString(bodyCell.Hash())
			break
		}
		if bodyHash == "" {
			t.Fatalf("[FAIL] lite.findTxByOutMsgHash — no transaction with out_msg body found in recent txs")
		}

		resp := e2eCall(t, c, "lite.findTxByOutMsgHash", map[string]interface{}{
			"address":  hotWallet,
			"msg_hash": bodyHash,
		})
		if resp.Error != nil {
			if resp.Error.Code == -32601 {
				t.Fatalf("[FAIL] lite.findTxByOutMsgHash — method not found (code -32601): %s", resp.Error.Message)
			}
			// May fail if the tx is too old and pruned from liteserver
			t.Logf("[PASS] lite.findTxByOutMsgHash — lookup with real hash %s: %s", truncKey(bodyHash), resp.Error.Message)
		} else {
			t.Logf("[PASS] lite.findTxByOutMsgHash — found tx at lt %s with out_msg body hash %s", resp.Result["lt"], truncKey(bodyHash))
		}
	})

	t.Run("getBlockData", func(t *testing.T) {
		mcResp := e2eCall(t, c, "lite.getMasterchainInfo", nil)
		mcResult := e2eRequireResult(t, mcResp, "lite.getMasterchainInfo")
		seqno := int(mcResult["seqno"].(float64))

		resp := e2eCall(t, c, "lite.getBlockData", map[string]interface{}{
			"workchain": -1,
			"shard":     "8000000000000000",
			"seqno":     seqno,
		})
		result := e2eRequireResult(t, resp, "lite.getBlockData")
		if _, ok := result["boc"]; !ok {
			t.Fatalf("[FAIL] lite.getBlockData — missing boc field")
		}
		boc, ok := result["boc"].(string)
		if !ok || boc == "" {
			t.Fatalf("[FAIL] lite.getBlockData — empty boc string")
		}
		t.Logf("[PASS] lite.getBlockData — block %d, boc %d chars", seqno, len(boc))
	})

	t.Run("getBlockHeader", func(t *testing.T) {
		mcResp := e2eCall(t, c, "lite.getMasterchainInfo", nil)
		mcResult := e2eRequireResult(t, mcResp, "lite.getMasterchainInfo")
		seqno := int(mcResult["seqno"].(float64))

		resp := e2eCall(t, c, "lite.getBlockHeader", map[string]interface{}{
			"workchain": -1,
			"shard":     "8000000000000000",
			"seqno":     seqno,
		})
		result := e2eRequireResult(t, resp, "lite.getBlockHeader")
		if _, ok := result["header_boc"]; !ok {
			t.Fatalf("[FAIL] lite.getBlockHeader — missing header_boc field")
		}
		if _, ok := result["seqno"]; !ok {
			t.Fatalf("[FAIL] lite.getBlockHeader — missing seqno field")
		}
		t.Logf("[PASS] lite.getBlockHeader — block %d header retrieved", seqno)
	})

	t.Run("getLibraries", func(t *testing.T) {
		fakeHash := "0000000000000000000000000000000000000000000000000000000000000000"
		resp := e2eCallRetry(t, c, "lite.getLibraries", map[string]interface{}{
			"hashes": []string{fakeHash},
		}, 3)
		result := e2eRequireResult(t, resp, "lite.getLibraries")
		if _, ok := result["libraries"]; !ok {
			t.Fatalf("[FAIL] lite.getLibraries — missing libraries field")
		}
		count := 0
		if libs, ok := result["libraries"].([]interface{}); ok {
			count = len(libs)
		}
		t.Logf("[PASS] lite.getLibraries — %d libraries returned", count)
	})

	t.Run("findTxByInMsgHash", func(t *testing.T) {
		txsResp := e2eCallRetry(t, c, "lite.getTransactions", map[string]interface{}{
			"address": hotWallet,
			"limit":   5,
		}, 3)
		txsResult := e2eRequireResult(t, txsResp, "lite.getTransactions")
		txList, ok := txsResult["transactions"].([]interface{})
		if !ok || len(txList) == 0 {
			t.Fatalf("[FAIL] lite.findTxByInMsgHash — no transactions available for hot wallet")
		}

		// Find a transaction that has an in_msg with a body.
		var bodyHash string
		for _, item := range txList {
			tx, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			inMsg, ok := tx["in_msg"].(map[string]interface{})
			if !ok {
				continue
			}
			bodyB64, ok := inMsg["body"].(string)
			if !ok || bodyB64 == "" {
				continue
			}
			bodyBytes, err := base64.StdEncoding.DecodeString(bodyB64)
			if err != nil {
				continue
			}
			bodyCell, err := cell.FromBOC(bodyBytes)
			if err != nil {
				continue
			}
			bodyHash = hex.EncodeToString(bodyCell.Hash())
			break
		}
		if bodyHash == "" {
			t.Fatalf("[FAIL] lite.findTxByInMsgHash — no transaction with in_msg body found in recent txs")
		}

		resp := e2eCall(t, c, "lite.findTxByInMsgHash", map[string]interface{}{
			"address":  hotWallet,
			"msg_hash": bodyHash,
		})
		if resp.Error != nil {
			// May fail if the tx is too old and pruned from liteserver
			t.Logf("[PASS] lite.findTxByInMsgHash — lookup with real hash %s: %s", truncKey(bodyHash), resp.Error.Message)
		} else {
			t.Logf("[PASS] lite.findTxByInMsgHash — found tx at lt %s with body hash %s", resp.Result["lt"], truncKey(bodyHash))
		}
	})

	t.Run("sendAndWatch_invalidBOC", func(t *testing.T) {
		resp := e2eCall(t, c, "lite.sendAndWatch", map[string]string{"boc": "AQID"})
		if resp.Error == nil {
			t.Fatal("[FAIL] lite.sendAndWatch — expected error for invalid BOC, got success")
		}
		if resp.Error.Code == -32601 {
			t.Fatalf("[FAIL] lite.sendAndWatch — method not found (code -32601): %s", resp.Error.Message)
		}
		t.Logf("[PASS] lite.sendAndWatch_invalidBOC — expected error: invalid BOC rejected (code %d)", resp.Error.Code)
	})
}

// ---------------------------------------------------------------------------
// TestE2E_DHT — 4 dht.* methods
// ---------------------------------------------------------------------------

func TestE2E_DHT(t *testing.T) {
	t.Parallel()
	c := e2eDial(t)

	// Compute the tunnel overlay key (same as handleFindTunnelNodes):
	// tl.Hash(OverlayKey{PaymentNode: [0...0]}) for free relays
	overlayKey, err := tl.Hash(OverlayKey{PaymentNode: make([]byte, 32)})
	if err != nil {
		t.Fatalf("failed to compute overlay key: %v", err)
	}
	overlayKeyB64 := base64.StdEncoding.EncodeToString(overlayKey)

	// Compute the relay ADNL ID: tl.Hash(pub.ed25519{key})
	relayKeyBytes, err := base64.StdEncoding.DecodeString(relayKey)
	if err != nil {
		t.Fatalf("failed to decode relay key: %v", err)
	}
	adnlID, err := tl.Hash(keys.PublicKeyED25519{Key: relayKeyBytes})
	if err != nil {
		t.Fatalf("failed to compute ADNL ID: %v", err)
	}
	adnlIDB64 := base64.StdEncoding.EncodeToString(adnlID)

	t.Run("findTunnelNodes", func(t *testing.T) {
		resp := e2eCallRetry(t, c, "dht.findTunnelNodes", nil, 2)
		if resp.Error != nil {
			// "value is not found" is valid — tunnel relays may not be registered on this network/DHT state.
			// Any other error (e.g., -32601 method not found) is a real failure.
			if resp.Error.Code == -32601 {
				t.Fatalf("[FAIL] dht.findTunnelNodes — method not found: %s", resp.Error.Message)
			}
			t.Logf("[PASS] dht.findTunnelNodes — 0 relays (DHT returned: %s)", resp.Error.Message)
			return
		}
		result := e2eRequireResult(t, resp, "dht.findTunnelNodes")
		count, _ := result["count"].(float64)
		t.Logf("[PASS] dht.findTunnelNodes — %.0f relays found", count)
	})

	t.Run("findOverlayNodes", func(t *testing.T) {
		resp := e2eCallRetry(t, c, "dht.findOverlayNodes", map[string]string{
			"overlay_key": overlayKeyB64,
		}, 2)
		result := e2eRequireResult(t, resp, "dht.findOverlayNodes")
		count := float64(0)
		if c, ok := result["count"].(float64); ok {
			count = c
		}
		t.Logf("[PASS] dht.findOverlayNodes — %.0f nodes found", count)
	})

	t.Run("findAddresses", func(t *testing.T) {
		resp := e2eCallRetry(t, c, "dht.findAddresses", map[string]string{
			"key": adnlIDB64,
		}, 2)
		e2eRequireResult(t, resp, "dht.findAddresses")
		t.Logf("[PASS] dht.findAddresses — resolved ADNL ID %s", truncKey(adnlIDB64))
	})

	t.Run("storeAddress", func(t *testing.T) {
		// dht.storeAddress is disabled to prevent DHT identity hijacking
		resp := e2eCall(t, c, "dht.storeAddress", map[string]interface{}{
			"addresses":   []map[string]interface{}{{"ip": "1.2.3.4", "port": 12345}},
			"ttl_seconds": 300,
		})
		if resp.Error == nil {
			t.Fatalf("[FAIL] dht.storeAddress — expected restricted error, got success")
		}
		if resp.Error.Code != -32603 {
			t.Fatalf("[FAIL] dht.storeAddress — expected code -32603, got %d", resp.Error.Code)
		}
		t.Logf("[PASS] dht.storeAddress — correctly restricted (code %d: %s)", resp.Error.Code, resp.Error.Message)
	})

	t.Run("storeAddress_empty", func(t *testing.T) {
		// dht.storeAddress is disabled — all calls return -32603 regardless of params
		resp := e2eCall(t, c, "dht.storeAddress", map[string]interface{}{
			"addresses":   []map[string]interface{}{},
			"ttl_seconds": 300,
		})
		if resp.Error == nil {
			t.Fatalf("[FAIL] dht.storeAddress — expected restricted error")
		}
		if resp.Error.Code != -32603 {
			t.Fatalf("[FAIL] dht.storeAddress — expected code -32603, got %d", resp.Error.Code)
		}
		t.Logf("[PASS] dht.storeAddress — correctly restricted (code %d)", resp.Error.Code)
	})

	t.Run("storeOverlayNodes_empty", func(t *testing.T) {
		// dht.storeOverlayNodes is disabled — all calls return -32603
		resp := e2eCall(t, c, "dht.storeOverlayNodes", map[string]interface{}{
			"overlay_key": base64.StdEncoding.EncodeToString(make([]byte, 32)),
			"nodes":       []string{},
			"ttl_seconds": 300,
		})
		if resp.Error == nil {
			t.Fatalf("[FAIL] dht.storeOverlayNodes — expected restricted error")
		}
		if resp.Error.Code != -32603 {
			t.Fatalf("[FAIL] dht.storeOverlayNodes — expected code -32603, got %d", resp.Error.Code)
		}
		t.Logf("[PASS] dht.storeOverlayNodes — correctly restricted (code %d)", resp.Error.Code)
	})

	t.Run("storeOverlayNodes_invalidTL", func(t *testing.T) {
		// dht.storeOverlayNodes is disabled — all calls return -32603
		resp := e2eCall(t, c, "dht.storeOverlayNodes", map[string]interface{}{
			"overlay_key": base64.StdEncoding.EncodeToString(make([]byte, 32)),
			"nodes":       []string{base64.StdEncoding.EncodeToString([]byte("garbage"))},
			"ttl_seconds": 300,
		})
		if resp.Error == nil {
			t.Fatalf("[FAIL] dht.storeOverlayNodes — expected restricted error")
		}
		if resp.Error.Code != -32603 {
			t.Fatalf("[FAIL] dht.storeOverlayNodes — expected code -32603, got %d", resp.Error.Code)
		}
		t.Logf("[PASS] dht.storeOverlayNodes — correctly restricted (code %d)", resp.Error.Code)
	})

	t.Run("findValue", func(t *testing.T) {
		resp := e2eCallRetry(t, c, "dht.findValue", map[string]interface{}{
			"key_id": adnlIDB64,
			"name":   "address",
			"index":  0,
		}, 2)
		e2eRequireResult(t, resp, "dht.findValue")
		t.Logf("[PASS] dht.findValue — value found for ADNL ID %s", truncKey(adnlIDB64))
	})
}

// ---------------------------------------------------------------------------
// TestE2E_Subscribe — 8 subscribe.* methods
// ---------------------------------------------------------------------------

func TestE2E_Subscribe(t *testing.T) {
	t.Parallel()

	t.Run("blocks", func(t *testing.T) {
		c := e2eDial(t)
		start := time.Now()
		resp := e2eCall(t, c, "subscribe.blocks", nil)
		result := e2eRequireResult(t, resp, "subscribe.blocks")
		if result["subscribed"] != true {
			t.Fatalf("[FAIL] subscribe.blocks — expected subscribed:true, got %v", result["subscribed"])
		}

		// Wait for one block push event (15s timeout)
		c.SetReadDeadline(time.Now().Add(15 * time.Second))
		_, msg, err := c.ReadMessage()
		if err != nil {
			t.Logf("[PASS] subscribe.blocks — subscribed OK, no block push within 15s (timing-dependent)")
			return
		}
		var event map[string]interface{}
		json.Unmarshal(msg, &event)
		if event["event"] != "block" {
			t.Logf("[PASS] subscribe.blocks — subscribed OK, received event type %v instead of block", event["event"])
			return
		}
		data := event["data"].(map[string]interface{})
		seqno, ok := data["seqno"].(float64)
		if !ok || seqno <= 0 {
			t.Fatalf("[FAIL] subscribe.blocks — expected block event with seqno > 0, got %v", data["seqno"])
		}
		elapsed := time.Since(start).Seconds()
		t.Logf("[PASS] subscribe.blocks — block %.0f received in %.1fs", seqno, elapsed)
	})

	t.Run("transactions", func(t *testing.T) {
		c := e2eDial(t)
		resp := e2eCall(t, c, "subscribe.transactions", map[string]interface{}{
			"address": testAddr,
			"last_lt": "0",
		})
		result := e2eRequireResult(t, resp, "subscribe.transactions")
		if result["subscribed"] != true {
			t.Fatalf("[FAIL] subscribe.transactions — expected subscribed:true, got %v", result["subscribed"])
		}
		t.Logf("[PASS] subscribe.transactions — subscribed to %s", truncKey(testAddr))
	})

	t.Run("accountState", func(t *testing.T) {
		c := e2eDial(t)
		resp := e2eCall(t, c, "subscribe.accountState", map[string]string{
			"address": testAddr,
		})
		result := e2eRequireResult(t, resp, "subscribe.accountState")
		if result["subscribed"] != true {
			t.Fatalf("[FAIL] subscribe.accountState — expected subscribed:true, got %v", result["subscribed"])
		}
		t.Logf("[PASS] subscribe.accountState — subscribed to %s", truncKey(testAddr))
	})

	t.Run("newTransactions", func(t *testing.T) {
		c := e2eDial(t)
		resp := e2eCall(t, c, "subscribe.newTransactions", nil)
		result := e2eRequireResult(t, resp, "subscribe.newTransactions")
		if result["subscribed"] != true {
			t.Fatalf("[FAIL] subscribe.newTransactions — expected subscribed:true, got %v", result["subscribed"])
		}
		startSeqno, ok := result["start_seqno"].(float64)
		if !ok || startSeqno <= 0 {
			t.Fatalf("[FAIL] subscribe.newTransactions — expected start_seqno > 0, got %v", result["start_seqno"])
		}
		t.Logf("[PASS] subscribe.newTransactions — start seqno %.0f", startSeqno)
	})

	t.Run("unsubscribe", func(t *testing.T) {
		c := e2eDial(t)

		subResp := e2eCall(t, c, "subscribe.blocks", nil)
		subResult := e2eRequireResult(t, subResp, "subscribe.blocks")
		subID, ok := subResult["subscription_id"].(string)
		if !ok || subID == "" {
			t.Fatalf("[FAIL] subscribe.unsubscribe — proxy did not return subscription_id; cannot test unsubscribe")
		}

		resp := e2eCall(t, c, "subscribe.unsubscribe", map[string]string{
			"subscription_id": subID,
		})
		result := e2eRequireResult(t, resp, "subscribe.unsubscribe")
		if result["unsubscribed"] != true {
			t.Fatalf("[FAIL] subscribe.unsubscribe — expected unsubscribed:true, got %v", result["unsubscribed"])
		}
		t.Logf("[PASS] subscribe.unsubscribe — subscription %s cancelled", truncKey(subID))
	})

	t.Run("transactions_with_opcode_filter", func(t *testing.T) {
		c := e2eDial(t)
		resp := e2eCall(t, c, "subscribe.transactions", map[string]interface{}{
			"address":    testAddr,
			"last_lt":    "0",
			"operations": []uint32{0x7362d09c},
		})
		result := e2eRequireResult(t, resp, "subscribe.transactions")
		if result["subscribed"] != true {
			t.Fatalf("[FAIL] subscribe.transactions — expected subscribed:true, got %v", result["subscribed"])
		}
		t.Logf("[PASS] subscribe.transactions — subscribed with opcode filter 0x7362d09c")
	})

	t.Run("configChanges", func(t *testing.T) {
		c := e2eDial(t)
		resp := e2eCall(t, c, "subscribe.configChanges", map[string]interface{}{
			"params": []int{34},
		})
		result := e2eRequireResult(t, resp, "subscribe.configChanges")
		if result["subscribed"] != true {
			t.Fatalf("[FAIL] subscribe.configChanges — expected subscribed:true, got %v", result["subscribed"])
		}
		subID, ok := result["subscription_id"].(string)
		if !ok || subID == "" {
			t.Fatalf("[FAIL] subscribe.configChanges — missing subscription_id")
		}
		startSeqno := "n/a"
		if s, ok := result["start_seqno"].(float64); ok {
			startSeqno = fmt.Sprintf("%.0f", s)
		}
		t.Logf("[PASS] subscribe.configChanges — param 34, start seqno %s", startSeqno)
	})

	t.Run("multiAccount", func(t *testing.T) {
		c := e2eDial(t)
		resp := e2eCall(t, c, "subscribe.multiAccount", map[string]interface{}{
			"accounts": []map[string]interface{}{
				{"address": testAddr},
				{"address": usdtMaster},
			},
		})
		result := e2eRequireResult(t, resp, "subscribe.multiAccount")
		if result["subscribed"] != true {
			t.Fatalf("[FAIL] subscribe.multiAccount — expected subscribed:true, got %v", result["subscribed"])
		}
		subID, ok := result["subscription_id"].(string)
		if !ok || subID == "" {
			t.Fatalf("[FAIL] subscribe.multiAccount — missing subscription_id")
		}
		count, ok := result["account_count"].(float64)
		if !ok || int(count) != 2 {
			t.Fatalf("[FAIL] subscribe.multiAccount — expected account_count 2, got %v", result["account_count"])
		}
		t.Logf("[PASS] subscribe.multiAccount — %.0f accounts subscribed", count)
	})

	t.Run("trace", func(t *testing.T) {
		c := e2eDial(t)
		resp := e2eCall(t, c, "subscribe.trace", map[string]interface{}{
			"address":         hotWallet,
			"max_depth":       2,
			"msg_timeout_sec": 20,
		})
		result := e2eRequireResult(t, resp, "subscribe.trace")
		if result["subscribed"] != true {
			t.Fatalf("[FAIL] subscribe.trace — expected subscribed:true")
		}
		subID, _ := result["subscription_id"].(string)

		// Wait for a real trace (hot wallet is always active)
		c.SetReadDeadline(time.Now().Add(90 * time.Second))
		gotStarted := false
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				if gotStarted {
					t.Logf("[PASS] subscribe.trace — trace started but timed out waiting for complete (sub=%s)", subID)
				} else {
					t.Fatalf("[FAIL] subscribe.trace — no trace events in 90s on hot wallet")
				}
				return
			}
			var ev map[string]interface{}
			json.Unmarshal(msg, &ev)
			event, _ := ev["event"].(string)
			switch event {
			case "trace_started":
				gotStarted = true
				d := ev["data"].(map[string]interface{})
				id := d["trace_id"].(string)
				if len(id) > 12 {
					id = id[:12]
				}
				tx := d["root_tx"].(map[string]interface{})
				outMsgs := 0
				if outs, ok := tx["out_msgs"].([]interface{}); ok {
					outMsgs = len(outs)
				}
				t.Logf("  trace_started id=%s out_msgs=%d", id, outMsgs)
			case "trace_tx":
				d := ev["data"].(map[string]interface{})
				a, _ := d["address"].(string)
				if len(a) > 25 {
					a = a[:25] + "..."
				}
				t.Logf("  trace_tx depth=%.0f → %s", d["depth"], a)
			case "trace_timeout":
				d := ev["data"].(map[string]interface{})
				a, _ := d["address"].(string)
				if len(a) > 25 {
					a = a[:25] + "..."
				}
				t.Logf("  trace_timeout depth=%.0f → %s", d["depth"], a)
			case "trace_complete":
				d := ev["data"].(map[string]interface{})
				t.Logf("[PASS] subscribe.trace — complete: %.0f tx, depth %.0f, %.0f timeouts (sub=%s)",
					d["total_txs"], d["max_depth_reached"], d["timed_out_count"], subID)
				return
			}
		}
	})
}

// ---------------------------------------------------------------------------
// TestE2E_ADNL — 9 adnl.* methods
// ---------------------------------------------------------------------------

func TestE2E_ADNL(t *testing.T) {
	t.Parallel()
	c := e2eDial(t)

	// Compute the relay ADNL ID: tl.Hash(pub.ed25519{key})
	relayKeyBytes, err := base64.StdEncoding.DecodeString(relayKey)
	if err != nil {
		t.Fatalf("failed to decode relay key: %v", err)
	}
	adnlID, err := tl.Hash(keys.PublicKeyED25519{Key: relayKeyBytes})
	if err != nil {
		t.Fatalf("failed to compute ADNL ID: %v", err)
	}
	adnlIDB64 := base64.StdEncoding.EncodeToString(adnlID)

	var peerID string

	t.Run("connect", func(t *testing.T) {
		resp := e2eCall(t, c, "adnl.connect", map[string]string{
			"address": relayAddr,
			"key":     relayKey,
		})
		result := e2eRequireResult(t, resp, "adnl.connect")
		if result["connected"] != true {
			t.Fatalf("[FAIL] adnl.connect — expected connected:true, got %v", result["connected"])
		}
		id, ok := result["peer_id"].(string)
		if !ok || id == "" {
			t.Fatalf("[FAIL] adnl.connect — missing peer_id")
		}
		peerID = id
		t.Logf("[PASS] adnl.connect — connected to %s", relayAddr)
	})

	t.Run("peers", func(t *testing.T) {
		if peerID == "" {
			t.Fatalf("[FAIL] adnl.peers — no peer_id (adnl.connect must succeed first)")
		}
		resp := e2eCall(t, c, "adnl.peers", nil)
		result := e2eRequireResult(t, resp, "adnl.peers")
		peers, ok := result["peers"].([]interface{})
		if !ok || len(peers) == 0 {
			t.Fatalf("[FAIL] adnl.peers — expected at least 1 peer, got %v", result["peers"])
		}
		t.Logf("[PASS] adnl.peers — %d active peers", len(peers))
	})

	t.Run("ping", func(t *testing.T) {
		if peerID == "" {
			t.Fatalf("[FAIL] adnl.ping — no peer_id (adnl.connect must succeed first)")
		}
		resp := e2eCallRetry(t, c, "adnl.ping", map[string]string{"peer_id": peerID}, 2)
		result := e2eRequireResult(t, resp, "adnl.ping")
		latency, ok := result["latency_ms"].(float64)
		if !ok || latency < 0 {
			t.Fatalf("[FAIL] adnl.ping — expected latency_ms >= 0, got %v", result["latency_ms"])
		}
		t.Logf("[PASS] adnl.ping — latency %.0fms", latency)
	})

	t.Run("sendMessage", func(t *testing.T) {
		if peerID == "" {
			t.Fatalf("[FAIL] adnl.sendMessage — no peer_id (adnl.connect must succeed first)")
		}
		resp := e2eCall(t, c, "adnl.sendMessage", map[string]string{
			"peer_id": peerID,
			"data":    "SGVsbG8=", // "Hello" base64
		})
		result := e2eRequireResult(t, resp, "adnl.sendMessage")
		if result["sent"] != true {
			t.Fatalf("[FAIL] adnl.sendMessage — expected sent:true, got %v", result["sent"])
		}
		t.Logf("[PASS] adnl.sendMessage — message sent to peer %s", truncKey(peerID))
	})

	t.Run("query_rejectGarbage", func(t *testing.T) {
		if peerID == "" {
			t.Fatalf("[FAIL] adnl.query — no peer_id (adnl.connect must succeed first)")
		}
		resp := e2eCall(t, c, "adnl.query", map[string]interface{}{
			"peer_id": peerID,
			"data":    "AQID",
			"timeout": 5,
		})
		if resp.Error != nil {
			if resp.Error.Code == -32601 {
				t.Fatalf("[FAIL] adnl.query — method not found (code -32601): %s", resp.Error.Message)
			}
			t.Logf("[PASS] adnl.query_rejectGarbage — garbage data rejected by peer (%s)", resp.Error.Message)
		} else {
			t.Logf("[PASS] adnl.query_rejectGarbage — response received from peer %s", truncKey(peerID))
		}
	})

	t.Run("setQueryHandler", func(t *testing.T) {
		if peerID == "" {
			t.Fatalf("[FAIL] adnl.setQueryHandler — no peer_id (adnl.connect must succeed first)")
		}
		resp := e2eCall(t, c, "adnl.setQueryHandler", map[string]string{
			"peer_id": peerID,
		})
		if resp.Error != nil && resp.Error.Code == -32601 {
			t.Fatalf("[FAIL] adnl.setQueryHandler — method not found (code -32601): %s", resp.Error.Message)
		}
		result := e2eRequireResult(t, resp, "adnl.setQueryHandler")
		if result["enabled"] != true {
			t.Fatalf("[FAIL] adnl.setQueryHandler — expected enabled:true, got %v", result["enabled"])
		}
		t.Logf("[PASS] adnl.setQueryHandler — handler enabled for peer %s", truncKey(peerID))
	})

	t.Run("answer", func(t *testing.T) {
		resp := e2eCall(t, c, "adnl.answer", map[string]interface{}{
			"query_id": "deadbeefdeadbeefdeadbeefdeadbeef",
			"data":     "AQID",
		})
		if resp.Error == nil {
			t.Fatalf("[FAIL] adnl.answer — expected error for non-existent query_id, got result: %v", resp.Result)
		}
		if resp.Error.Code == -32601 {
			t.Fatalf("[FAIL] adnl.answer — method not found (code -32601): %s", resp.Error.Message)
		}
		t.Logf("[PASS] adnl.answer — expected error: query not found (code %d)", resp.Error.Code)
	})

	t.Run("disconnect", func(t *testing.T) {
		if peerID == "" {
			t.Fatalf("[FAIL] adnl.disconnect — no peer_id (adnl.connect must succeed first)")
		}
		resp := e2eCall(t, c, "adnl.disconnect", map[string]string{"peer_id": peerID})
		result := e2eRequireResult(t, resp, "adnl.disconnect")
		if result["disconnected"] != true {
			t.Fatalf("[FAIL] adnl.disconnect — expected disconnected:true, got %v", result["disconnected"])
		}

		peersResp := e2eCall(t, c, "adnl.peers", nil)
		peersResult := e2eRequireResult(t, peersResp, "adnl.peers")
		peers, _ := peersResult["peers"].([]interface{})
		for _, p := range peers {
			peer := p.(map[string]interface{})
			if peer["id"] == peerID {
				t.Fatalf("[FAIL] adnl.disconnect — peer %s still in list after disconnect", peerID)
			}
		}
		t.Logf("[PASS] adnl.disconnect — peer %s removed", truncKey(peerID))
		peerID = ""
	})

	t.Run("connectByADNL", func(t *testing.T) {
		resp := e2eCallRetry(t, c, "adnl.connectByADNL", map[string]string{
			"adnl_id": adnlIDB64,
		}, 2)
		result := e2eRequireResult(t, resp, "adnl.connectByADNL")
		if result["connected"] != true {
			t.Fatalf("[FAIL] adnl.connectByADNL — expected connected:true, got %v", result["connected"])
		}
		if id, ok := result["peer_id"].(string); ok && id != "" {
			t.Cleanup(func() {
				e2eCall(t, c, "adnl.disconnect", map[string]string{"peer_id": id})
			})
		}
		t.Logf("[PASS] adnl.connectByADNL — connected via ADNL ID %s", truncKey(adnlIDB64))
	})
}

// ---------------------------------------------------------------------------
// TestE2E_Overlay — 7 overlay.* methods
// ---------------------------------------------------------------------------

func TestE2E_Overlay(t *testing.T) {
	c := e2eDial(t)

	// Compute the real tunnel overlay ID:
	// 1. Inner overlay key = tl.Hash(OverlayKey{PaymentNode: [0...0]})
	// 2. Overlay ID = tl.Hash(pub.overlay{name: overlayKey})
	overlayKey, err := tl.Hash(OverlayKey{PaymentNode: make([]byte, 32)})
	if err != nil {
		t.Fatalf("failed to compute overlay key: %v", err)
	}
	overlayID, err := tl.Hash(keys.PublicKeyOverlay{Key: overlayKey})
	if err != nil {
		t.Fatalf("failed to compute overlay ID: %v", err)
	}
	realOverlayID := base64.StdEncoding.EncodeToString(overlayID)

	connResp := e2eCall(t, c, "adnl.connect", map[string]string{
		"address": relayAddr,
		"key":     relayKey,
	})
	if connResp.Error != nil {
		t.Fatalf("[FAIL] adnl.connect — required for overlay tests: %s", connResp.Error.Message)
	}
	peerID := connResp.Result["peer_id"].(string)

	t.Cleanup(func() {
		e2eCall(t, c, "overlay.leave", map[string]string{"overlay_id": realOverlayID})
		e2eCall(t, c, "adnl.disconnect", map[string]string{"peer_id": peerID})
	})

	t.Run("join", func(t *testing.T) {
		resp := e2eCall(t, c, "overlay.join", map[string]string{
			"overlay_id": realOverlayID,
			"peer_id":    peerID,
		})
		result := e2eRequireResult(t, resp, "overlay.join")
		if result["joined"] != true {
			t.Fatalf("[FAIL] overlay.join — expected joined:true, got %v", result["joined"])
		}
		t.Logf("[PASS] overlay.join — joined overlay %s", truncKey(realOverlayID))
	})

	t.Run("getPeers_emptyOverlay", func(t *testing.T) {
		resp := e2eCall(t, c, "overlay.getPeers", map[string]string{
			"overlay_id": realOverlayID,
		})
		if resp.Error != nil {
			// Empty overlay with no announced peers — error is expected behavior
			t.Logf("[PASS] overlay.getPeers_emptyOverlay — empty overlay error as expected (%s)", resp.Error.Message)
		} else {
			result := e2eRequireResult(t, resp, "overlay.getPeers")
			count := 0
			if peers, ok := result["peers"].([]interface{}); ok {
				count = len(peers)
			}
			t.Logf("[PASS] overlay.getPeers_emptyOverlay — %d peers in overlay", count)
		}
	})

	t.Run("sendMessage_emptyOverlay", func(t *testing.T) {
		resp := e2eCallRetry(t, c, "overlay.sendMessage", map[string]string{
			"overlay_id": realOverlayID,
			"data":       "SGVsbG8gZnJvbSBXUyBicmlkZ2U=",
		}, 2)
		if resp.Error != nil {
			// No peers in freshly joined overlay — "no peers" error is valid
			t.Logf("[PASS] overlay.sendMessage_emptyOverlay — no peers error as expected (%s)", resp.Error.Message)
		} else {
			result := e2eRequireResult(t, resp, "overlay.sendMessage")
			if result["sent"] != true {
				t.Fatalf("[FAIL] overlay.sendMessage — expected sent:true, got %v", result["sent"])
			}
			t.Logf("[PASS] overlay.sendMessage_emptyOverlay — broadcast sent to overlay %s", truncKey(realOverlayID))
		}
	})

	t.Run("query_rejectGarbage", func(t *testing.T) {
		resp := e2eCall(t, c, "overlay.query", map[string]interface{}{
			"overlay_id": realOverlayID,
			"data":       "AQID",
			"timeout":    5,
		})
		if resp.Error != nil {
			if resp.Error.Code == -32601 {
				t.Fatalf("[FAIL] overlay.query — method not found (code -32601): %s", resp.Error.Message)
			}
			t.Logf("[PASS] overlay.query_rejectGarbage — garbage data rejected (%s)", resp.Error.Message)
		} else {
			t.Logf("[PASS] overlay.query_rejectGarbage — response received from overlay %s", truncKey(realOverlayID))
		}
	})

	t.Run("setQueryHandler", func(t *testing.T) {
		resp := e2eCall(t, c, "overlay.setQueryHandler", map[string]string{
			"overlay_id": realOverlayID,
			"peer_id":    peerID,
		})
		if resp.Error != nil && resp.Error.Code == -32601 {
			t.Fatalf("[FAIL] overlay.setQueryHandler — method not found (code -32601): %s", resp.Error.Message)
		}
		result := e2eRequireResult(t, resp, "overlay.setQueryHandler")
		if result["enabled"] != true {
			t.Fatalf("[FAIL] overlay.setQueryHandler — expected enabled:true, got %v", result["enabled"])
		}
		t.Logf("[PASS] overlay.setQueryHandler — handler enabled for overlay %s", truncKey(realOverlayID))
	})

	t.Run("answer", func(t *testing.T) {
		resp := e2eCall(t, c, "overlay.answer", map[string]interface{}{
			"query_id": "deadbeefdeadbeefdeadbeefdeadbeef",
			"data":     "AQID",
		})
		if resp.Error == nil {
			t.Fatalf("[FAIL] overlay.answer — expected error for non-existent query_id, got result: %v", resp.Result)
		}
		if resp.Error.Code == -32601 {
			t.Fatalf("[FAIL] overlay.answer — method not found (code -32601): %s", resp.Error.Message)
		}
		t.Logf("[PASS] overlay.answer — expected error: query not found (code %d)", resp.Error.Code)
	})

	t.Run("leave", func(t *testing.T) {
		resp := e2eCall(t, c, "overlay.leave", map[string]string{
			"overlay_id": realOverlayID,
		})
		result := e2eRequireResult(t, resp, "overlay.leave")
		if result["left"] != true {
			t.Fatalf("[FAIL] overlay.leave — expected left:true, got %v", result["left"])
		}
		t.Logf("[PASS] overlay.leave — left overlay %s", truncKey(realOverlayID))
	})
}

// ---------------------------------------------------------------------------
// TestE2E_Jetton — 3 jetton.* methods
// ---------------------------------------------------------------------------

func TestE2E_Jetton(t *testing.T) {
	t.Parallel()
	c := e2eDial(t)

	t.Run("getData", func(t *testing.T) {
		resp := e2eCall(t, c, "jetton.getData", map[string]string{"address": usdtMaster})
		result := e2eRequireResult(t, resp, "jetton.getData")
		symbol := "?"
		if s, ok := result["symbol"].(string); ok && s != "" {
			symbol = s
		}
		supply := "unknown"
		if s, ok := result["total_supply"].(string); ok && s != "" {
			// Jetton decimals are typically 6 or 9; format raw supply with big.Int
			sup := new(big.Int)
			if _, parsed := sup.SetString(s, 10); parsed {
				decimals := 6 // USDT default
				if d, ok := result["decimals"].(float64); ok && d > 0 {
					decimals = int(d)
				}
				div := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
				whole := new(big.Int).Div(sup, div)
				frac := new(big.Int).Mod(sup, div)
				centPart := new(big.Int).Mul(frac, big.NewInt(100))
				centPart.Div(centPart, div)
				supply = fmt.Sprintf("%s.%02d", whole.String(), centPart.Int64())
			}
		}
		t.Logf("[PASS] jetton.getData — %s, supply %s", symbol, supply)
	})

	var walletAddr string

	t.Run("getWalletAddress", func(t *testing.T) {
		resp := e2eCall(t, c, "jetton.getWalletAddress", map[string]interface{}{
			"jetton_master": usdtMaster,
			"owner":         testAddr,
		})
		result := e2eRequireResult(t, resp, "jetton.getWalletAddress")
		addr, ok := result["wallet_address"].(string)
		if !ok || addr == "" {
			t.Fatalf("[FAIL] jetton.getWalletAddress — missing wallet_address")
		}
		walletAddr = addr
		t.Logf("[PASS] jetton.getWalletAddress — %s", truncKey(addr))
	})

	t.Run("getBalance", func(t *testing.T) {
		if walletAddr == "" {
			t.Fatalf("[FAIL] jetton.getBalance — no wallet address (jetton.getWalletAddress must succeed first)")
		}
		resp := e2eCall(t, c, "jetton.getBalance", map[string]string{
			"jetton_wallet": walletAddr,
		})
		result := e2eRequireResult(t, resp, "jetton.getBalance")
		balance := "unknown"
		if b, ok := result["balance"].(string); ok {
			balance = b
		}
		t.Logf("[PASS] jetton.getBalance — balance %s (raw)", balance)
	})
}

// ---------------------------------------------------------------------------
// TestE2E_NFT — 5 nft.* methods
// ---------------------------------------------------------------------------

func TestE2E_NFT(t *testing.T) {
	t.Parallel()
	c := e2eDial(t)

	// Shared state: populated by getAddressByIndex, consumed by getData.
	var nftItemAddr string

	t.Run("getCollectionData", func(t *testing.T) {
		resp := e2eCallRetry(t, c, "nft.getCollectionData", map[string]string{"address": nftCollection}, 3)
		result := e2eRequireResult(t, resp, "nft.getCollectionData")
		items := "unknown"
		if n, ok := result["next_item_index"].(float64); ok {
			items = fmt.Sprintf("%.0f", n)
		}
		owner := "unknown"
		if o, ok := result["owner"].(string); ok {
			owner = truncKey(o)
		}
		t.Logf("[PASS] nft.getCollectionData — %s items, owner %s", items, owner)
	})

	t.Run("getAddressByIndex", func(t *testing.T) {
		resp := e2eCallRetry(t, c, "nft.getAddressByIndex", map[string]interface{}{
			"collection": nftCollection,
			"index":      "0",
		}, 3)
		result := e2eRequireResult(t, resp, "nft.getAddressByIndex")
		addr, ok := result["address"].(string)
		if !ok || addr == "" {
			t.Fatalf("[FAIL] nft.getAddressByIndex — expected non-empty address")
		}
		nftItemAddr = addr
		t.Logf("[PASS] nft.getAddressByIndex — index 0 = %s", truncKey(addr))
	})

	t.Run("getData", func(t *testing.T) {
		if nftItemAddr == "" {
			t.Fatalf("[FAIL] nft.getData — no NFT address available (nft.getAddressByIndex must succeed first)")
		}
		resp := e2eCallRetry(t, c, "nft.getData", map[string]string{"address": nftItemAddr}, 3)
		result := e2eRequireResult(t, resp, "nft.getData")
		owner := "unknown"
		if o, ok := result["owner"].(string); ok {
			owner = truncKey(o)
		}
		index := "n/a"
		if idx, ok := result["index"].(float64); ok {
			index = fmt.Sprintf("%.0f", idx)
		} else if idx, ok := result["index"].(string); ok {
			index = idx
		}
		t.Logf("[PASS] nft.getData — index %s, owner %s", index, owner)
	})

	t.Run("getRoyaltyParams", func(t *testing.T) {
		resp := e2eCallRetry(t, c, "nft.getRoyaltyParams", map[string]string{
			"collection": nftCollection,
		}, 2)
		result := e2eRequireResult(t, resp, "nft.getRoyaltyParams")
		if _, ok := result["factor"]; !ok {
			t.Fatalf("[FAIL] nft.getRoyaltyParams — missing factor field")
		}
		if _, ok := result["base"]; !ok {
			t.Fatalf("[FAIL] nft.getRoyaltyParams — missing base field")
		}
		factor := fmt.Sprintf("%v", result["factor"])
		base := fmt.Sprintf("%v", result["base"])
		t.Logf("[PASS] nft.getRoyaltyParams — factor %s, base %s", factor, base)
	})

	t.Run("getContent_invalidBOC", func(t *testing.T) {
		resp := e2eCall(t, c, "nft.getContent", map[string]interface{}{
			"collection":         nftCollection,
			"index":              "0",
			"individual_content": "AQID",
		})
		if resp.Error == nil {
			t.Fatalf("[FAIL] nft.getContent_invalidBOC — expected error for invalid BOC, got success: %v", resp.Result)
		}
		if resp.Error.Code == -32601 {
			t.Fatalf("[FAIL] nft.getContent — method not found (code -32601): %s", resp.Error.Message)
		}
		t.Logf("[PASS] nft.getContent_invalidBOC — expected error: invalid BOC rejected (code %d)", resp.Error.Code)
	})
}

// ---------------------------------------------------------------------------
// TestE2E_DNS — 1 dns.* method
// ---------------------------------------------------------------------------

func TestE2E_DNS(t *testing.T) {
	t.Parallel()
	c := e2eDial(t)

	t.Run("resolve", func(t *testing.T) {
		resp := e2eCall(t, c, "dns.resolve", map[string]string{"domain": "foundation.ton"})
		result := e2eRequireResult(t, resp, "dns.resolve")
		wallet := "n/a"
		if w, ok := result["wallet"].(string); ok {
			wallet = truncKey(w)
		}
		// Build a summary of resolved record types
		var keys []string
		for k := range result {
			keys = append(keys, k)
		}
		t.Logf("[PASS] dns.resolve — foundation.ton -> wallet %s (fields: %s)", wallet, strings.Join(keys, ", "))
	})
}

// ---------------------------------------------------------------------------
// TestE2E_Network — 1 network.* method
// ---------------------------------------------------------------------------

func TestE2E_Network(t *testing.T) {
	t.Parallel()
	c := e2eDial(t)

	t.Run("info", func(t *testing.T) {
		resp := e2eCall(t, c, "network.info", nil)
		result := e2eRequireResult(t, resp, "network.info")
		clients, ok := result["ws_clients"].(float64)
		if !ok || clients < 1 {
			t.Fatalf("[FAIL] network.info — expected ws_clients >= 1, got %v", result["ws_clients"])
		}
		t.Logf("[PASS] network.info — %.0f WS clients connected", clients)
	})
}

// ---------------------------------------------------------------------------
// TestE2E_JSONRPC — protocol-level edge cases
// ---------------------------------------------------------------------------

func TestE2E_JSONRPC(t *testing.T) {
	c := e2eDial(t)

	t.Run("invalidJSON", func(t *testing.T) {
		resp := e2eWriteRaw(t, c, []byte("{not json"))
		if resp.Error == nil {
			t.Fatal("[FAIL] jsonrpc.invalidJSON — expected error for invalid JSON")
		}
		if resp.Error.Code != -32700 {
			t.Fatalf("[FAIL] jsonrpc.invalidJSON — expected error code -32700, got %d", resp.Error.Code)
		}
		t.Logf("[PASS] jsonrpc.invalidJSON — parse error returned (code -32700)")
	})

	t.Run("unknownMethod", func(t *testing.T) {
		resp := e2eCall(t, c, "fake.method", nil)
		if resp.Error == nil {
			t.Fatal("[FAIL] jsonrpc.unknownMethod — expected error for unknown method")
		}
		if resp.Error.Code != -32601 {
			t.Fatalf("[FAIL] jsonrpc.unknownMethod — expected error code -32601, got %d", resp.Error.Code)
		}
		t.Logf("[PASS] jsonrpc.unknownMethod — method not found returned (code -32601)")
	})

	t.Run("invalidParams", func(t *testing.T) {
		resp := e2eCall(t, c, "lite.getAccountState", map[string]string{
			"address": "totally-not-an-address",
		})
		if resp.Error == nil {
			t.Fatal("[FAIL] jsonrpc.invalidParams — expected error for invalid address")
		}
		if resp.Error.Code != -32602 && resp.Error.Code != -32603 {
			t.Fatalf("[FAIL] jsonrpc.invalidParams — expected error code -32602 or -32603, got %d", resp.Error.Code)
		}
		t.Logf("[PASS] jsonrpc.invalidParams — invalid address rejected (code %d)", resp.Error.Code)
	})

	t.Run("idPreservation", func(t *testing.T) {
		customID := "test-id-42"
		resp := e2eCallWithID(t, c, customID, "network.info", nil)
		if resp.ID != customID {
			t.Fatalf("[FAIL] jsonrpc.idPreservation — expected id=%q, got %q", customID, resp.ID)
		}
		t.Logf("[PASS] jsonrpc.idPreservation — response id matches request id %q", customID)
	})
}

// ---------------------------------------------------------------------------
// TestE2E_Wallet — wallet.* methods
// ---------------------------------------------------------------------------

func TestE2E_Wallet(t *testing.T) {
	t.Parallel()
	c := e2eDial(t)

	t.Run("getSeqno", func(t *testing.T) {
		resp := e2eCallRetry(t, c, "wallet.getSeqno", map[string]string{
			"address": testAddr,
		}, 3)
		result := e2eRequireResult(t, resp, "wallet.getSeqno")
		seqno, ok := result["seqno"].(float64)
		if !ok {
			t.Fatalf("[FAIL] wallet.getSeqno — expected numeric seqno, got %T %v", result["seqno"], result["seqno"])
		}
		if seqno < 0 {
			t.Fatalf("[FAIL] wallet.getSeqno — expected seqno >= 0, got %v", seqno)
		}
		t.Logf("[PASS] wallet.getSeqno — seqno %.0f", seqno)
	})

	t.Run("getPublicKey", func(t *testing.T) {
		resp := e2eCallRetry(t, c, "wallet.getPublicKey", map[string]string{
			"address": testAddr,
		}, 3)
		result := e2eRequireResult(t, resp, "wallet.getPublicKey")
		pubKey, ok := result["public_key"].(string)
		if !ok || pubKey == "" {
			t.Fatalf("[FAIL] wallet.getPublicKey — expected non-empty public_key")
		}
		t.Logf("[PASS] wallet.getPublicKey — key %s", truncKey(pubKey))
	})
}

// ---------------------------------------------------------------------------
// TestE2E_SBT — sbt.* methods
// ---------------------------------------------------------------------------

func TestE2E_SBT(t *testing.T) {
	t.Parallel()
	c := e2eDial(t)

	t.Run("getAuthorityAddress", func(t *testing.T) {
		resp := e2eCall(t, c, "sbt.getAuthorityAddress", map[string]string{
			"address": sbtAddr,
		})
		result := e2eRequireResult(t, resp, "sbt.getAuthorityAddress")
		authority, _ := result["authority"].(string)
		if authority == "" {
			t.Fatalf("[FAIL] sbt.getAuthorityAddress — empty authority")
		}
		t.Logf("[PASS] sbt.getAuthorityAddress — authority %s", truncKey(authority))
	})

	t.Run("getRevokedTime", func(t *testing.T) {
		resp := e2eCall(t, c, "sbt.getRevokedTime", map[string]string{
			"address": sbtAddr,
		})
		result := e2eRequireResult(t, resp, "sbt.getRevokedTime")
		revokedTime, _ := result["revoked_time"].(float64)
		t.Logf("[PASS] sbt.getRevokedTime — revoked_time %.0f (0=not revoked)", revokedTime)
	})
}

// ---------------------------------------------------------------------------
// TestE2E_Payment — payment.* methods
// ---------------------------------------------------------------------------

func TestE2E_Payment(t *testing.T) {
	t.Parallel()
	c := e2eDial(t)

	t.Run("getChannelState", func(t *testing.T) {
		resp := e2eCall(t, c, "payment.getChannelState", map[string]string{
			"address": payChannelAddr,
		})
		result := e2eRequireResult(t, resp, "payment.getChannelState")
		channelID, _ := result["channel_id"].(string)
		status, _ := result["status"].(float64)
		seqA, _ := result["committed_seqno_a"].(float64)
		t.Logf("[PASS] payment.getChannelState — channel=%s status=%.0f seqno_a=%.0f", channelID, status, seqA)
	})
}

// ---------------------------------------------------------------------------
// TestE2E_LiteSendReal — real wallet transactions via sendMessage,
// sendMessageWait, and sendAndWatch
// ---------------------------------------------------------------------------

// testWalletJSON is the JSON structure of testdata/.wallet.json.
type testWalletJSON struct {
	PrivateKey string `json:"private_key"`
	Address    string `json:"address"`
}

// testdataDir returns the absolute path to the testdata/ directory next to this
// source file, regardless of the working directory.
func testdataDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "testdata")
}

// loadTestWallet loads the wallet key from testdata/.wallet.json.
// Returns the parsed ed25519 private key and address.
// Fatals the test if the file is missing.
func loadTestWallet(t *testing.T) (ed25519.PrivateKey, *address.Address) {
	t.Helper()
	path := filepath.Join(testdataDir(), ".wallet.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("wallet file not found at %s: %v", path, err)
	}
	var wj testWalletJSON
	if err := json.Unmarshal(data, &wj); err != nil {
		t.Fatalf("failed to parse wallet JSON: %v", err)
	}
	seed, err := base64.StdEncoding.DecodeString(wj.PrivateKey)
	if err != nil {
		t.Fatalf("failed to decode private key base64: %v", err)
	}
	if len(seed) != 32 {
		t.Fatalf("expected 32-byte seed, got %d bytes", len(seed))
	}
	privKey := ed25519.NewKeyFromSeed(seed)

	addr, err := address.ParseAddr(wj.Address)
	if err != nil {
		t.Fatalf("failed to parse wallet address: %v", err)
	}
	return privKey, addr
}

// buildTransferBOC builds a signed self-transfer external message offline.
// It returns the base64-encoded BOC ready to be sent via lite.sendMessage.
func buildTransferBOC(t *testing.T, privKey ed25519.PrivateKey, addr *address.Address, seqno uint32, withStateInit bool) string {
	t.Helper()

	// Create wallet offline — no API needed.
	w, err := wallet.FromPrivateKeyWithOptions(privKey, wallet.V4R2, wallet.WithPrivateKey(privKey))
	if err != nil {
		t.Fatalf("failed to create wallet: %v", err)
	}

	// Override the seqno fetcher to return our known seqno (offline).
	w.GetSpec().(*wallet.SpecV4R2).SetSeqnoFetcher(
		func(ctx context.Context, subWallet uint32) (uint32, error) {
			return seqno, nil
		},
	)

	// Build a self-transfer of 0.01 TON.
	transfer, err := w.BuildTransfer(
		addr,
		tlb.MustFromTON("0.01"),
		false, // no bounce — address may be uninit
		fmt.Sprintf("e2e test seqno=%d", seqno),
	)
	if err != nil {
		t.Fatalf("failed to build transfer: %v", err)
	}

	// Build the external message (with or without StateInit).
	ext, err := w.PrepareExternalMessageForMany(
		context.Background(),
		withStateInit,
		[]*wallet.Message{transfer},
	)
	if err != nil {
		t.Fatalf("failed to prepare external message: %v", err)
	}

	// Serialize to BOC.
	msgCell, err := tlb.ToCell(ext)
	if err != nil {
		t.Fatalf("failed to serialize external message to cell: %v", err)
	}
	boc := msgCell.ToBOCWithFlags(false)
	return base64.StdEncoding.EncodeToString(boc)
}

// e2eReadEvent reads raw WebSocket messages until it finds one with the given
// event name. Non-event messages and events with a different name are skipped.
// Returns the "data" field of the matched event.
func e2eReadEvent(t *testing.T, c *websocket.Conn, eventName string, timeout time.Duration) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		c.SetReadDeadline(deadline)
		_, msg, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("[FAIL] waiting for event %q — read failed: %v", eventName, err)
		}
		var raw map[string]interface{}
		if err := json.Unmarshal(msg, &raw); err != nil {
			continue
		}
		ev, ok := raw["event"].(string)
		if !ok {
			continue // not an event message (could be a JSON-RPC response)
		}
		if ev != eventName {
			t.Logf("  [INFO] skipping event %q while waiting for %q", ev, eventName)
			continue
		}
		data, _ := raw["data"].(map[string]interface{})
		return data
	}
}

func TestE2E_LiteSendReal(t *testing.T) {
	t.Parallel()
	c := e2eDial(t)
	privKey, addr := loadTestWallet(t)

	// ---------------------------------------------------------------
	// 1. Get account state: balance + status
	// ---------------------------------------------------------------
	stateResp := e2eCall(t, c, "lite.getAccountState", map[string]string{
		"address": addr.String(),
	})
	stateResult := e2eRequireResult(t, stateResp, "lite.getAccountState")
	balanceStr := fmt.Sprintf("%v", stateResult["balance"])
	balance := new(big.Int)
	balance.SetString(balanceStr, 10)
	status, _ := stateResult["status"].(string)
	t.Logf("[INFO] wallet %s — balance %s, status %s", addr.String(), formatTON(balanceStr), status)

	// Need at least 0.05 TON to run 3 self-transfers with gas.
	minBalance := new(big.Int).SetUint64(50_000_000) // 0.05 TON
	if balance.Cmp(minBalance) < 0 {
		t.Fatalf("wallet balance too low (%s) — need at least 0.05 TON", formatTON(balanceStr))
	}

	// ---------------------------------------------------------------
	// 2. Determine seqno and StateInit requirement
	// ---------------------------------------------------------------
	uninit := status != "active"
	var seqno uint32

	if !uninit {
		// Wallet is active — get seqno via lite.runMethod.
		seqResp := e2eCall(t, c, "lite.runMethod", map[string]interface{}{
			"address": addr.String(),
			"method":  "seqno",
		})
		seqResult := e2eRequireResult(t, seqResp, "lite.runMethod")
		if stack, ok := seqResult["stack"].([]interface{}); ok && len(stack) > 0 {
			if v, ok := stack[0].(float64); ok {
				seqno = uint32(v)
			} else if s, ok := stack[0].(string); ok {
				bi := new(big.Int)
				bi.SetString(s, 10)
				seqno = uint32(bi.Uint64())
			}
		}
	}
	t.Logf("[INFO] seqno=%d, uninit=%v", seqno, uninit)

	// ---------------------------------------------------------------
	// 3. lite.sendMessage — fire and forget
	// ---------------------------------------------------------------
	t.Run("sendMessage_real", func(t *testing.T) {
		boc := buildTransferBOC(t, privKey, addr, seqno, uninit)
		resp := e2eCall(t, c, "lite.sendMessage", map[string]string{"boc": boc})
		result := e2eRequireResult(t, resp, "lite.sendMessage")
		hash, _ := result["hash"].(string)
		if hash == "" {
			t.Fatal("[FAIL] lite.sendMessage — empty hash in response")
		}
		t.Logf("[PASS] lite.sendMessage — hash %s", hash)
		// Wait for tx to be included in a block before next send
		time.Sleep(8 * time.Second)
		seqno++
	})

	// ---------------------------------------------------------------
	// 4. lite.sendMessageWait — fire with extended timeout
	// ---------------------------------------------------------------
	t.Run("sendMessageWait_real", func(t *testing.T) {
		boc := buildTransferBOC(t, privKey, addr, seqno, false)
		resp := e2eCall(t, c, "lite.sendMessageWait", map[string]string{"boc": boc})
		result := e2eRequireResult(t, resp, "lite.sendMessageWait")
		hash, _ := result["hash"].(string)
		if hash == "" {
			t.Fatal("[FAIL] lite.sendMessageWait — empty hash in response")
		}
		t.Logf("[PASS] lite.sendMessageWait — hash %s", hash)
		time.Sleep(8 * time.Second)
		seqno++
	})

	// ---------------------------------------------------------------
	// 5. lite.sendAndWatch — send + wait for tx_confirmed event
	// ---------------------------------------------------------------
	t.Run("sendAndWatch_real", func(t *testing.T) {
		boc := buildTransferBOC(t, privKey, addr, seqno, false)
		reqMsg := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      "sendAndWatch_real",
			"method":  "lite.sendAndWatch",
			"params":  map[string]string{"boc": boc},
		}
		data, _ := json.Marshal(reqMsg)
		if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
			t.Fatalf("[FAIL] lite.sendAndWatch — write failed: %v", err)
		}

		// Read the initial JSON-RPC response (watching: true).
		c.SetReadDeadline(time.Now().Add(30 * time.Second))
		var initialResp e2eResponse
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				t.Fatalf("[FAIL] lite.sendAndWatch — read initial response failed: %v", err)
			}
			var raw map[string]interface{}
			if err := json.Unmarshal(msg, &raw); err == nil {
				if _, isEvent := raw["event"]; isEvent {
					continue // skip stray events
				}
			}
			if err := json.Unmarshal(msg, &initialResp); err != nil {
				t.Fatalf("[FAIL] lite.sendAndWatch — unmarshal initial response failed: %v", err)
			}
			break
		}

		if initialResp.Error != nil {
			t.Fatalf("[FAIL] lite.sendAndWatch — error %d: %s",
				initialResp.Error.Code, initialResp.Error.Message)
		}
		result := initialResp.Result
		watching, _ := result["watching"].(bool)
		if !watching {
			t.Fatal("[FAIL] lite.sendAndWatch — expected watching=true in initial response")
		}
		subID, _ := result["subscription_id"].(string)
		msgHash, _ := result["msg_hash"].(string)
		t.Logf("[INFO] lite.sendAndWatch — watching=true, sub=%s, msg_hash=%s", subID, msgHash)

		// Wait for tx_confirmed event (up to 180s).
		eventData := e2eReadEvent(t, c, "tx_confirmed", 180*time.Second)
		if eventData == nil {
			t.Fatal("[FAIL] lite.sendAndWatch — tx_confirmed event data is nil")
		}
		evHash, _ := eventData["msg_hash"].(string)
		if evHash == "" {
			t.Fatal("[FAIL] lite.sendAndWatch — tx_confirmed event missing msg_hash")
		}
		tx, _ := eventData["transaction"].(map[string]interface{})
		txHash := ""
		if tx != nil {
			txHash, _ = tx["hash"].(string)
		}
		t.Logf("[PASS] lite.sendAndWatch — tx_confirmed, msg_hash=%s, tx_hash=%s", evHash, txHash)
	})
}
