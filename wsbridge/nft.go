package wsbridge

import (
	"context"
	"encoding/json"
	"math/big"
	"time"

	"github.com/xssnick/tonutils-go/ton/nft"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

func (b *WSBridge) handleNFTGetData(client *wsClient, req *WSRequest) {
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

	nftClient := nft.NewItemClient(b.api, addr)
	data, err := nftClient.GetNFTData(ctx)
	if err != nil {
		b.sendError(client, req.ID, "get nft data failed: "+err.Error())
		return
	}

	indexStr := "0"
	if data.Index != nil {
		indexStr = data.Index.String()
	}
	// If the NFT belongs to a collection, resolve full content via the collection
	// contract (combines base URL + individual content per TEP-62).
	content := data.Content
	if data.CollectionAddress != nil && data.Index != nil {
		collClient := nft.NewCollectionClient(b.api, data.CollectionAddress)
		individualContent := data.Content
		if individualContent == nil {
			individualContent = &nft.ContentOffchain{}
		}
		if fullContent, err := collClient.GetNFTContent(ctx, data.Index, individualContent); err == nil {
			content = fullContent
		}
	}

	result := map[string]any{
		"initialized": data.Initialized,
		"index":       indexStr,
		"collection":  nil,
		"owner":       nil,
		"content":     serializeContent(content),
	}

	if data.CollectionAddress != nil {
		result["collection"] = data.CollectionAddress.String()
	}
	if data.OwnerAddress != nil {
		result["owner"] = data.OwnerAddress.String()
	}

	b.sendResult(client, req.ID, result)
}

func (b *WSBridge) handleNFTGetCollectionData(client *wsClient, req *WSRequest) {
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

	collClient := nft.NewCollectionClient(b.api, addr)
	data, err := collClient.GetCollectionData(ctx)
	if err != nil {
		b.sendError(client, req.ID, "get collection data failed: "+err.Error())
		return
	}

	nextItemIndex := "0"
	if data.NextItemIndex != nil {
		nextItemIndex = data.NextItemIndex.String()
	}
	result := map[string]any{
		"next_item_index": nextItemIndex,
		"owner":           nil,
		"content":         serializeContent(data.Content),
	}

	if data.OwnerAddress != nil {
		result["owner"] = data.OwnerAddress.String()
	}

	b.sendResult(client, req.ID, result)
}

func (b *WSBridge) handleNFTGetAddressByIndex(client *wsClient, req *WSRequest) {
	var params struct {
		Collection string `json:"collection"`
		Index      string `json:"index"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	collAddr, err := parseAddress(params.Collection)
	if err != nil {
		b.sendError(client, req.ID, "invalid collection address: "+err.Error(), -32602)
		return
	}

	index := new(big.Int)
	if _, ok := index.SetString(params.Index, 10); !ok {
		b.sendError(client, req.ID, "invalid index: must be a decimal integer", -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
	defer cancel()

	collClient := nft.NewCollectionClient(b.api, collAddr)
	addr, err := collClient.GetNFTAddressByIndex(ctx, index)
	if err != nil {
		b.sendError(client, req.ID, "get nft address by index failed: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]any{
		"address": addr.String(),
	})
}

func (b *WSBridge) handleNFTGetRoyaltyParams(client *wsClient, req *WSRequest) {
	var params struct {
		Collection string `json:"collection"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	collAddr, err := parseAddress(params.Collection)
	if err != nil {
		b.sendError(client, req.ID, "invalid collection address: "+err.Error(), -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
	defer cancel()

	collClient := nft.NewCollectionClient(b.api, collAddr)
	royalty, err := collClient.RoyaltyParams(ctx)
	if err != nil {
		b.sendError(client, req.ID, "get royalty params failed: "+err.Error())
		return
	}

	result := map[string]any{
		"factor":  royalty.Factor,
		"base":    royalty.Base,
		"address": nil,
	}
	if royalty.Address != nil {
		result["address"] = royalty.Address.String()
	}

	b.sendResult(client, req.ID, result)
}

// serializeContent returns a JSON-serializable map describing an nft.ContentAny value.
func serializeContent(content nft.ContentAny) map[string]any {
	return serializeContentBase(content)
}

func (b *WSBridge) handleNFTGetContent(client *wsClient, req *WSRequest) {
	var params struct {
		Collection        string `json:"collection"`
		Index             string `json:"index"`
		IndividualContent string `json:"individual_content"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	collAddr, err := parseAddress(params.Collection)
	if err != nil {
		b.sendError(client, req.ID, "invalid collection address: "+err.Error(), -32602)
		return
	}

	index := new(big.Int)
	if _, ok := index.SetString(params.Index, 10); !ok {
		b.sendError(client, req.ID, "invalid index: must be a decimal integer", -32602)
		return
	}

	bocBytes, err := decodeBase64(params.IndividualContent)
	if err != nil {
		b.sendError(client, req.ID, "invalid individual_content: base64 decode failed: "+err.Error(), -32602)
		return
	}

	contentCell, err := cell.FromBOC(bocBytes)
	if err != nil {
		b.sendError(client, req.ID, "invalid individual_content: BOC parse failed: "+err.Error(), -32602)
		return
	}

	individualContent, err := nft.ContentFromCell(contentCell)
	if err != nil {
		b.sendError(client, req.ID, "invalid individual_content: content parse failed: "+err.Error(), -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, 10*time.Second)
	defer cancel()

	collClient := nft.NewCollectionClient(b.api, collAddr)
	fullContent, err := collClient.GetNFTContent(ctx, index, individualContent)
	if err != nil {
		b.sendError(client, req.ID, "get nft content failed: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]any{
		"content": serializeContent(fullContent),
	})
}
