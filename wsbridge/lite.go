package wsbridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/tvm"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

func (b *WSBridge) handleGetMasterchainInfo(client *wsClient, req *WSRequest) {
	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.Timeout)
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

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.Timeout)
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
		Address string `json:"address"`
		Method  string `json:"method"`
		Params  []any  `json:"params"`
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

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.Timeout)
	defer cancel()

	block, err := b.api.CurrentMasterchainInfo(ctx)
	if err != nil {
		b.sendError(client, req.ID, "failed to get masterchain info: "+err.Error())
		return
	}

	// Convert JSON params to RunGetMethod args
	var methodParams []any
	for i, p := range params.Params {
		converted, err := convertRunMethodParam(p)
		if err != nil {
			b.sendError(client, req.ID, fmt.Sprintf("unsupported param at index %d: %s", i, err.Error()), -32602)
			return
		}
		methodParams = append(methodParams, converted)
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

func convertRunMethodParam(value any) (any, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case float64:
		if math.Trunc(v) != v || v > 1<<53 || v < -(1<<53) {
			return nil, fmt.Errorf("JSON number must be a safe integer; use a decimal string for larger values")
		}
		return new(big.Int).SetInt64(int64(v)), nil
	case string:
		bi := new(big.Int)
		if _, ok := bi.SetString(v, 10); !ok {
			return nil, fmt.Errorf("string is not a valid decimal integer")
		}
		return bi, nil
	case []any:
		items := make([]any, len(v))
		for i, item := range v {
			converted, err := convertRunMethodParam(item)
			if err != nil {
				return nil, fmt.Errorf("tuple item %d: %w", i, err)
			}
			items[i] = converted
		}
		return items, nil
	case map[string]any:
		kind, _ := v["type"].(string)
		bocB64, _ := v["boc"].(string)
		if bocB64 == "" || (kind != "slice" && kind != "cell") {
			return nil, fmt.Errorf("expected {type:'slice'|'cell', boc:<base64>}")
		}
		bocBytes, err := decodeBase64(bocB64)
		if err != nil {
			return nil, fmt.Errorf("invalid base64 BOC: %w", err)
		}
		c, err := cell.FromBOC(bocBytes)
		if err != nil {
			return nil, fmt.Errorf("invalid BOC: %w", err)
		}
		if kind == "cell" {
			return c, nil
		}
		slice, err := c.BeginParse()
		if err != nil {
			return nil, fmt.Errorf("invalid slice: %w", err)
		}
		return slice, nil
	default:
		return nil, fmt.Errorf("unsupported type %T", value)
	}
}

