package wsbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/xssnick/tonutils-go/adnl/address"
	"github.com/xssnick/tonutils-go/adnl/dht"
	"github.com/xssnick/tonutils-go/adnl/keys"
	"github.com/xssnick/tonutils-go/adnl/overlay"
)

func (b *WSBridge) handleFindAddresses(client *wsClient, req *WSRequest) {
	var params struct {
		Key string `json:"key"` // base64-encoded ADNL ID
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	keyBytes, err := decodeBase64(params.Key)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 key: "+err.Error(), -32602)
		return
	}
	if len(keyBytes) != 32 {
		b.sendError(client, req.ID, "key must be 32 bytes", -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.DHT.Timeout)
	defer cancel()

	addrs, pubKey, err := b.dht.FindAddresses(ctx, keyBytes)
	if err != nil {
		b.sendError(client, req.ID, "dht lookup failed: "+err.Error())
		return
	}

	type addrResult struct {
		IP   string `json:"ip"`
		Port int32  `json:"port"`
	}

	results := []addrResult{}
	for _, addr := range addrs.Addresses {
		results = append(results, addrResult{
			IP:   addr.IP.String(),
			Port: addr.Port,
		})
	}

	b.sendResult(client, req.ID, map[string]any{
		"addresses": results,
		"pubkey":    base64.StdEncoding.EncodeToString(pubKey),
	})
}

func (b *WSBridge) handleFindOverlayNodes(client *wsClient, req *WSRequest) {
	var params struct {
		OverlayKey string `json:"overlay_key"` // base64-encoded overlay key
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	keyBytes, err := decodeBase64(params.OverlayKey)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 key: "+err.Error(), -32602)
		return
	}
	if len(keyBytes) != 32 {
		b.sendError(client, req.ID, "overlay_key must be 32 bytes", -32602)
		return
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.DHT.Timeout)
	defer cancel()

	nodesList, _, err := b.dht.FindOverlayNodes(ctx, keyBytes)
	if err != nil {
		b.sendError(client, req.ID, "overlay lookup failed: "+err.Error())
		return
	}

	type overlayNodeInfo struct {
		ID      string `json:"id"`
		Overlay string `json:"overlay"`
		Version int32  `json:"version"`
	}
	var nodes []overlayNodeInfo
	if nodesList != nil {
		for _, node := range nodesList.List {
			id, ok := node.ID.(keys.PublicKeyED25519)
			if !ok {
				continue
			}
			nodes = append(nodes, overlayNodeInfo{
				ID:      base64.StdEncoding.EncodeToString(id.Key),
				Overlay: base64.StdEncoding.EncodeToString(node.Overlay),
				Version: node.Version,
			})
		}
	}
	if nodes == nil {
		nodes = []overlayNodeInfo{}
	}

	b.sendResult(client, req.ID, map[string]any{
		"nodes": nodes,
		"count": len(nodes),
	})
}

func (b *WSBridge) handleFindTunnelNodes(client *wsClient, req *WSRequest) {
	// Compute the overlay key the same way adnl-tunnel does:
	// SHA256(TL-serialize(OverlayKey{PaymentNode: [0...0]})) for free relays.
	// We serialize manually to avoid TL registry conflicts between
	// wsbridge.OverlayKey and tunnel.OverlayKey.
	overlayKey := tunnelOverlayKeyHash(make([]byte, 32))

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.DHT.TunnelTimeout)
	defer cancel()

	// Query with continuation (up to 3 rounds) to collect all registered nodes
	var cont *dht.Continuation
	seen := make(map[string]bool)

	type relayInfo struct {
		ADNL    string `json:"adnl_id"`
		Version int32  `json:"version"`
	}
	relays := []relayInfo{}

	for i := 0; i < 3; i++ {
		nodesList, c, err := b.dht.FindOverlayNodes(ctx, overlayKey, cont)
		if err != nil {
			if i == 0 {
				b.sendError(client, req.ID, "tunnel relay lookup failed: "+err.Error())
				return
			}
			break
		}
		if nodesList != nil {
			for _, node := range nodesList.List {
				id, ok := node.ID.(keys.PublicKeyED25519)
				if !ok {
					continue
				}
				key := base64.StdEncoding.EncodeToString(id.Key)
				if seen[key] {
					continue
				}
				seen[key] = true
				relays = append(relays, relayInfo{
					ADNL:    key,
					Version: node.Version,
				})
			}
		}
		if c == nil {
			break
		}
		cont = c
	}

	b.sendResult(client, req.ID, map[string]any{
		"relays": relays,
		"count":  len(relays),
	})
}

