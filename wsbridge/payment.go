package wsbridge

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/payments"
	pnclient "github.com/xssnick/ton-payment-network/tonpayments/chain/client"
)

func (b *WSBridge) handlePaymentGetChannelState(client *wsClient, req *WSRequest) {
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

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Payment.Timeout)
	defer cancel()

	pClient := payments.NewPaymentChannelClient(pnclient.NewTON(b.api))
	ch, err := pClient.GetChannel(ctx, addr, false, time.Time{})
	if err != nil {
		b.sendError(client, req.ID, "get channel failed: "+err.Error())
		return
	}

	s := ch.Storage

	result := map[string]any{
		"status":          int(ch.Status),
		"is_a":            s.IsA,
		"initialized":     s.Initialized,
		"committed_seqno": s.CommittedSeqno,
		"wallet_seqno":    s.WalletSeqno,
		"key_a":           base64.StdEncoding.EncodeToString(s.KeyA),
		"key_b":           base64.StdEncoding.EncodeToString(s.KeyB),
		"channel_id":      hex.EncodeToString(s.ChannelID),
		"party_address":   nil,
		"closing_config": map[string]any{
			"quarantine_duration":               s.ClosingConfig.QuarantineDuration,
			"conditional_close_duration":         s.ClosingConfig.ConditionalCloseDuration,
			"actions_duration":                   s.ClosingConfig.ActionsDuration,
			"replication_message_attach_amount":  s.ClosingConfig.ReplicationMessageAttachAmount.Nano().String(),
		},
		"quarantine": nil,
	}

	if s.PartyAddress != nil {
		result["party_address"] = s.PartyAddress.String()
	}

	if s.Quarantine != nil {
		q := s.Quarantine
		qm := map[string]any{
			"seqno":                    q.Seqno,
			"quarantine_starts":        q.QuarantineStarts,
			"committed_by_owner":       q.CommittedByOwner,
			"our_settlement_finalized": q.OurSettlementFinalized,
			"actions_to_execute_hash":  hex.EncodeToString(q.ActionsToExecuteHash),
			"their_state":              nil,
		}
		if q.TheirState != nil {
			qm["their_state"] = map[string]any{
				"conditionals_hash":  hex.EncodeToString(q.TheirState.ConditionalsHash),
				"action_states_hash": hex.EncodeToString(q.TheirState.ActionStatesHash),
			}
		}
		result["quarantine"] = qm
	}

	b.sendResult(client, req.ID, result)
}
