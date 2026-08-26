package wsbridge

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

func TestEmulateTransactionParamsSignatureCheckOption(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "disabled by default", raw: `{"address":"0:abc","boc":"te6ccg=="}`, want: false},
		{name: "enabled explicitly", raw: `{"address":"0:abc","boc":"te6ccg==","ignore_chksig":true}`, want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			params, err := parseEmulateTransactionParams(json.RawMessage(test.raw))
			if err != nil {
				t.Fatalf("parse params: %v", err)
			}
			if got := params.transactionOptions().SignatureCheckAlwaysSucceed; got != test.want {
				t.Fatalf("SignatureCheckAlwaysSucceed = %t, want %t", got, test.want)
			}
		})
	}
}

func TestAccountStateForTransactionEmulationAcceptsUninitializedAccount(t *testing.T) {
	state := &tlb.AccountState{
		IsValid: true,
		AccountStorage: tlb.AccountStorage{
			Status: tlb.AccountStatusUninit,
		},
	}
	got, err := accountStateForTransactionEmulation(&tlb.Account{State: state})
	if err != nil {
		t.Fatalf("uninitialized account rejected: %v", err)
	}
	if got != state {
		t.Fatal("returned a different account state")
	}
}

func TestAccountStateForTransactionEmulationRejectsMissingAccount(t *testing.T) {
	tests := []struct {
		name string
		acc  *tlb.Account
	}{
		{name: "nil account"},
		{name: "missing state", acc: &tlb.Account{}},
		{name: "invalid state", acc: &tlb.Account{State: &tlb.AccountState{}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := accountStateForTransactionEmulation(test.acc); err == nil {
				t.Fatal("expected account to be rejected")
			}
		})
	}
}

type verifiedBlockAPI struct {
	ton.APIClientWrapped
	dataCalled   bool
	headerCalled bool
}

func (a *verifiedBlockAPI) LookupBlock(context.Context, int32, int64, uint32) (*ton.BlockIDExt, error) {
	return &ton.BlockIDExt{Workchain: -1, Shard: -0x8000000000000000, SeqNo: 1, RootHash: make([]byte, 32), FileHash: make([]byte, 32)}, nil
}

func (a *verifiedBlockAPI) GetBlockDataAsCell(context.Context, *ton.BlockIDExt) (*cell.Cell, error) {
	a.dataCalled = true
	return cell.BeginCell().EndCell(), nil
}

func (a *verifiedBlockAPI) GetBlockHeader(context.Context, *ton.BlockIDExt) (*tlb.BlockHeader, error) {
	a.headerCalled = true
	return &tlb.BlockHeader{}, nil
}

// ordinaryTx wraps an ordinary transaction description for summarizeTxPhases.
func ordinaryTx(desc tlb.TransactionDescriptionOrdinary) *tlb.Transaction {
	return &tlb.Transaction{Description: desc}
}

func vmCompute(success bool, gasFeeNano uint64, gasUsed int64, exit int32) tlb.ComputePhase {
	return tlb.ComputePhase{Phase: tlb.ComputePhaseVM{
		Success: success,
		GasFees: tlb.FromNanoTONU(gasFeeNano),
		Details: tlb.ComputePhaseVMDetails{
			GasUsed:  big.NewInt(gasUsed),
			ExitCode: exit,
		},
	}}
}

func TestSummarizeTxPhases_VMSuccess(t *testing.T) {
	out := map[string]any{"success": false, "exit_code": int64(0), "gas_used": int64(0)}
	tx := ordinaryTx(tlb.TransactionDescriptionOrdinary{
		StoragePhase: &tlb.StoragePhase{StorageFeesCollected: tlb.FromNanoTONU(1000)},
		ComputePhase: vmCompute(true, 874000, 874, 0),
		ActionPhase: &tlb.ActionPhase{
			Success:         true,
			TotalFwdFees:    ptrCoins(266668),
			TotalActionFees: ptrCoins(133332),
			ResultCode:      0,
		},
		Aborted: false,
	})

	summarizeTxPhases(tx, out)

	if out["success"] != true {
		t.Fatalf("expected success=true, got %v", out["success"])
	}
	if out["aborted"] != false {
		t.Fatalf("expected aborted=false, got %v", out["aborted"])
	}
	if out["exit_code"] != int32(0) {
		t.Fatalf("expected exit_code int32(0), got %v (%T)", out["exit_code"], out["exit_code"])
	}
	if out["gas_used"] != "874" {
		t.Fatalf("expected gas_used \"874\", got %v (%T)", out["gas_used"], out["gas_used"])
	}
	fees, ok := out["fees"].(map[string]any)
	if !ok {
		t.Fatalf("expected fees map, got %T", out["fees"])
	}
	for k, want := range map[string]string{
		"storage_fee": "1000",
		"gas_fee":     "874000",
		"fwd_fee":     "266668",
		"action_fee":  "133332",
	} {
		if fees[k] != want {
			t.Errorf("fees[%s]: expected %s, got %v", k, want, fees[k])
		}
	}
	if out["action_result_code"] != int32(0) {
		t.Errorf("expected action_result_code int32(0), got %v", out["action_result_code"])
	}
}

