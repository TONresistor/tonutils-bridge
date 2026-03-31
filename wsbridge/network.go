package wsbridge

func (b *WSBridge) handleNetworkInfo(client *wsClient, req *WSRequest) {
	b.mu.RLock()
	clientCount := len(b.clients)
	b.mu.RUnlock()

	b.sendResult(client, req.ID, map[string]any{
		"dht_connected": b.dht != nil,
		"ws_clients":    clientCount,
	})
}
