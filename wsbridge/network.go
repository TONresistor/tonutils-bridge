package wsbridge

func (b *WSBridge) handleNetworkInfo(client *wsClient, req *WSRequest) {
	b.mu.RLock()
	clientCount := len(b.clients)
	b.mu.RUnlock()

	dhtNodes := 0
	if b.dht != nil {
		dhtNodes = b.dht.ActiveNodesCount()
	}
	b.sendResult(client, req.ID, map[string]any{
		"dht_initialized":  b.dht != nil,
		"dht_connected":    dhtNodes > 0,
		"dht_active_nodes": dhtNodes,
		"ws_clients":       clientCount,
	})
}
