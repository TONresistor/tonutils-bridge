package wsbridge

import (
	"math/big"
	"testing"

	"github.com/xssnick/tonutils-go/tlb"
)

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
