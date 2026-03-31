package wsbridge

import (
	"context"
	"encoding/json"
	"math/big"
	"time"

	"github.com/xssnick/tonutils-go/tvm/cell"
)

func (b *WSBridge) handleSBTGetAuthorityAddress(client *wsClient, req *WSRequest) {
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

	res, err := b.api.RunGetMethod(ctx, block, addr, "get_authority_address")
	if err != nil {
		b.sendError(client, req.ID, "run get_authority_address failed: "+err.Error())
		return
	}

	stack := res.AsTuple()
	if len(stack) < 1 {
		b.sendError(client, req.ID, "empty result from get_authority_address")
		return
	}

	sl, ok := stack[0].(*cell.Slice)
	if !ok || sl == nil {
		b.sendResult(client, req.ID, map[string]any{
			"authority": nil,
		})
		return
	}

	authority, err := sl.LoadAddr()
	if err != nil {
		b.sendError(client, req.ID, "failed to load authority address: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]any{
		"authority": authority.String(),
	})
}

func (b *WSBridge) handleSBTGetRevokedTime(client *wsClient, req *WSRequest) {
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

	res, err := b.api.RunGetMethod(ctx, block, addr, "get_revoked_time")
	if err != nil {
		b.sendError(client, req.ID, "run get_revoked_time failed: "+err.Error())
		return
	}

	stack := res.AsTuple()
	revokedTime := int64(0)
	if len(stack) >= 1 {
		if v, ok := stack[0].(*big.Int); ok && v != nil {
			revokedTime = v.Int64()
		}
	}

	b.sendResult(client, req.ID, map[string]any{
		"revoked_time": revokedTime,
	})
}