// handleEmulateMessage runs a message locally against the target account's real
// on-chain state using the native Go TVM (tonutils-go v1.17+), without
// broadcasting it. Useful as a dry-run before lite.sendMessage: it reports
// whether the message is accepted, its exit code, gas usage and emitted
// messages.
//
// The TVM emulator is marked alpha upstream; results may differ from real
// on-chain execution in edge cases.
func (b *WSBridge) handleEmulateMessage(client *wsClient, req *WSRequest) {
	var params struct {
		Address string `json:"address"`
		BOC     string `json:"boc"`
		Type    string `json:"type"`   // "external" (default) | "internal"
		Amount  string `json:"amount"` // nano-TON, required when type=internal
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	msgType := params.Type
	if msgType == "" {
		msgType = "external"
	}
	if msgType != "external" && msgType != "internal" {
		b.sendError(client, req.ID, "invalid type: expected 'external' or 'internal'", -32602)
		return
	}

	addr, err := parseAddress(params.Address)
	if err != nil {
		b.sendError(client, req.ID, "invalid address: "+err.Error(), -32602)
		return
	}

	bocBytes, err := decodeBase64(params.BOC)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 boc: "+err.Error(), -32602)
		return
	}
	msgCell, err := cell.FromBOC(bocBytes)
	if err != nil {
		b.sendError(client, req.ID, "invalid BOC: "+err.Error(), -32602)
		return
	}

	var amount uint64
	if msgType == "internal" {
		amount, err = strconv.ParseUint(params.Amount, 10, 64)
		if err != nil {
			b.sendError(client, req.ID, "invalid amount: expected nano-TON integer string", -32602)
			return
		}
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.Timeout)
	defer cancel()

	block, err := b.api.CurrentMasterchainInfo(ctx)
	if err != nil {
		b.sendError(client, req.ID, "failed to get masterchain info: "+err.Error())
		return
	}
	networkNow, err := b.api.GetTime(ctx)
	if err != nil {
		b.sendError(client, req.ID, "failed to get network time: "+err.Error())
		return
	}

	acc, err := b.api.GetAccount(ctx, block, addr)
	if err != nil {
		b.sendError(client, req.ID, "failed to get account: "+err.Error())
		return
	}
	if acc.Code == nil || acc.Data == nil {
		b.sendError(client, req.ID, "account is not initialized, cannot emulate", -32602)
		return
	}

	bcCfg, err := b.api.GetBlockchainConfig(ctx, block)
	if err != nil {
		b.sendError(client, req.ID, "failed to get blockchain config: "+err.Error())
		return
	}
	preparedCfg, err := tvm.PrepareBlockchainConfig(bcCfg.Root)
	if err != nil {
		b.sendError(client, req.ID, "failed to prepare blockchain config: "+err.Error())
		return
	}

	balance := big.NewInt(0)
	if acc.State != nil && acc.State.IsValid {
		balance = acc.State.Balance.Nano()
	}

	emCfg := tvm.MessageEmulationConfig{
		Address:  addr,
		Now:      networkNow,
		Balance:  balance,
		Config:   preparedCfg,
		RandSeed: make([]byte, 32),
	}

	machine := tvm.NewTVM()

	var res *tvm.MessageExecutionResult
	if msgType == "external" {
		sl, bpErr := msgCell.BeginParse()
		if bpErr != nil {
			b.sendError(client, req.ID, "invalid external message BOC: "+bpErr.Error(), -32602)
			return
		}
		var extMsg tlb.ExternalMessage
		if lErr := tlb.LoadFromCell(&extMsg, sl); lErr != nil {
			b.sendError(client, req.ID, "failed to parse external message: "+lErr.Error(), -32602)
			return
		}
		res, err = machine.EmulateExternalMessage(acc.Code, acc.Data, &extMsg, emCfg)
	} else {
		res, err = machine.EmulateInternalMessage(acc.Code, acc.Data, msgCell, amount, emCfg)
	}
	if err != nil {
		b.sendError(client, req.ID, "emulation failed: "+err.Error())
		return
	}

	result := map[string]any{
		"accepted":     res.Accepted,
		"exit_code":    res.ExitCode,
		"gas_used":     res.GasUsed,
		"steps":        res.Steps,
		"committed":    res.Committed,
		"new_data":     nil,
		"actions":      nil,
		"out_messages": []any{},
	}
	if res.Committed && res.Data != nil {
		result["new_data"] = base64.StdEncoding.EncodeToString(res.Data.ToBOCWithFlags(false))
	}
	if res.Actions != nil {
		result["actions"] = base64.StdEncoding.EncodeToString(res.Actions.ToBOCWithFlags(false))
		result["out_messages"] = parseOutActions(res.Actions)
	}

	b.sendResult(client, req.ID, result)
}

// handleEmulateTransaction runs a FULL transaction locally against the target
// account's real on-chain state using the native Go TVM, without broadcasting.
// Unlike lite.emulateMessage (compute-phase only), it executes every phase
// (storage, credit, compute, action) and reports the emulated fee breakdown and
// total fees — a preflight before lite.sendMessage.
//
// `boc` must be a FULL message cell (an external-in message, as passed to
// lite.sendMessage; or a full internal message). The account must exist, but it
// may still be uninitialized when the message carries its StateInit.
// The TVM emulator is alpha upstream; results may differ in edge cases.
type emulateTransactionParams struct {
	Address      string `json:"address"`
	BOC          string `json:"boc"`
	IgnoreChkSig bool   `json:"ignore_chksig"`
}

func parseEmulateTransactionParams(raw json.RawMessage) (emulateTransactionParams, error) {
	var params emulateTransactionParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return emulateTransactionParams{}, err
	}
	return params, nil
}

func (p emulateTransactionParams) transactionOptions() tvm.TransactionOptions {
	return tvm.TransactionOptions{SignatureCheckAlwaysSucceed: p.IgnoreChkSig}
}

func accountStateForTransactionEmulation(acc *tlb.Account) (*tlb.AccountState, error) {
	if acc == nil || acc.State == nil || !acc.State.IsValid {
		return nil, fmt.Errorf("account does not exist, cannot emulate")
	}
	return acc.State, nil
}

