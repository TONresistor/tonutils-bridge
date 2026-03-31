package wsbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"time"

	"github.com/xssnick/tonutils-go/ton/wallet"
)

func (b *WSBridge) handleWalletGetSeqno(client *wsClient, req *WSRequest) {
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

	res, err := b.api.RunGetMethod(ctx, block, addr, "seqno")
	if err != nil {
		b.sendError(client, req.ID, "run seqno failed: "+err.Error())
		return
	}

	stack := res.AsTuple()
	seqno := uint64(0)
	if len(stack) >= 1 {
		if v, ok := stack[0].(*big.Int); ok && v != nil {
			seqno = v.Uint64()
		}
	}

	b.sendResult(client, req.ID, map[string]any{
		"seqno": seqno,
	})
}

func (b *WSBridge) handleWalletGetPublicKey(client *wsClient, req *WSRequest) {
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

	pubKey, err := wallet.GetPublicKey(ctx, b.api, addr)
	if err != nil {
		b.sendError(client, req.ID, "get public key failed: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]any{
		"public_key": base64.StdEncoding.EncodeToString(pubKey),
	})
}
