package wsbridge

import (
	"context"
	"encoding/json"
	"math/big"

	"github.com/xssnick/tonutils-go/ton/jetton"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

func (b *WSBridge) handleJettonGetData(client *wsClient, req *WSRequest) {
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

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Jetton.Timeout)
	defer cancel()

	jClient := jetton.NewJettonMasterClient(b.api, addr)
	data, err := jClient.GetJettonData(ctx)
	if err != nil {
		b.sendError(client, req.ID, "get jetton data failed: "+err.Error())
		return
	}

	totalSupply := "0"
	if data.TotalSupply != nil {
		totalSupply = data.TotalSupply.String()
	}
	result := map[string]any{
		"total_supply": totalSupply,
		"mintable":     data.Mintable,
		"admin":        nil,
		"content":      serializeJettonContent(data.Content),
	}

	if data.AdminAddr != nil {
		result["admin"] = data.AdminAddr.String()
	}

	b.sendResult(client, req.ID, result)
}

func (b *WSBridge) handleJettonGetWalletAddress(client *wsClient, req *WSRequest) {
	var params struct {
		JettonMaster string `json:"jetton_master"`
		Owner        string `json:"owner"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	masterAddr, err := parseAddress(params.JettonMaster)
	if err != nil {
		b.sendError(client, req.ID, "invalid jetton_master address: "+err.Error(), -32602)
		return
	}

	ownerAddr, err := parseAddress(params.Owner)
	if err != nil {
		b.sendError(client, req.ID, "invalid owner address: "+err.Error(), -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Jetton.Timeout)
	defer cancel()

	jClient := jetton.NewJettonMasterClient(b.api, masterAddr)
	wallet, err := jClient.GetJettonWallet(ctx, ownerAddr)
	if err != nil {
		b.sendError(client, req.ID, "get jetton wallet failed: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]any{
		"wallet_address": wallet.Address().String(),
	})
}

func (b *WSBridge) handleJettonGetBalance(client *wsClient, req *WSRequest) {
	var params struct {
		JettonWallet string `json:"jetton_wallet"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	addr, err := parseAddress(params.JettonWallet)
	if err != nil {
		b.sendError(client, req.ID, "invalid address: "+err.Error(), -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.Jetton.Timeout)
	defer cancel()

	block, err := b.api.CurrentMasterchainInfo(ctx)
	if err != nil {
		b.sendError(client, req.ID, "failed to get masterchain info: "+err.Error())
		return
	}

	// Call get_wallet_data on the jetton wallet contract
	res, err := b.api.RunGetMethod(ctx, block, addr, "get_wallet_data")
	if err != nil {
		b.sendError(client, req.ID, "run get_wallet_data failed: "+err.Error())
		return
	}

	stack := res.AsTuple()
	result := map[string]any{
		"balance":       "0",
		"owner":         nil,
		"jetton_master": nil,
	}

	// Stack: [balance, owner_addr, jetton_master, wallet_code]
	if len(stack) >= 1 {
		if bal, ok := stack[0].(*big.Int); ok {
			result["balance"] = bal.String()
		}
	}
	if len(stack) >= 2 {
		if sl, ok := stack[1].(*cell.Slice); ok && sl != nil {
			ownerAddr, err := sl.LoadAddr()
			if err == nil && ownerAddr != nil {
				result["owner"] = ownerAddr.String()
			}
		}
	}
	if len(stack) >= 3 {
		if sl, ok := stack[2].(*cell.Slice); ok && sl != nil {
			masterAddr, err := sl.LoadAddr()
			if err == nil && masterAddr != nil {
				result["jetton_master"] = masterAddr.String()
			}
		}
	}

	b.sendResult(client, req.ID, result)
}