func (b *WSBridge) handleEmulateTransaction(client *wsClient, req *WSRequest) {
	params, err := parseEmulateTransactionParams(req.Params)
	if err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	addr, err := parseAddress(params.Address)
	if err != nil {
		b.sendError(client, req.ID, "invalid address: "+err.Error(), -32602)
		return
	}

	bocBytes, err := decodeBase64(params.BOC)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 boc: "+err.Error(), -32602)
		return
	}
	msgCell, err := cell.FromBOC(bocBytes)
	if err != nil {
		b.sendError(client, req.ID, "invalid BOC: "+err.Error(), -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.Timeout)
	defer cancel()

	block, err := b.api.CurrentMasterchainInfo(ctx)
	if err != nil {
		b.sendError(client, req.ID, "failed to get masterchain info: "+err.Error())
		return
	}
	networkNow, err := b.api.GetTime(ctx)
	if err != nil {
		b.sendError(client, req.ID, "failed to get network time: "+err.Error())
		return
	}

	acc, err := b.api.GetAccount(ctx, block, addr)
	if err != nil {
		b.sendError(client, req.ID, "failed to get account: "+err.Error())
		return
	}
	accountState, err := accountStateForTransactionEmulation(acc)
	if err != nil {
		b.sendError(client, req.ID, err.Error(), -32602)
		return
	}

	accountCell, err := accountState.ToCell()
	if err != nil {
		b.sendError(client, req.ID, "failed to serialize verified account state: "+err.Error())
		return
	}

	shard := &tlb.ShardAccount{
		Account:       accountCell,
		LastTransHash: acc.LastTxHash,
		LastTransLT:   acc.LastTxLT,
	}

	bcCfg, err := b.api.GetBlockchainConfig(ctx, block)
	if err != nil {
		b.sendError(client, req.ID, "failed to get blockchain config: "+err.Error())
		return
	}
	preparedCfg, err := tvm.PrepareBlockchainConfig(bcCfg.Root)
	if err != nil {
		b.sendError(client, req.ID, "failed to prepare blockchain config: "+err.Error())
		return
	}
	blockCtx, err := preparedCfg.NewBlockContext(tvm.BlockOptions{
		Now:      networkNow,
		RandSeed: make([]byte, 32),
	})
	if err != nil {
		b.sendError(client, req.ID, "failed to prepare block context: "+err.Error())
		return
	}
	preparedAccount, err := tvm.PrepareAccount(shard, addr)
	if err != nil {
		b.sendError(client, req.ID, "failed to prepare account: "+err.Error())
		return
	}
	preparedMessage, err := tvm.PrepareMessage(msgCell)
	if err != nil {
		b.sendError(client, req.ID, "failed to prepare message: "+err.Error(), -32602)
		return
	}

	res, err := tvm.NewTVM().EmulateTransaction(blockCtx, preparedAccount, preparedMessage, params.transactionOptions())
	if err != nil {
		b.sendError(client, req.ID, "emulation failed: "+err.Error())
		return
	}

	// success defaults to false (e.g. a rejected external produces no transaction
	// and no fees); summarizeTxPhases overrides it when a transaction is produced.
	result := map[string]any{
		"accepted":   res.Accepted,
		"success":    false,
		"exit_code":  res.ExitCode,
		"gas_used":   res.GasUsed,
		"total_fees": "0",
	}
	if res.TransactionCell != nil {
		tx, parseErr := res.ParseTransaction()
		if parseErr != nil {
			b.sendError(client, req.ID, "failed to parse emulated transaction: "+parseErr.Error())
			return
		}
		result["transaction"] = serializeTransaction(tx)
		result["total_fees"] = tx.TotalFees.Coins.Nano().String()
		summarizeTxPhases(tx, result)
	}

	b.sendResult(client, req.ID, result)
}