func TestSummarizeTxPhases_ComputeSkipped(t *testing.T) {
	out := map[string]any{"success": false}
	tx := ordinaryTx(tlb.TransactionDescriptionOrdinary{
		ComputePhase: tlb.ComputePhase{Phase: tlb.ComputePhaseSkipped{
			Reason: tlb.ComputeSkipReason{Type: tlb.ComputeSkipReasonNoGas},
		}},
	})

	summarizeTxPhases(tx, out)

	if out["compute_skipped"] != string(tlb.ComputeSkipReasonNoGas) {
		t.Fatalf("expected compute_skipped=NO_GAS, got %v", out["compute_skipped"])
	}
	if out["success"] != false {
		t.Fatalf("expected success=false when compute skipped, got %v", out["success"])
	}
}

func TestSummarizeTxPhases_NilPhases(t *testing.T) {
	// No storage and no action phase: fee fields default to "0",
	// action success defaults to true (nothing to fail), so a successful
	// VM compute with no action phase yields success=true.
	out := map[string]any{"success": false}
	tx := ordinaryTx(tlb.TransactionDescriptionOrdinary{
		ComputePhase: vmCompute(true, 500, 10, 0),
	})

	summarizeTxPhases(tx, out)

	fees := out["fees"].(map[string]any)
	if fees["storage_fee"] != "0" || fees["fwd_fee"] != "0" || fees["action_fee"] != "0" {
		t.Fatalf("expected zero default fees, got %v", fees)
	}
	if fees["gas_fee"] != "500" {
		t.Fatalf("expected gas_fee 500, got %v", fees["gas_fee"])
	}
	if out["success"] != true {
		t.Fatalf("expected success=true with successful compute and no action phase, got %v", out["success"])
	}
	if _, present := out["action_result_code"]; present {
		t.Fatalf("action_result_code should be absent without an action phase")
	}
}

func TestSummarizeTxPhases_Aborted(t *testing.T) {
	out := map[string]any{"success": false}
	tx := ordinaryTx(tlb.TransactionDescriptionOrdinary{
		ComputePhase: vmCompute(true, 100, 5, 0),
		Aborted:      true,
	})

	summarizeTxPhases(tx, out)

	if out["aborted"] != true {
		t.Fatalf("expected aborted=true, got %v", out["aborted"])
	}
	if out["success"] != false {
		t.Fatalf("expected success=false when aborted, got %v", out["success"])
	}
}

func TestSummarizeTxPhases_ActionFailure(t *testing.T) {
	out := map[string]any{"success": false}
	tx := ordinaryTx(tlb.TransactionDescriptionOrdinary{
		ComputePhase: vmCompute(true, 100, 5, 0),
		ActionPhase:  &tlb.ActionPhase{Success: false, ResultCode: 37},
	})

	summarizeTxPhases(tx, out)

	if out["success"] != false {
		t.Fatalf("expected success=false when action phase fails, got %v", out["success"])
	}
	if out["action_result_code"] != int32(37) {
		t.Fatalf("expected action_result_code=37, got %v", out["action_result_code"])
	}
}

func TestSummarizeTxPhases_NonOrdinaryLeavesUntouched(t *testing.T) {
	out := map[string]any{"success": false, "marker": "kept"}
	tx := &tlb.Transaction{Description: tlb.TransactionDescriptionStorage{}}

	summarizeTxPhases(tx, out)

	if out["marker"] != "kept" {
		t.Fatalf("non-ordinary description must leave the map untouched")
	}
	if _, present := out["fees"]; present {
		t.Fatalf("non-ordinary description must not add fees")
	}
}