func (b *WSBridge) handleDHTFindValue(client *wsClient, req *WSRequest) {
	var params struct {
		KeyID string `json:"key_id"` // base64
		Name  string `json:"name"`
		Index int32  `json:"index"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	keyID, err := decodeBase64(params.KeyID)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 key_id: "+err.Error(), -32602)
		return
	}
	if len(keyID) != 32 {
		b.sendError(client, req.ID, "key_id must be 32 bytes", -32602)
		return
	}

	key := &dht.Key{
		ID:    keyID,
		Name:  []byte(params.Name),
		Index: params.Index,
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.DHT.Timeout)
	defer cancel()

	value, _, err := b.dht.FindValue(ctx, key)
	if err != nil {
		b.sendError(client, req.ID, "dht find value failed: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]any{
		"data": base64.StdEncoding.EncodeToString(value.Data),
		"ttl":  value.TTL,
	})
}

func (b *WSBridge) handleDHTStoreAddress(client *wsClient, req *WSRequest) {
	if !b.cfg.Namespaces.DHT.AllowWrite {
		log.Warn().Str("method", "dht.storeAddress").Msg("restricted method called")
		b.sendError(client, req.ID, "dht.storeAddress is disabled (set namespaces.dht.allow_write in config)", -32603)
		return
	}

	var params struct {
		Addresses []struct {
			IP   string `json:"ip"`
			Port int32  `json:"port"`
		} `json:"addresses"`
		TTL      int `json:"ttl"`      // seconds
		Replicas int `json:"replicas"` // number of DHT nodes to store on
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}
	if len(params.Addresses) == 0 {
		b.sendError(client, req.ID, "addresses list must not be empty", -32602)
		return
	}

	addrList := address.List{
		Addresses: make([]*address.UDP, 0, len(params.Addresses)),
	}
	for _, a := range params.Addresses {
		ip := net.ParseIP(a.IP)
		if ip == nil {
			b.sendError(client, req.ID, "invalid IP address: "+a.IP, -32602)
			return
		}
		ip4 := ip.To4()
		if ip4 == nil {
			b.sendError(client, req.ID, "only IPv4 addresses are supported", -32602)
			return
		}
		if b.cfg.Namespaces.ADNL.SSRFProtection && isPrivateIP(ip) {
			b.sendError(client, req.ID, "private/loopback addresses not allowed", -32602)
			return
		}
		addrList.Addresses = append(addrList.Addresses, &address.UDP{
			IP:   ip4,
			Port: a.Port,
		})
	}

	ttl := 5 * time.Minute
	if params.TTL > 0 {
		ttl = time.Duration(params.TTL) * time.Second
	}
	replicas := 3
	if params.Replicas > 0 {
		replicas = params.Replicas
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.DHT.Timeout)
	defer cancel()

	made, idKey, err := b.dht.StoreAddress(ctx, addrList, ttl, b.key, replicas)
	if err != nil {
		b.sendError(client, req.ID, "dht store address failed: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]any{
		"stored":   true,
		"replicas": made,
		"id_key":   base64.StdEncoding.EncodeToString(idKey),
	})
}

func (b *WSBridge) handleDHTStoreOverlayNodes(client *wsClient, req *WSRequest) {
	if !b.cfg.Namespaces.DHT.AllowWrite {
		log.Warn().Str("method", "dht.storeOverlayNodes").Msg("restricted method called")
		b.sendError(client, req.ID, "dht.storeOverlayNodes is disabled (set namespaces.dht.allow_write in config)", -32603)
		return
	}

	var params struct {
		OverlayKey string `json:"overlay_key"` // base64 32 bytes
		Nodes      []struct {
			ID        string `json:"id"`        // base64 ed25519 public key
			Overlay   string `json:"overlay"`   // base64 overlay ID
			Version   int32  `json:"version"`
			Signature string `json:"signature"` // base64 node signature
		} `json:"nodes"`
		TTL      int `json:"ttl"`      // seconds
		Replicas int `json:"replicas"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		b.sendError(client, req.ID, "invalid params: "+err.Error(), -32602)
		return
	}

	overlayKeyBytes, err := decodeBase64(params.OverlayKey)
	if err != nil {
		b.sendError(client, req.ID, "invalid base64 overlay_key: "+err.Error(), -32602)
		return
	}
	if len(overlayKeyBytes) != 32 {
		b.sendError(client, req.ID, "overlay_key must be 32 bytes", -32602)
		return
	}
	if len(params.Nodes) == 0 {
		b.sendError(client, req.ID, "nodes list must not be empty", -32602)
		return
	}

	nodesList := &overlay.NodesList{
		List: make([]overlay.Node, 0, len(params.Nodes)),
	}
	for _, n := range params.Nodes {
		idBytes, err := decodeBase64(n.ID)
		if err != nil {
			b.sendError(client, req.ID, "invalid base64 node id: "+err.Error(), -32602)
			return
		}
		overlayBytes, err := decodeBase64(n.Overlay)
		if err != nil {
			b.sendError(client, req.ID, "invalid base64 node overlay: "+err.Error(), -32602)
			return
		}
		sigBytes, err := decodeBase64(n.Signature)
		if err != nil {
			b.sendError(client, req.ID, "invalid base64 node signature: "+err.Error(), -32602)
			return
		}
		nodesList.List = append(nodesList.List, overlay.Node{
			ID:        keys.PublicKeyED25519{Key: idBytes},
			Overlay:   overlayBytes,
			Version:   n.Version,
			Signature: sigBytes,
		})
	}

	ttl := 5 * time.Minute
	if params.TTL > 0 {
		ttl = time.Duration(params.TTL) * time.Second
	}
	replicas := 3
	if params.Replicas > 0 {
		replicas = params.Replicas
	}

	ctx, cancel := context.WithTimeout(client.ctx, b.cfg.Namespaces.DHT.Timeout)
	defer cancel()

	made, idKey, err := b.dht.StoreOverlayNodes(ctx, overlayKeyBytes, nodesList, ttl, replicas)
	if err != nil {
		b.sendError(client, req.ID, "dht store overlay nodes failed: "+err.Error())
		return
	}

	b.sendResult(client, req.ID, map[string]any{
		"stored":   true,
		"replicas": made,
		"id_key":   base64.StdEncoding.EncodeToString(idKey),
	})
}