// summarizeTxPhases extracts a wallet-friendly fee breakdown and success flag
// from an emulated ordinary transaction, writing them into out. It overrides
// exit_code/gas_used with the authoritative compute-phase values. A non-ordinary
// description (e.g. tick-tock) leaves out untouched.
func summarizeTxPhases(tx *tlb.Transaction, out map[string]any) {
	var desc tlb.TransactionDescriptionOrdinary
	switch d := tx.Description.(type) {
	case tlb.TransactionDescriptionOrdinary:
		desc = d
	case *tlb.TransactionDescriptionOrdinary:
		desc = *d
	default:
		return
	}

	out["aborted"] = desc.Aborted

	fees := map[string]any{"storage_fee": "0", "gas_fee": "0", "fwd_fee": "0", "action_fee": "0"}
	if desc.StoragePhase != nil {
		fees["storage_fee"] = desc.StoragePhase.StorageFeesCollected.Nano().String()
	}

	computeSuccess := false
	switch cp := desc.ComputePhase.Phase.(type) {
	case tlb.ComputePhaseVM:
		computeSuccess = cp.Success
		fees["gas_fee"] = cp.GasFees.Nano().String()
		out["exit_code"] = cp.Details.ExitCode
		if cp.Details.GasUsed != nil {
			out["gas_used"] = cp.Details.GasUsed.String()
		}
	case tlb.ComputePhaseSkipped:
		out["compute_skipped"] = string(cp.Reason.Type)
	}

	actionSuccess := true // no action phase = nothing to fail
	if desc.ActionPhase != nil {
		actionSuccess = desc.ActionPhase.Success
		out["action_result_code"] = desc.ActionPhase.ResultCode
		if desc.ActionPhase.TotalFwdFees != nil {
			fees["fwd_fee"] = desc.ActionPhase.TotalFwdFees.Nano().String()
		}
		if desc.ActionPhase.TotalActionFees != nil {
			fees["action_fee"] = desc.ActionPhase.TotalActionFees.Nano().String()
		}
	}

	out["fees"] = fees
	out["success"] = computeSuccess && actionSuccess && !desc.Aborted
}

// parseOutActions best-effort decodes a c5 action list (out_list) into a slice
// of out-message descriptors, in send order. The raw actions BOC is returned
// separately, so a partial or failed parse here loses no information.
func parseOutActions(actions *cell.Cell) []any {
	out := []any{}
	cur := actions
	for cur != nil {
		if cur.BitsSize() == 0 && cur.RefsNum() == 0 {
			break // out_list_empty terminator
		}
		sl, err := cur.BeginParse()
		if err != nil {
			break
		}
		prev, err := sl.LoadRefCell()
		if err != nil {
			break
		}
		tag, err := sl.LoadUInt(32)
		if err != nil {
			break
		}
		if tag == 0x0ec3c86d { // action_send_msg
			entry := map[string]any{}
			if mode, mErr := sl.LoadUInt(8); mErr == nil {
				entry["mode"] = mode
			}
			if msgRef, rErr := sl.LoadRefCell(); rErr == nil {
				if msl, pErr := msgRef.BeginParse(); pErr == nil {
					var m tlb.Message
					if m.LoadFromCell(msl) == nil {
						if m.MsgType == tlb.MsgTypeInternal {
							im := m.AsInternal()
							entry["type"] = "internal"
							if im.DstAddr != nil {
								entry["to"] = im.DstAddr.String()
							}
							entry["value"] = im.Amount.Nano().String()
							if im.Body != nil {
								entry["body"] = base64.StdEncoding.EncodeToString(im.Body.ToBOCWithFlags(false))
							}
						} else {
							entry["type"] = "external_out"
						}
					}
				}
			}
			out = append(out, entry)
		} else {
			out = append(out, map[string]any{"type": "other"})
		}
		cur = prev
	}
	// out_list is stored newest-first; reverse to send order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
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

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.Timeout)
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

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.SendWaitTimeout)
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

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.Timeout)
	defer cancel()

	var startLT uint64
	var startHash []byte

	if (params.LastLT == "") != (params.LastHash == "") {
		b.sendError(client, req.ID, "last_lt and last_hash must be provided together", -32602)
		return
	}
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
		if len(startHash) != 32 {
			b.sendError(client, req.ID, "last_hash must be 32 bytes", -32602)
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
	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.Timeout)
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

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.Timeout)
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
		After     *struct {
			Account string `json:"account"`
			LT      string `json:"lt"`
		} `json:"after,omitempty"`
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

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.Timeout)
	defer cancel()

	block, err := b.api.LookupBlock(ctx, params.Workchain, shard, params.Seqno)
	if err != nil {
		b.sendError(client, req.ID, "lookup block failed: "+err.Error())
		return
	}

	var after *ton.TransactionID3
	if params.After != nil {
		account, decodeErr := hex.DecodeString(params.After.Account)
		if decodeErr != nil || len(account) != 32 {
			b.sendError(client, req.ID, "invalid after.account: expected 32-byte hex", -32602)
			return
		}
		lt, parseErr := strconv.ParseUint(params.After.LT, 10, 64)
		if parseErr != nil {
			b.sendError(client, req.ID, "invalid after.lt: "+parseErr.Error(), -32602)
			return
		}
		after = &ton.TransactionID3{Account: account, LT: lt}
	}

	var txList []ton.TransactionShortInfo
	var incomplete bool
	if after == nil {
		txList, incomplete, err = b.api.GetBlockTransactionsV2(ctx, block, params.Count)
	} else {
		txList, incomplete, err = b.api.GetBlockTransactionsV2(ctx, block, params.Count, after)
	}
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

	result := map[string]any{
		"transactions": txResults,
		"incomplete":   incomplete,
		"next_after":   nil,
	}
	if incomplete && len(txList) > 0 {
		last := txList[len(txList)-1]
		result["next_after"] = map[string]any{
			"account": hex.EncodeToString(last.Account),
			"lt":      fmt.Sprintf("%d", last.LT),
		}
	}
	b.sendResult(client, req.ID, result)
}

