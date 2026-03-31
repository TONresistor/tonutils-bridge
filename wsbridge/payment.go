package wsbridge

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"

	"github.com/xssnick/tonutils-go/ton/payments"
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

	block, err := b.api.CurrentMasterchainInfo(ctx)
	if err != nil {
		b.sendError(client, req.ID, "failed to get masterchain info: "+err.Error())
		return
	}

	pClient := payments.NewPaymentChannelClient(b.api)
	ch, err := pClient.GetAsyncChannel(ctx, block, addr, false)
	if err != nil {
		b.sendError(client, req.ID, "get async channel failed: "+err.Error())
		return
	}

	s := ch.Storage

	result := map[string]any{
		"status":            int(ch.Status),
		"initialized":       s.Initialized,
		"balance_a":         s.BalanceA.Nano().String(),
		"balance_b":         s.BalanceB.Nano().String(),
		"key_a":             base64.StdEncoding.EncodeToString(s.KeyA),
		"key_b":             base64.StdEncoding.EncodeToString(s.KeyB),
		"channel_id":        hex.EncodeToString(s.ChannelID),
		"committed_seqno_a": s.CommittedSeqnoA,
		"committed_seqno_b": s.CommittedSeqnoB,
		"quarantine":        nil,
		"dest_a":            nil,
		"dest_b":            nil,
		"excess_fee":        s.Payments.ExcessFee.Nano().String(),
		"closing_config": map[string]any{
			"quarantine_duration":         s.ClosingConfig.QuarantineDuration,
			"conditional_close_duration":  s.ClosingConfig.ConditionalCloseDuration,
			"misbehavior_fine":            s.ClosingConfig.MisbehaviorFine.Nano().String(),
		},
	}

	if s.Payments.DestA != nil {
		result["dest_a"] = s.Payments.DestA.String()
	}
	if s.Payments.DestB != nil {
		result["dest_b"] = s.Payments.DestB.String()
	}

	if s.Quarantine != nil {
		q := s.Quarantine
		result["quarantine"] = map[string]any{
			"quarantine_starts":   q.QuarantineStarts,
			"state_committed_by_a": q.StateCommittedByA,
			"state_challenged":    q.StateChallenged,
			"state_a": map[string]any{
				"seqno": q.StateA.Seqno,
				"sent":  q.StateA.Sent.Nano().String(),
			},
			"state_b": map[string]any{
				"seqno": q.StateB.Seqno,
				"sent":  q.StateB.Sent.Nano().String(),
			},
		}
	}

	b.sendResult(client, req.ID, result)
}
