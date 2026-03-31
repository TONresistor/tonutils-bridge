package wsbridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

func (b *WSBridge) handleGetMasterchainInfo(client *wsClient, req *WSRequest) {
	ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
	defer cancel()

	block, err := b.api.CurrentMasterchainInfo(ctx)
	if err != nil {
		b.sendError(client, req.ID, "failed to get masterchain info: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]any{
		"seqno":     block.SeqNo,
		"workchain": block.Workchain,
		"shard":     fmt.Sprintf("%016x", uint64(block.Shard)),
		"root_hash": hex.EncodeToString(block.RootHash),
		"file_hash": hex.EncodeToString(block.FileHash),
	})
}

func (b *WSBridge) handleGetAccountState(client *wsClient, req *WSRequest) {
	var params struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	addr, err := parseAddress(params.Address)
	if err != nil {
		b.sendError(client, req.ID, "invalid address: "+err.Error(), -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
	defer cancel()

	block, err := b.api.CurrentMasterchainInfo(ctx)
	if err != nil {
		b.sendError(client, req.ID, "failed to get masterchain info: "+err.Error())
		return
	}

	acc, err := b.api.GetAccount(ctx, block, addr)
	if err != nil {
		b.sendError(client, req.ID, "failed to get account: "+err.Error())
		return
	}

	var status string
	if acc.State == nil || !acc.State.IsValid {
		status = "uninit"
	} else {
		switch string(acc.State.Status) {
		case "ACTIVE":
			status = "active"
		case "FROZEN":
			status = "frozen"
		default:
			status = "uninit"
		}
	}

	result := map[string]any{
		"status":       status,
		"last_tx_lt":   fmt.Sprintf("%d", acc.LastTxLT),
		"last_tx_hash": hex.EncodeToString(acc.LastTxHash),
		"has_code":     acc.Code != nil,
		"has_data":     acc.Data != nil,
	}

	if acc.State != nil && acc.State.IsValid {
		result["balance"] = acc.State.Balance.Nano().String()
	} else {
		result["balance"] = "0"
	}

	if acc.Code != nil {
		boc := acc.Code.ToBOCWithFlags(false)
		result["code"] = base64.StdEncoding.EncodeToString(boc)
	}

	if acc.Data != nil {
		boc := acc.Data.ToBOCWithFlags(false)
		result["data"] = base64.StdEncoding.EncodeToString(boc)
	}

	b.sendResult(client, req.ID, result)
}

func (b *WSBridge) handleRunMethod(client *wsClient, req *WSRequest) {
	var params struct {
		Address string        `json:"address"`
		Method  string        `json:"method"`
		Params  []any `json:"params"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	addr, err := parseAddress(params.Address)
	if err != nil {
		b.sendError(client, req.ID, "invalid address: "+err.Error(), -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
	defer cancel()

	block, err := b.api.CurrentMasterchainInfo(ctx)
	if err != nil {
		b.sendError(client, req.ID, "failed to get masterchain info: "+err.Error())
		return
	}

	// Convert JSON params to RunGetMethod args
	var methodParams []any
	for i, p := range params.Params {
		switch v := p.(type) {
		case float64:
			methodParams = append(methodParams, new(big.Int).SetInt64(int64(v)))
		case string:
			// Try to parse as big.Int
			bi := new(big.Int)
			if _, ok := bi.SetString(v, 10); ok {
				methodParams = append(methodParams, bi)
			} else {
				b.sendError(client, req.ID, fmt.Sprintf("unsupported param at index %d: string is not a valid integer", i), -32602)
				return
			}
		default:
			b.sendError(client, req.ID, fmt.Sprintf("unsupported param type at index %d", i), -32602)
			return
		}
	}

	res, err := b.api.RunGetMethod(ctx, block, addr, params.Method, methodParams...)
	if err != nil {
		b.sendError(client, req.ID, "run method failed: "+err.Error())
		return
	}

	stack := serializeStack(res.AsTuple())

	// RunGetMethod returns an error for exit codes other than 0 and 1,
	// and ExecutionResult does not expose the actual code. Report 0 for success.
	b.sendResult(client, req.ID, map[string]any{
		"exit_code": 0,
		"stack":     stack,
	})
}

func (b *WSBridge) handleSendMessage(client *wsClient, req *WSRequest) {
	var params struct {
		BOC string `json:"boc"` // base64-encoded BOC
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	bocBytes, err := decodeBase64(params.BOC)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 boc: "+err.Error(), -32602)
		return
	}

	// Parse the cell to get the hash
	c, err := cell.FromBOC(bocBytes)
	if err != nil {
		b.sendError(client, req.ID, "invalid BOC: "+err.Error(), -32602)
		return
	}
	msgHash := c.Hash()

	ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
	defer cancel()

	var resp tl.Serializable
	err = b.api.Client().QueryLiteserver(ctx, ton.SendMessage{Body: bocBytes}, &resp)
	if err != nil {
		b.sendError(client, req.ID, "send message failed: "+err.Error())
		return
	}

	var status int32
	if s, ok := resp.(ton.SendMessageStatus); ok {
		status = s.Status
	}

	b.sendResult(client, req.ID, map[string]any{
		"hash":   hex.EncodeToString(msgHash),
		"status": status,
	})
}

// handleSendMessageWait sends a message with a longer timeout (60s). Despite the name,
// it does NOT wait for on-chain confirmation — polling for transaction inclusion is the
// client's responsibility. The extended timeout only covers the liteserver round-trip.
func (b *WSBridge) handleSendMessageWait(client *wsClient, req *WSRequest) {
	var params struct {
		BOC string `json:"boc"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	bocBytes, err := decodeBase64(params.BOC)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 boc: "+err.Error(), -32602)
		return
	}

	c, err := cell.FromBOC(bocBytes)
	if err != nil {
		b.sendError(client, req.ID, "invalid BOC: "+err.Error(), -32602)
		return
	}
	msgHash := c.Hash()

	ctx, cancel := context.WithTimeout(client.ctx, 60*time.Second)
	defer cancel()

	var resp tl.Serializable
	err = b.api.Client().QueryLiteserver(ctx, ton.SendMessage{Body: bocBytes}, &resp)
	if err != nil {
		b.sendError(client, req.ID, "send message failed: "+err.Error())
		return
	}

	var status int32
	if s, ok := resp.(ton.SendMessageStatus); ok {
		status = s.Status
	}

	b.sendResult(client, req.ID, map[string]any{
		"hash":   hex.EncodeToString(msgHash),
		"status": status,
	})
}

func (b *WSBridge) handleGetTransactions(client *wsClient, req *WSRequest) {
	var params struct {
		Address  string `json:"address"`
		Limit    uint32 `json:"limit"`
		LastLT   string `json:"last_lt"`   // optional, for pagination
		LastHash string `json:"last_hash"` // optional, hex-encoded
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	if params.Limit == 0 || params.Limit > 100 {
		params.Limit = 100
	}

	addr, err := parseAddress(params.Address)
	if err != nil {
		b.sendError(client, req.ID, "invalid address: "+err.Error(), -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
	defer cancel()

	var startLT uint64
	var startHash []byte

	if params.LastLT != "" && params.LastHash != "" {
		startLT, err = strconv.ParseUint(params.LastLT, 10, 64)
		if err != nil {
			b.sendError(client, req.ID, "invalid last_lt: "+err.Error(), -32602)
			return
		}
		startHash, err = hex.DecodeString(params.LastHash)
		if err != nil {
			b.sendError(client, req.ID, "invalid last_hash hex: "+err.Error(), -32602)
			return
		}
	} else {
		block, err := b.api.CurrentMasterchainInfo(ctx)
		if err != nil {
			b.sendError(client, req.ID, "failed to get masterchain info: "+err.Error())
			return
		}

		acc, err := b.api.GetAccount(ctx, block, addr)
		if err != nil {
			b.sendError(client, req.ID, "failed to get account: "+err.Error())
			return
		}

		if acc.LastTxLT == 0 {
			b.sendResult(client, req.ID, map[string]any{
				"transactions": []any{},
			})
			return
		}

		startLT = acc.LastTxLT
		startHash = acc.LastTxHash
	}

	txList, err := b.api.ListTransactions(ctx, addr, params.Limit, startLT, startHash)
	if err != nil {
		b.sendError(client, req.ID, "failed to list transactions: "+err.Error())
		return
	}

	var txResults []map[string]any
	for _, tx := range txList {
		txResults = append(txResults, serializeTransaction(tx))
	}

	if txResults == nil {
		txResults = []map[string]any{}
	}

	b.sendResult(client, req.ID, map[string]any{
		"transactions": txResults,
	})
}

func (b *WSBridge) handleGetTime(client *wsClient, req *WSRequest) {
	ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
	defer cancel()

	t, err := b.api.GetTime(ctx)
	if err != nil {
		b.sendError(client, req.ID, "failed to get time: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]any{
		"time": t,
	})
}

func (b *WSBridge) handleLookupBlock(client *wsClient, req *WSRequest) {
	var params struct {
		Workchain int32  `json:"workchain"`
		Shard     string `json:"shard"`
		Seqno     uint32 `json:"seqno"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	shardU, err := strconv.ParseUint(params.Shard, 16, 64)
	shard := int64(shardU)
	if err != nil {
		b.sendError(client, req.ID, "invalid shard hex: "+err.Error(), -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
	defer cancel()

	block, err := b.api.LookupBlock(ctx, params.Workchain, shard, params.Seqno)
	if err != nil {
		b.sendError(client, req.ID, "lookup block failed: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]any{
		"workchain": block.Workchain,
		"shard":     fmt.Sprintf("%016x", uint64(block.Shard)),
		"seqno":     block.SeqNo,
		"root_hash": hex.EncodeToString(block.RootHash),
		"file_hash": hex.EncodeToString(block.FileHash),
	})
}

func (b *WSBridge) handleGetBlockTransactions(client *wsClient, req *WSRequest) {
	var params struct {
		Workchain int32  `json:"workchain"`
		Shard     string `json:"shard"`
		Seqno     uint32 `json:"seqno"`
		Count     uint32 `json:"count"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	if params.Count == 0 || params.Count > 256 {
		params.Count = 256
	}

	shardU, err := strconv.ParseUint(params.Shard, 16, 64)
	shard := int64(shardU)
	if err != nil {
		b.sendError(client, req.ID, "invalid shard hex: "+err.Error(), -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
	defer cancel()

	block, err := b.api.LookupBlock(ctx, params.Workchain, shard, params.Seqno)
	if err != nil {
		b.sendError(client, req.ID, "lookup block failed: "+err.Error())
		return
	}

	txList, incomplete, err := b.api.GetBlockTransactionsV2(ctx, block, params.Count)
	if err != nil {
		b.sendError(client, req.ID, "get block transactions failed: "+err.Error())
		return
	}

	var txResults []map[string]any
	for _, tx := range txList {
		txResults = append(txResults, map[string]any{
			"account": hex.EncodeToString(tx.Account),
			"lt":      fmt.Sprintf("%d", tx.LT),
			"hash":    hex.EncodeToString(tx.Hash),
		})
	}

	if txResults == nil {
		txResults = []map[string]any{}
	}

	b.sendResult(client, req.ID, map[string]any{
		"transactions": txResults,
		"incomplete":   incomplete,
	})
}

func (b *WSBridge) handleGetShards(client *wsClient, req *WSRequest) {
	ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
	defer cancel()

	master, err := b.api.CurrentMasterchainInfo(ctx)
	if err != nil {
		b.sendError(client, req.ID, "failed to get masterchain info: "+err.Error())
		return
	}

	shards, err := b.api.GetBlockShardsInfo(ctx, master)
	if err != nil {
		b.sendError(client, req.ID, "failed to get shards info: "+err.Error())
		return
	}

	var shardResults []map[string]any
	for _, s := range shards {
		shardResults = append(shardResults, map[string]any{
			"workchain": s.Workchain,
			"shard":     fmt.Sprintf("%016x", uint64(s.Shard)),
			"seqno":     s.SeqNo,
		})
	}

	if shardResults == nil {
		shardResults = []map[string]any{}
	}

	b.sendResult(client, req.ID, map[string]any{
		"shards": shardResults,
	})
}

func (b *WSBridge) handleGetBlockchainConfig(client *wsClient, req *WSRequest) {
	var params struct {
		Params []int32 `json:"params"`
	}
	// params is optional — if missing or empty, get all
	if req.Params != nil {
		_ = json.Unmarshal(req.Params, &params)
	}

	ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
	defer cancel()

	block, err := b.api.CurrentMasterchainInfo(ctx)
	if err != nil {
		b.sendError(client, req.ID, "failed to get masterchain info: "+err.Error())
		return
	}

	cfg, err := b.api.GetBlockchainConfig(ctx, block, params.Params...)
	if err != nil {
		b.sendError(client, req.ID, "failed to get blockchain config: "+err.Error())
		return
	}

	result := map[string]any{}
	if len(params.Params) > 0 {
		for _, id := range params.Params {
			c := cfg.Get(id)
			if c != nil {
				boc := c.ToBOCWithFlags(false)
				result[fmt.Sprintf("%d", id)] = base64.StdEncoding.EncodeToString(boc)
			} else {
				result[fmt.Sprintf("%d", id)] = nil
			}
		}
	} else {
		for id, c := range cfg.All() {
			boc := c.ToBOCWithFlags(false)
			result[fmt.Sprintf("%d", id)] = base64.StdEncoding.EncodeToString(boc)
		}
	}

	b.sendResult(client, req.ID, map[string]any{
		"params": result,
	})
}

func (b *WSBridge) handleGetTransaction(client *wsClient, req *WSRequest) {
	var params struct {
		Address string `json:"address"`
		LT      string `json:"lt"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	addr, err := parseAddress(params.Address)
	if err != nil {
		b.sendError(client, req.ID, "invalid address: "+err.Error(), -32602)
		return
	}

	lt, err := strconv.ParseUint(params.LT, 10, 64)
	if err != nil {
		b.sendError(client, req.ID, "invalid lt: "+err.Error(), -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
	defer cancel()

	block, err := b.api.CurrentMasterchainInfo(ctx)
	if err != nil {
		b.sendError(client, req.ID, "failed to get masterchain info: "+err.Error())
		return
	}

	// ListTransactions walks backward from the account's latest tx.
	// We fetch enough transactions to find the one with the matching LT.
	acc, err := b.api.GetAccount(ctx, block, addr)
	if err != nil {
		b.sendError(client, req.ID, "failed to get account: "+err.Error())
		return
	}

	// Paginate backward from the latest tx (max 10 pages × 20 = 200 txs)
	currentLT := acc.LastTxLT
	currentHash := acc.LastTxHash

	for page := 0; page < 10 && currentLT != 0; page++ {
		txList, listErr := b.api.ListTransactions(ctx, addr, 20, currentLT, currentHash)
		if listErr != nil {
			if page == 0 {
				b.sendError(client, req.ID, "failed to list transactions: "+listErr.Error())
				return
			}
			break
		}
		for _, tx := range txList {
			if tx.LT == lt {
				b.sendResult(client, req.ID, serializeTransaction(tx))
				return
			}
		}
		if len(txList) == 0 {
			break
		}
		// txList[0] is the oldest; its PrevTxLT points to the next (older) page
		oldest := txList[0]
		currentLT = oldest.PrevTxLT
		currentHash = oldest.PrevTxHash
	}

	b.sendError(client, req.ID, "transaction not found for lt "+params.LT)
}


func (b *WSBridge) handleFindTxByInMsgHash(client *wsClient, req *WSRequest) {
	var params struct {
		Address string `json:"address"`
		MsgHash string `json:"msg_hash"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	addr, err := parseAddress(params.Address)
	if err != nil {
		b.sendError(client, req.ID, "invalid address: "+err.Error(), -32602)
		return
	}

	hashBytes, err := hex.DecodeString(params.MsgHash)
	if err != nil {
		b.sendError(client, req.ID, "invalid msg_hash hex: "+err.Error(), -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
	defer cancel()

	tx, err := b.api.FindLastTransactionByInMsgHash(ctx, addr, hashBytes)
	if err != nil {
		b.sendError(client, req.ID, "find tx by in msg hash failed: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, serializeTransaction(tx))
}

func (b *WSBridge) handleFindTxByOutMsgHash(client *wsClient, req *WSRequest) {
	var params struct {
		Address string `json:"address"`
		MsgHash string `json:"msg_hash"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	addr, err := parseAddress(params.Address)
	if err != nil {
		b.sendError(client, req.ID, "invalid address: "+err.Error(), -32602)
		return
	}

	hashBytes, err := hex.DecodeString(params.MsgHash)
	if err != nil {
		b.sendError(client, req.ID, "invalid msg_hash hex: "+err.Error(), -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
	defer cancel()

	tx, err := b.api.FindLastTransactionByOutMsgHash(ctx, addr, hashBytes)
	if err != nil {
		b.sendError(client, req.ID, "find tx by out msg hash failed: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, serializeTransaction(tx))
}

func (b *WSBridge) handleGetBlockData(client *wsClient, req *WSRequest) {
	var params struct {
		Workchain int32  `json:"workchain"`
		Shard     string `json:"shard"`
		Seqno     uint32 `json:"seqno"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	shardU, err := strconv.ParseUint(params.Shard, 16, 64)
	if err != nil {
		b.sendError(client, req.ID, "invalid shard hex: "+err.Error(), -32602)
		return
	}
	shard := int64(shardU)

	ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
	defer cancel()

	block, err := b.api.LookupBlock(ctx, params.Workchain, shard, params.Seqno)
	if err != nil {
		b.sendError(client, req.ID, "lookup block failed: "+err.Error())
		return
	}

	var resp tl.Serializable
	if err = b.api.Client().QueryLiteserver(ctx, ton.GetBlockData{ID: block}, &resp); err != nil {
		b.sendError(client, req.ID, "get block data failed: "+err.Error())
		return
	}

	bd, ok := resp.(ton.BlockData)
	if !ok {
		b.sendError(client, req.ID, "unexpected response type from liteserver")
		return
	}

	cl, err := cell.FromBOC(bd.Payload)
	if err != nil {
		b.sendError(client, req.ID, "failed to parse block BOC: "+err.Error())
		return
	}

	boc := cl.ToBOCWithFlags(false)
	b.sendResult(client, req.ID, map[string]any{
		"boc": base64.StdEncoding.EncodeToString(boc),
	})
}

func (b *WSBridge) handleGetBlockHeader(client *wsClient, req *WSRequest) {
	var params struct {
		Workchain int32  `json:"workchain"`
		Shard     string `json:"shard"`
		Seqno     uint32 `json:"seqno"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	shardU, err := strconv.ParseUint(params.Shard, 16, 64)
	if err != nil {
		b.sendError(client, req.ID, "invalid shard hex: "+err.Error(), -32602)
		return
	}
	shard := int64(shardU)

	ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
	defer cancel()

	block, err := b.api.LookupBlock(ctx, params.Workchain, shard, params.Seqno)
	if err != nil {
		b.sendError(client, req.ID, "lookup block failed: "+err.Error())
		return
	}

	var resp tl.Serializable
	if err = b.api.Client().QueryLiteserver(ctx, ton.GetBlockHeader{ID: block, Mode: 0}, &resp); err != nil {
		b.sendError(client, req.ID, "get block header failed: "+err.Error())
		return
	}

	bh, ok := resp.(ton.BlockHeader)
	if !ok {
		b.sendError(client, req.ID, "unexpected response type from liteserver")
		return
	}

	// Parse the header proof to extract block info fields
	proofCell, err := cell.FromBOC(bh.HeaderProof)
	if err != nil {
		b.sendError(client, req.ID, "failed to parse header proof BOC: "+err.Error())
		return
	}

	proofBOC := base64.StdEncoding.EncodeToString(proofCell.ToBOCWithFlags(false))

	b.sendResult(client, req.ID, map[string]any{
		"workchain":  bh.ID.Workchain,
		"shard":      fmt.Sprintf("%016x", uint64(bh.ID.Shard)),
		"seqno":      bh.ID.SeqNo,
		"root_hash":  hex.EncodeToString(bh.ID.RootHash),
		"file_hash":  hex.EncodeToString(bh.ID.FileHash),
		"header_boc": proofBOC,
	})
}

func (b *WSBridge) handleGetLibraries(client *wsClient, req *WSRequest) {
	var params struct {
		Hashes []string `json:"hashes"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	if len(params.Hashes) == 0 {
		b.sendResult(client, req.ID, map[string]any{
			"libraries": []any{},
		})
		return
	}
	if len(params.Hashes) > 256 {
		b.sendError(client, req.ID, "too many hashes (max 256)", -32602)
		return
	}

	hashBytes := make([][]byte, len(params.Hashes))
	for i, h := range params.Hashes {
		decoded, err := hex.DecodeString(h)
		if err != nil {
			b.sendError(client, req.ID, fmt.Sprintf("invalid hash at index %d: %s", i, err.Error()), -32602)
			return
		}
		hashBytes[i] = decoded
	}

	ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
	defer cancel()

	cells, err := b.api.GetLibraries(ctx, hashBytes...)
	if err != nil {
		b.sendError(client, req.ID, "get libraries failed: "+err.Error())
		return
	}

	libraries := make([]any, len(cells))
	for i, c := range cells {
		if c == nil {
			libraries[i] = nil
		} else {
			boc := c.ToBOCWithFlags(false)
			libraries[i] = map[string]any{
				"hash": params.Hashes[i],
				"boc":  base64.StdEncoding.EncodeToString(boc),
			}
		}
	}

	b.sendResult(client, req.ID, map[string]any{
		"libraries": libraries,
	})
}

func (b *WSBridge) handleSendAndWatch(client *wsClient, req *WSRequest) {
	// 1. Count as a subscription (uses the same atomic limit)
	if atomic.AddInt32(&client.activeSubs, 1) > 50 {
		atomic.AddInt32(&client.activeSubs, -1)
		b.sendError(client, req.ID, "too many subscriptions (max 50)", -32602)
		return
	}
	defer atomic.AddInt32(&client.activeSubs, -1)

	var params struct {
		BOC string `json:"boc"` // base64-encoded external message BOC
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	bocBytes, err := decodeBase64(params.BOC)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 boc: "+err.Error(), -32602)
		return
	}

	// Parse the BOC to get the external message cell
	msgCell, err := cell.FromBOC(bocBytes)
	if err != nil {
		b.sendError(client, req.ID, "invalid BOC: "+err.Error(), -32602)
		return
	}

	// Parse the external message to extract destination address and body hash
	// An ExternalIn message structure:
	//   ext_in_msg_info$10 src:MsgAddressExt dest:MsgAddressInt import_fee:Grams = CommonMsgInfo;
	// We need to parse the cell to extract dest address and body
	var extMsg tlb.ExternalMessage
	if err := tlb.LoadFromCell(&extMsg, msgCell.BeginParse()); err != nil {
		b.sendError(client, req.ID, "failed to parse external message: "+err.Error())
		return
	}

	destAddr := extMsg.DstAddr
	// Hash the body cell for matching
	bodyHash := extMsg.Body.Hash()
	msgHash := msgCell.Hash()

	// Create a context with 180s timeout, cancellable by client disconnect
	ctx, cancel := context.WithTimeout(client.ctx, 180*time.Second)
	defer cancel()

	// Generate subscription ID for tracking
	subID := fmt.Sprintf("sub_%d", atomic.AddUint64(&client.subCounter, 1))
	client.subscriptionsMu.Lock()
	client.subscriptions[subID] = cancel
	client.subscriptionsMu.Unlock()
	defer func() {
		client.subscriptionsMu.Lock()
		delete(client.subscriptions, subID)
		client.subscriptionsMu.Unlock()
	}()

	// Send the BOC to the network
	var resp tl.Serializable
	sendCtx, sendCancel := context.WithTimeout(ctx, 30*time.Second)
	err = b.api.Client().QueryLiteserver(sendCtx, ton.SendMessage{Body: bocBytes}, &resp)
	sendCancel()
	if err != nil {
		b.sendError(client, req.ID, "send message failed: "+err.Error())
		return
	}

	// Send immediate confirmation with subscription ID
	b.sendResult(client, req.ID, map[string]any{
		"watching":        true,
		"subscription_id": subID,
		"msg_hash":        hex.EncodeToString(msgHash),
	})

	// Get starting block
	block, err := b.api.CurrentMasterchainInfo(ctx)
	if err != nil {
		b.sendEvent(client, "tx_timeout", map[string]any{
			"msg_hash": hex.EncodeToString(msgHash),
			"reason":   "failed to get masterchain info",
		})
		return
	}

	// Get initial account state for comparison
	acc, err := b.api.GetAccount(ctx, block, destAddr)
	if err != nil {
		b.sendEvent(client, "tx_timeout", map[string]any{
			"msg_hash": hex.EncodeToString(msgHash),
			"reason":   "failed to get account state",
		})
		return
	}
	lastLT := acc.LastTxLT

	// Poll blocks until we find the matching transaction
	for {
		select {
		case <-ctx.Done():
			b.sendEvent(client, "tx_timeout", map[string]any{
				"msg_hash": hex.EncodeToString(msgHash),
				"reason":   "timeout",
			})
			return
		default:
		}

		newBlock, err := b.api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
		if err != nil {
			if ctx.Err() != nil {
				b.sendEvent(client, "tx_timeout", map[string]any{
					"msg_hash": hex.EncodeToString(msgHash),
					"reason":   "timeout",
				})
				return
			}
			time.Sleep(time.Second)
			continue
		}
		block = newBlock

		// Check if account's LastTxLT changed
		newAcc, err := b.api.GetAccount(ctx, block, destAddr)
		if err != nil {
			continue
		}

		if newAcc.LastTxLT == lastLT {
			continue // no new transactions
		}

		// New transactions! Scan them for our message
		txList, err := b.api.ListTransactions(ctx, destAddr, 10, newAcc.LastTxLT, newAcc.LastTxHash)
		if err != nil {
			lastLT = newAcc.LastTxLT
			continue
		}

		for _, tx := range txList {
			if tx.IO.In == nil {
				continue
			}
			// Check if this is an external in message with matching body hash
			inMsg := tx.IO.In.Msg
			if inMsg.Payload() == nil {
				continue
			}
			if bytes.Equal(inMsg.Payload().Hash(), bodyHash) {
				// Found it!
				b.sendEvent(client, "tx_confirmed", map[string]any{
					"msg_hash":    hex.EncodeToString(msgHash),
					"transaction": serializeTransaction(tx),
					"block": map[string]any{
						"seqno":     block.SeqNo,
						"workchain": block.Workchain,
						"shard":     fmt.Sprintf("%016x", uint64(block.Shard)),
					},
				})
				return
			}
		}

		lastLT = newAcc.LastTxLT
	}
}