func (b *WSBridge) handleGetShards(client *wsClient, req *WSRequest) {
	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.Timeout)
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
	if len(req.Params) > 0 && string(req.Params) != "null" {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
			return
		}
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.Timeout)
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

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.Timeout)
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

	// The request context bounds the history walk.
	currentLT := acc.LastTxLT
	currentHash := acc.LastTxHash

	for currentLT != 0 {
		txList, listErr := b.api.ListTransactions(ctx, addr, 100, currentLT, currentHash)
		if listErr != nil {
			b.sendError(client, req.ID, "failed to list transactions: "+listErr.Error())
			return
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
	if len(hashBytes) != 32 {
		b.sendError(client, req.ID, "msg_hash must be 32 bytes", -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.Timeout)
	defer cancel()

	tx, err := b.api.FindLastTransactionByInMsgHashAfterTime(ctx, addr, hashBytes, time.Time{})
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
	if len(hashBytes) != 32 {
		b.sendError(client, req.ID, "msg_hash must be 32 bytes", -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.Timeout)
	defer cancel()

	tx, err := b.api.FindLastTransactionByOutMsgHashAfterTime(ctx, addr, hashBytes, time.Time{})
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

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.Timeout)
	defer cancel()

	block, err := b.api.LookupBlock(ctx, params.Workchain, shard, params.Seqno)
	if err != nil {
		b.sendError(client, req.ID, "lookup block failed: "+err.Error())
		return
	}

	cl, err := b.api.GetBlockDataAsCell(ctx, block)
	if err != nil {
		b.sendError(client, req.ID, "get block data failed: "+err.Error())
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

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.Timeout)
	defer cancel()

	block, err := b.api.LookupBlock(ctx, params.Workchain, shard, params.Seqno)
	if err != nil {
		b.sendError(client, req.ID, "lookup block failed: "+err.Error())
		return
	}

	header, err := b.api.GetBlockHeader(ctx, block)
	if err != nil {
		b.sendError(client, req.ID, "get block header failed: "+err.Error())
		return
	}
	headerCell, err := tlb.ToCell(header)
	if err != nil {
		b.sendError(client, req.ID, "failed to serialize verified block header: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]any{
		"workchain":  block.Workchain,
		"shard":      fmt.Sprintf("%016x", uint64(block.Shard)),
		"seqno":      block.SeqNo,
		"root_hash":  hex.EncodeToString(block.RootHash),
		"file_hash":  hex.EncodeToString(block.FileHash),
		"header_boc": base64.StdEncoding.EncodeToString(headerCell.ToBOCWithFlags(false)),
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
		if len(decoded) != 32 {
			b.sendError(client, req.ID, fmt.Sprintf("invalid hash at index %d: expected 32 bytes", i), -32602)
			return
		}
		hashBytes[i] = decoded
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.Timeout)
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
	maxSubs := int32(b.cfg.Namespaces.Subscribe.MaxSubscriptions)
	if atomic.AddInt32(&client.activeSubs, 1) > maxSubs {
		atomic.AddInt32(&client.activeSubs, -1)
		b.sendError(client, req.ID, fmt.Sprintf("too many subscriptions (max %d)", maxSubs), -32602)
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
	msgSlice, err := msgCell.BeginParse()
	if err != nil {
		b.sendError(client, req.ID, "invalid BOC: "+err.Error(), -32602)
		return
	}
	var extMsg tlb.ExternalMessage
	if err := tlb.LoadFromCell(&extMsg, msgSlice); err != nil {
		b.sendError(client, req.ID, "failed to parse external message: "+err.Error())
		return
	}
	if extMsg.DstAddr == nil || extMsg.Body == nil {
		b.sendError(client, req.ID, "external message must contain destination and body", -32602)
		return
	}

	msgHash := msgCell.Hash()

	// Create a context with 180s timeout, cancellable by client disconnect
	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Lite.WatchTimeout)
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

	block, err := b.api.CurrentMasterchainInfo(ctx)
	if err != nil {
		b.sendError(client, req.ID, "failed to get masterchain info: "+err.Error())
		return
	}
	acc, err := b.api.GetAccount(ctx, block, extMsg.DstAddr)
	if err != nil {
		b.sendError(client, req.ID, "failed to get account state: "+err.Error())
		return
	}
	if err := b.api.SendExternalMessage(ctx, &extMsg); err != nil {
		b.sendError(client, req.ID, "send message failed: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]any{
		"watching":        true,
		"subscription_id": subID,
		"msg_hash":        hex.EncodeToString(msgHash),
	})

	tx, confirmedAt, err := b.waitForExternalMessage(ctx, &extMsg, block, acc)
	if err != nil {
		b.sendEvent(client, "tx_timeout", map[string]any{
			"msg_hash": hex.EncodeToString(msgHash),
			"reason":   err.Error(),
		})
		return
	}
	b.sendEvent(client, "tx_confirmed", map[string]any{
		"msg_hash":    hex.EncodeToString(msgHash),
		"transaction": serializeTransaction(tx),
		"block": map[string]any{
			"seqno":     confirmedAt.SeqNo,
			"workchain": confirmedAt.Workchain,
			"shard":     fmt.Sprintf("%016x", uint64(confirmedAt.Shard)),
		},
	})
}

func (b *WSBridge) waitForExternalMessage(ctx context.Context, ext *tlb.ExternalMessage, block *ton.BlockIDExt, acc *tlb.Account) (*tlb.Transaction, *ton.BlockIDExt, error) {
	for ctx.Err() == nil {
		newBlock, err := b.api.WaitForBlock(block.SeqNo + 1).GetMasterchainInfo(ctx)
		if err != nil {
			continue
		}
		newAcc, err := b.api.WaitForBlock(newBlock.SeqNo).GetAccount(ctx, newBlock, ext.DstAddr)
		if err != nil {
			continue
		}
		block = newBlock

		if newAcc.LastTxLT == acc.LastTxLT {
			if err := b.api.SendExternalMessage(ctx, ext); err != nil {
				continue
			}
			continue
		}

		lastLT, lastHash := newAcc.LastTxLT, newAcc.LastTxHash
		for ctx.Err() == nil && lastLT != 0 {
			txList, err := b.api.WaitForBlock(block.SeqNo).ListTransactions(ctx, ext.DstAddr, 20, lastLT, lastHash)
			if err != nil {
				continue
			}

			sawPrevious := false
			for i, tx := range txList {
				if i == 0 {
					lastLT, lastHash = tx.PrevTxLT, tx.PrevTxHash
				}
				if tx.PrevTxLT == acc.LastTxLT && bytes.Equal(tx.PrevTxHash, acc.LastTxHash) {
					sawPrevious = true
				}
				if tx.IO.In == nil || tx.IO.In.MsgType != tlb.MsgTypeExternalIn {
					continue
				}
				in := tx.IO.In.AsExternalIn()
				if ext.StateInit != nil {
					if in.StateInit == nil || ext.StateInit.Code == nil || ext.StateInit.Data == nil || in.StateInit.Code == nil || in.StateInit.Data == nil {
						continue
					}
					if ext.StateInit.Code.HashKey() != in.StateInit.Code.HashKey() || ext.StateInit.Data.HashKey() != in.StateInit.Data.HashKey() {
						continue
					}
				}
				if in.Body != nil && ext.Body != nil && in.Body.HashKey() == ext.Body.HashKey() {
					return tx, block, nil
				}
			}
			if sawPrevious || len(txList) == 0 {
				break
			}
		}
		acc = newAcc
	}
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	return nil, nil, fmt.Errorf("transaction was not confirmed")
}
