# Architecture

## Runtime

```text
WebSocket client
  -> JSON-RPC dispatcher
  -> tonutils-go clients
  -> TON liteservers, DHT and ADNL peers
```

`main.go` loads the configuration and network config. It creates the liteserver pool, DNS client, DHT client and ADNL gateway. Direct and tunnel modes use the same bridge API.

`wsbridge/bridge.go` owns the WebSocket server, request dispatcher, client limits and event delivery.

Namespace files implement the RPC handlers:

```text
wsbridge/adnl.go
wsbridge/overlay.go
wsbridge/dht.go
wsbridge/lite.go
wsbridge/subscribe.go
wsbridge/trace.go
wsbridge/dns.go
wsbridge/jetton.go
wsbridge/nft.go
wsbridge/wallet.go
wsbridge/sbt.go
wsbridge/payment.go
wsbridge/network.go
```

## Identities

The WebSocket ADNL gateway uses the persistent key from `config.json`.

The DHT client uses an ephemeral key. DHT write methods use the persistent bridge key when enabled.

## Client ownership

Each WebSocket client owns the outbound ADNL peers, overlays and subscriptions it creates. Another client cannot operate on them.

Closing a WebSocket connection stops its subscriptions and closes its peers and overlays.

Inbound ADNL connections have no owner. Their events are sent to all connected clients.