func TestSummarizeTxPhases_PointerDescription(t *testing.T) {
	// The handler may receive Description as a pointer; both forms must work.
	out := map[string]any{"success": false}
	tx := &tlb.Transaction{Description: &tlb.TransactionDescriptionOrdinary{
		ComputePhase: vmCompute(true, 100, 5, 0),
	}}

	summarizeTxPhases(tx, out)

	if out["success"] != true {
		t.Fatalf("expected pointer-form description to be handled, got success=%v", out["success"])
	}
}

func ptrCoins(nano uint64) *tlb.Coins {
	c := tlb.FromNanoTONU(nano)
	return &c
}

func TestBlockMethodsUseVerifiedTonutilsHelpers(t *testing.T) {
	api := &verifiedBlockAPI{}
	bridge := testBridge()
	bridge.api = api
	conn, cleanup := dialTestBridge(t, bridge)
	defer cleanup()

	params := map[string]any{"workchain": -1, "shard": "8000000000000000", "seqno": 1}
	resp := rpc(t, conn, "data", "lite.getBlockData", params)
	if resp.Error != nil {
		t.Fatalf("getBlockData failed: %s", resp.Error.Message)
	}
	if !api.dataCalled {
		t.Fatal("lite.getBlockData bypassed GetBlockDataAsCell")
	}

	_ = rpc(t, conn, "header", "lite.getBlockHeader", params)
	if !api.headerCalled {
		t.Fatal("lite.getBlockHeader bypassed GetBlockHeader")
	}
}

type transientTransactionListAPI struct {
	ton.APIClientWrapped
	masterCalls int
	listCalls   int
	account     *tlb.Account
	tx          *tlb.Transaction
}

func (a *transientTransactionListAPI) WaitForBlock(uint32) ton.APIClientWrapped {
	return a
}

func (a *transientTransactionListAPI) GetMasterchainInfo(context.Context) (*ton.BlockIDExt, error) {
	a.masterCalls++
	return &ton.BlockIDExt{SeqNo: uint32(10 + a.masterCalls)}, nil
}

func (a *transientTransactionListAPI) GetAccount(context.Context, *ton.BlockIDExt, *address.Address) (*tlb.Account, error) {
	return a.account, nil
}

func (a *transientTransactionListAPI) ListTransactions(context.Context, *address.Address, uint32, uint64, []byte) ([]*tlb.Transaction, error) {
	a.listCalls++
	if a.masterCalls < 2 {
		return nil, errors.New("transient liteserver error")
	}
	return []*tlb.Transaction{a.tx}, nil
}

func TestWaitForExternalMessageRetriesTransactionScanOnNextBlock(t *testing.T) {
	body := cell.BeginCell().MustStoreUInt(0x1234, 16).EndCell()
	destination := address.NewAddress(0, 0, make([]byte, 32))
	previousHash := make([]byte, 32)
	previousHash[0] = 1
	latestHash := make([]byte, 32)
	latestHash[0] = 2

	tx := &tlb.Transaction{PrevTxLT: 1, PrevTxHash: previousHash}
	tx.IO.In = &tlb.Message{
		MsgType: tlb.MsgTypeExternalIn,
		Msg:     &tlb.ExternalMessage{DstAddr: destination, Body: body},
	}
	api := &transientTransactionListAPI{
		account: &tlb.Account{LastTxLT: 2, LastTxHash: latestHash},
		tx:      tx,
	}
	bridge := testBridge()
	bridge.api = api

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got, block, err := bridge.waitForExternalMessage(
		ctx,
		&tlb.ExternalMessage{DstAddr: destination, Body: body},
		&ton.BlockIDExt{SeqNo: 10},
		&tlb.Account{LastTxLT: 1, LastTxHash: previousHash},
	)
	if err != nil {
		t.Fatalf("wait for external message: %v", err)
	}
	if got != tx {
		t.Fatal("returned a different transaction")
	}
	if block == nil || block.SeqNo != 12 {
		t.Fatalf("confirmed block = %#v, want seqno 12", block)
	}
	if api.masterCalls != 2 || api.listCalls != 2 {
		t.Fatalf("master calls = %d, list calls = %d; want 2 and 2", api.masterCalls, api.listCalls)
	}
}
