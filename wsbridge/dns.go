package wsbridge

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strings"

	"github.com/xssnick/tonutils-go/tvm/cell"
)

func (b *WSBridge) handleDNSResolve(client *wsClient, req *WSRequest) {
	var params struct {
		Domain string `json:"domain"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}
	if params.Domain == "" {
		b.sendError(client, req.ID, "domain must not be empty", -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.DNS.Timeout)
	defer cancel()

	domain, err := b.dns.Resolve(ctx, params.Domain)
	if err != nil {
		b.sendError(client, req.ID, "dns resolve failed: "+err.Error())
		return
	}

	result := map[string]any{
		"wallet":      nil,
		"site_adnl":   nil,
		"has_storage": false,
		"owner":       nil,
		"nft_address": nil,
		"collection":  nil,
		"editor":      nil,
		"initialized": false,
		"expiring_at": nil,
	}

	wallet := domain.GetWalletRecord()
	if wallet != nil {
		result["wallet"] = wallet.String()
	}

	siteRecord, inStorage := domain.GetSiteRecord()
	if siteRecord != nil {
		result["site_adnl"] = hex.EncodeToString(siteRecord)
	}
	result["has_storage"] = inStorage

	// NFT data: owner, collection, address, initialization status
	nftData, err := domain.GetNFTData(ctx)
	if err == nil {
		result["initialized"] = nftData.Initialized
		if nftData.OwnerAddress != nil {
			result["owner"] = nftData.OwnerAddress.String()
		}
		if nftData.CollectionAddress != nil {
			result["collection"] = nftData.CollectionAddress.String()
		}
	}

	// Domain expiration from the collection contract
	if nftData != nil && nftData.CollectionAddress != nil {
		// Strip TLD, reverse remaining parts, null-separate for TON DNS encoding.
		// "example.ton" -> "example\0", "sub.example.ton" -> "example\0sub\0"
		parts := strings.Split(strings.TrimSuffix(params.Domain, "."), ".")
		if len(parts) > 1 {
			parts = parts[:len(parts)-1] // strip TLD
		}
		for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
			parts[i], parts[j] = parts[j], parts[i]
		}
		var buf []byte
		for _, p := range parts {
			buf = append(buf, []byte(p)...)
			buf = append(buf, 0)
		}
		if len(buf)*8 > 1023 {
			// Domain too long to fit in a single cell
			b.sendResult(client, req.ID, result)
			return
		}
		builder := cell.BeginCell()
		if err := builder.StoreSlice(buf, uint(len(buf)*8)); err != nil {
			b.sendResult(client, req.ID, result)
			return
		}
		domainCell := builder.EndCell()

		block, blkErr := b.api.CurrentMasterchainInfo(ctx)
		if blkErr == nil {
			expRes, expErr := b.api.RunGetMethod(ctx, block, nftData.CollectionAddress, "getexpiration", domainCell.BeginParse())
			if expErr == nil {
				stack := expRes.AsTuple()
				if len(stack) >= 1 {
					if v, ok := stack[0].(*big.Int); ok && v != nil {
						result["expiring_at"] = v.Int64()
					}
				}
			}
		}
	}

	// NFT address of the domain itself
	if nftAddr := domain.GetNFTAddress(); nftAddr != nil {
		result["nft_address"] = nftAddr.String()
	}

	// Editor address (who can modify DNS records)
	editor, err := domain.GetEditor(ctx)
	if err == nil && editor != nil {
		result["editor"] = editor.String()
	}

	// Collect all DNS text records (category 0x1eda) from the records dictionary.
	// Record keys are sha256(name) — since we can't reverse the hash, we expose
	// them as hex keys with their text values.
	if domain.Records != nil {
		allRecs, _ := domain.Records.LoadAll()
		textRecords := map[string]string{}
		for _, kv := range allRecs {
			ref, err := kv.Value.LoadRef()
			if err != nil {
				continue
			}
			cat, err := ref.LoadUInt(16)
			if err != nil || cat != 0x1eda {
				continue
			}
			data, err := ref.LoadBinarySnake()
			if err != nil || len(data) < 2 {
				continue
			}
			keySlice, err := kv.Key.LoadSlice(256)
			if err != nil {
				continue
			}
			keyHex := hex.EncodeToString(keySlice)
			textRecords[keyHex] = string(data[2:])
		}
		if len(textRecords) > 0 {
			result["text_records"] = textRecords
		}
	}

	b.sendResult(client, req.ID, result)
}
