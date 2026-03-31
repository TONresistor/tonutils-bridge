package wsbridge

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"net"
	"strings"

	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/ton/nft"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

// privateNets holds pre-parsed CIDR ranges for private/loopback/link-local IPs.
var privateNets []*net.IPNet

func init() {
	for _, cidr := range []string{
		"127.0.0.0/8",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"::1/128",
		"fe80::/10",
		"fc00::/7",
		"0.0.0.0/8",
		"100.64.0.0/10",
		"198.18.0.0/15",
		"224.0.0.0/4",
	} {
		_, network, _ := net.ParseCIDR(cidr)
		privateNets = append(privateNets, network)
	}
}


// tunnelOverlayKeyHash computes SHA256(TL-serialize(adnlTunnel.overlayKey{paymentNode}))
// without relying on the TL registry, avoiding conflicts between wsbridge.OverlayKey
// and tunnel.OverlayKey when both packages are loaded.
func tunnelOverlayKeyHash(paymentNode []byte) []byte {
	// TL: 4-byte LE CRC32 of schema + 32-byte int256
	// CRC32("adnlTunnel.overlayKey paymentNode:int256 = adnlTunnel.OverlayKey") = 0xf59313e9
	var buf [36]byte
	binary.LittleEndian.PutUint32(buf[:4], 0xf59313e9)
	copy(buf[4:], paymentNode)
	h := sha256.Sum256(buf[:])
	return h[:]
}

// decodeBase64 decodes base64 with or without padding
func decodeBase64(s string) ([]byte, error) {
	// Add padding if missing
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	return base64.StdEncoding.DecodeString(s)
}

// parseAddress safely parses a TON address.
func parseAddress(raw string) (*address.Address, error) {
	return address.ParseAddr(raw)
}

// serializeStack converts a TVM stack tuple into JSON-serializable values.
func serializeStack(tuple []any) []any {
	var result []any
	for _, item := range tuple {
		switch v := item.(type) {
		case *big.Int:
			result = append(result, v.String())
		case *cell.Cell:
			boc := v.ToBOCWithFlags(false)
			result = append(result, base64.StdEncoding.EncodeToString(boc))
		case *cell.Slice:
			if v != nil {
				c, err := v.ToCell()
				if err != nil {
					result = append(result, nil)
				} else {
					boc := c.ToBOCWithFlags(false)
					result = append(result, base64.StdEncoding.EncodeToString(boc))
				}
			} else {
				result = append(result, nil)
			}
		case nil:
			result = append(result, nil)
		default:
			result = append(result, fmt.Sprintf("%v", v))
		}
	}
	if result == nil {
		result = []any{}
	}
	return result
}

// serializeTransaction converts a TLB Transaction into a JSON-serializable map.
func serializeTransaction(tx *tlb.Transaction) map[string]any {
	txMap := map[string]any{
		"hash":        hex.EncodeToString(tx.Hash),
		"lt":          fmt.Sprintf("%d", tx.LT),
		"now":         tx.Now,
		"total_fees":  tx.TotalFees.Coins.Nano().String(),
		"prev_tx_lt":  fmt.Sprintf("%d", tx.PrevTxLT),
		"prev_tx_hash": hex.EncodeToString(tx.PrevTxHash),
	}

	if tx.IO.In != nil {
		txMap["in_msg"] = serializeMessage(tx.IO.In)
	}

	if tx.IO.Out != nil {
		outMsgs, err := tx.IO.Out.ToSlice()
		if err == nil {
			var outs []map[string]any
			for i := range outMsgs {
				outs = append(outs, serializeMessage(&outMsgs[i]))
			}
			txMap["out_msgs"] = outs
		} else {
			txMap["out_msgs"] = []any{}
		}
	} else {
		txMap["out_msgs"] = []any{}
	}

	return txMap
}

// serializeContentBase builds a JSON-serializable map from nft.ContentAny, extracting the
// common attributes shared by NFTs and jettons. Callers can add extra fields on top.
func serializeContentBase(content nft.ContentAny) map[string]any {
	if content == nil {
		return map[string]any{"type": "unknown"}
	}
	switch c := content.(type) {
	case *nft.ContentOffchain:
		return map[string]any{
			"type": "offchain",
			"uri":  c.URI,
		}
	case *nft.ContentSemichain:
		result := map[string]any{
			"type":        "semichain",
			"uri":         c.URI,
			"name":        c.GetAttribute("name"),
			"description": c.GetAttribute("description"),
			"image":       c.GetAttribute("image"),
		}
		if imgData := c.GetAttributeBinary("image_data"); len(imgData) > 0 {
			result["image_data"] = base64.StdEncoding.EncodeToString(imgData)
		}
		return result
	case *nft.ContentOnchain:
		result := map[string]any{
			"type":        "onchain",
			"name":        c.GetAttribute("name"),
			"description": c.GetAttribute("description"),
			"image":       c.GetAttribute("image"),
		}
		if imgData := c.GetAttributeBinary("image_data"); len(imgData) > 0 {
			result["image_data"] = base64.StdEncoding.EncodeToString(imgData)
		}
		return result
	default:
		return map[string]any{"type": "unknown"}
	}
}

// serializeJettonContent returns a JSON-serializable map with full TEP-64 jetton metadata.
func serializeJettonContent(content nft.ContentAny) map[string]any {
	result := serializeContentBase(content)
	// Jettons additionally expose symbol and decimals attributes.
	switch c := content.(type) {
	case *nft.ContentSemichain:
		result["symbol"] = c.GetAttribute("symbol")
		result["decimals"] = c.GetAttribute("decimals")
	case *nft.ContentOnchain:
		result["symbol"] = c.GetAttribute("symbol")
		result["decimals"] = c.GetAttribute("decimals")
	}
	return result
}

// serializeMessage converts a TLB Message into a JSON-serializable map.
func serializeMessage(msg *tlb.Message) map[string]any {
	m := map[string]any{}

	if msg.Msg.SenderAddr() != nil {
		m["source"] = msg.Msg.SenderAddr().String()
	} else {
		m["source"] = ""
	}

	if msg.Msg.DestAddr() != nil {
		m["destination"] = msg.Msg.DestAddr().String()
	} else {
		m["destination"] = ""
	}

	// Extract value for internal messages
	if intMsg, ok := msg.Msg.(*tlb.InternalMessage); ok {
		m["value"] = intMsg.Amount.Nano().String()
	} else {
		m["value"] = "0"
	}

	// Serialize body as base64 BOC
	if payload := msg.Msg.Payload(); payload != nil {
		boc := payload.ToBOCWithFlags(false)
		m["body"] = base64.StdEncoding.EncodeToString(boc)
	} else {
		m["body"] = ""
	}

	return m
}
