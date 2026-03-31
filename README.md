# tonutils-bridge

Standalone WebSocket gateway to the TON P2P network. Exposes liteserver queries, DHT, ADNL, overlay networks, and real-time subscriptions via JSON-RPC 2.0 over WebSocket.

Any language, any runtime. One WebSocket connection gives you access to the full TON stack.

## Quick Start

```bash
go build -o tonutils-bridge .

./tonutils-bridge                        # direct mode, WS on :8081
./tonutils-bridge --tunnel 2             # tunnel mode (IP hidden via DHT relays)
./tonutils-bridge --addr 127.0.0.1:9090  # custom port
./tonutils-bridge --data-dir ./myconfig  # custom config directory
```

On first launch, a persistent ADNL identity (ed25519 key) is generated and saved to `config.json`.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--addr` | `127.0.0.1:8081` | WebSocket listen address |
| `--config` | (fetch from network) | Path to TON global config JSON |
| `--data-dir` | `.` | Directory for config.json and ADNL key |
| `--tunnel` | `0` | Tunnel sections (0=direct, >=2 to enable) |
| `--verbosity` | `2` | Log level (0=fatal, 1=error, 2=info, 3=debug) |

## Protocol

JSON-RPC 2.0 over WebSocket.

Request:
```json
{"jsonrpc": "2.0", "id": "1", "method": "lite.getAccountState", "params": {"address": "EQ..."}}
```

Response:
```json
{"jsonrpc": "2.0", "id": "1", "result": {"balance": "1592527424082320", "status": "active"}}
```

Error:
```json
{"jsonrpc": "2.0", "id": "1", "error": {"code": -32602, "message": "invalid address"}}
```

Push event (no request ID):
```json
{"event": "block", "data": {"seqno": 58850000, "workchain": -1}}
```

Error codes: -32700 parse error, -32601 method not found, -32602 invalid params, -32603 internal error.

Origin restricted to `127.0.0.1`, `localhost`, and `::1`. Max message size 1 MB. Ping/pong keepalive (54s/60s).

### Limits

| Limit | Value |
|-------|-------|
| Max concurrent connections | 100 |
| Max message size | 1 MB |
| Max ADNL peers per client | 20 |
| Max overlays per client | 10 |
| Max subscriptions per client | 50 |
| Pending query TTL | 30s |
| Write deadline | 10s |
| Ping interval / pong deadline | 54s / 60s |

### SSRF Protection

`adnl.connect` and `adnl.connectByADNL` reject private, loopback, and reserved IP addresses.

## Methods (63)

### Subscriptions - Real-Time Push (8)

Max 50 per connection. All return `subscription_id` in the confirmation response. Cancel with `subscribe.unsubscribe`.

| Method | Params | Events pushed |
|--------|--------|---------------|
| `subscribe.transactions` | `address`, `last_lt`, `operations[]` (optional opcodes) | `transaction` |
| `subscribe.blocks` | | `block` |
| `subscribe.accountState` | `address` | `account_state` |
| `subscribe.newTransactions` | | `new_transaction` |
| `subscribe.configChanges` | `params[]` (config param IDs, 1-50 required) | `config_changed` |
| `subscribe.multiAccount` | `accounts: [{address, last_lt?, operations[]}]` (max 100) | `transaction` (with `address` field) |
| `subscribe.trace` | `address`, `last_lt?`, `max_depth` (default 3, [1-10]), `msg_timeout_sec` (default 10, [1-120]) | `trace_started`, `trace_tx`, `trace_timeout`, `trace_complete` |
| `subscribe.unsubscribe` | `subscription_id` | |

### ADNL - P2P Connections (9)

| Method | Params | Response |
|--------|--------|----------|
| `adnl.connect` | `address` (ip:port), `key` (base64) | `{connected, peer_id, remote_addr}` |
| `adnl.connectByADNL` | `adnl_id` (base64) | `{connected, peer_id, remote_addr}` |
| `adnl.sendMessage` | `peer_id`, `data` (base64) | `{sent}` |
| `adnl.ping` | `peer_id` | `{latency_ms}` |
| `adnl.disconnect` | `peer_id` | `{disconnected}` |
| `adnl.peers` | | `{peers: [{id, addr}]}` |
| `adnl.query` | `peer_id`, `data` (base64), `timeout` | `{data: "base64"}` |
| `adnl.setQueryHandler` | `peer_id` | `{enabled}` then push `adnl.queryReceived` events |
| `adnl.answer` | `query_id` (hex), `data` (base64) | `{answered}` |

### Overlay - Network Overlays (7)

| Method | Params | Response |
|--------|--------|----------|
| `overlay.join` | `overlay_id`, `peer_id` (base64) | `{joined, overlay_id}` |
| `overlay.leave` | `overlay_id` | `{left}` |
| `overlay.getPeers` | `overlay_id` | `{peers: [{id, overlay}]}` |
| `overlay.sendMessage` | `overlay_id`, `data` (base64) | `{sent}` |
| `overlay.query` | `overlay_id`, `data` (base64), `timeout` | `{data: "base64"}` |
| `overlay.setQueryHandler` | `overlay_id`, `peer_id` | `{enabled}` then push `overlay.queryReceived` events |
| `overlay.answer` | `query_id` (hex), `data` (base64) | `{answered}` |

### DHT - Distributed Hash Table (6)

| Method | Params | Response | Timeout |
|--------|--------|----------|---------|
| `dht.findAddresses` | `key` (base64, 32 bytes) | `{addresses: [{ip, port}], pubkey}` | 15s |
| `dht.findOverlayNodes` | `overlay_key` (base64) | `{nodes: [{id, overlay, version}], count}` | 15s |
| `dht.findTunnelNodes` | | `{relays: [{adnl_id, version}], count}` | 30s |
| `dht.findValue` | `key_id` (base64), `name`, `index` | `{data: "base64", ttl}` | 15s |
| `dht.storeAddress` | disabled | returns error -32603 | - |
| `dht.storeOverlayNodes` | disabled | returns error -32603 | - |

`dht.storeAddress` and `dht.storeOverlayNodes` are disabled to prevent DHT identity hijacking. Any call returns error -32603.

### Lite - Blockchain Queries (18)

| Method | Params | Response | Timeout |
|--------|--------|----------|---------|
| `lite.getMasterchainInfo` | | `{seqno, workchain, shard, root_hash, file_hash}` | 10s |
| `lite.getAccountState` | `address` | `{balance, status, last_tx_lt, last_tx_hash, has_code, has_data, code?, data?}` | 10s |
| `lite.runMethod` | `address`, `method`, `params[]` | `{exit_code, stack[]}` | 10s |
| `lite.sendMessage` | `boc` (base64) | `{hash, status}` | 10s |
| `lite.sendMessageWait` | `boc` (base64) | `{hash, status}` | 60s |
| `lite.getTransactions` | `address`, `limit`, `last_lt?`, `last_hash?` | `{transactions, incomplete}` | 10s |
| `lite.getTransaction` | `address`, `lt` | serialized transaction | 10s |
| `lite.findTxByInMsgHash` | `address`, `msg_hash` (hex) | serialized transaction | 10s |
| `lite.findTxByOutMsgHash` | `address`, `msg_hash` (hex) | serialized transaction | 10s |
| `lite.getTime` | | `{time}` | 10s |
| `lite.lookupBlock` | `workchain`, `shard` (hex), `seqno` | `{workchain, shard, seqno, root_hash, file_hash}` | 10s |
| `lite.getBlockTransactions` | `workchain`, `shard`, `seqno`, `count` | `{transactions, incomplete}` | 10s |
| `lite.getShards` | | `{shards}` | 10s |
| `lite.getBlockchainConfig` | `params[]` (optional) | `{params: {id: "base64_boc"}}` | 10s |
| `lite.getBlockData` | `workchain`, `shard`, `seqno` | `{boc}` | 10s |
| `lite.getBlockHeader` | `workchain`, `shard`, `seqno` | `{workchain, shard, seqno, root_hash, file_hash, header_boc}` | 10s |
| `lite.getLibraries` | `hashes[]` (hex) | `{libraries: [{hash, boc} or null]}` | 10s |
| `lite.sendAndWatch` | `boc` (base64) | `{watching, subscription_id, msg_hash}` then push events | 180s |

### Jetton (3)

| Method | Params | Response |
|--------|--------|----------|
| `jetton.getData` | `address` (master) | `{total_supply, mintable, admin, content}` |
| `jetton.getWalletAddress` | `jetton_master`, `owner` | `{wallet_address}` |
| `jetton.getBalance` | `jetton_wallet` | `{balance, owner, jetton_master}` |

### NFT (5)

| Method | Params | Response |
|--------|--------|----------|
| `nft.getData` | `address` (item) | `{index, collection, owner, content, initialized}` |
| `nft.getCollectionData` | `address` (collection) | `{next_item_index, owner, content}` |
| `nft.getAddressByIndex` | `collection`, `index` | `{address}` |
| `nft.getRoyaltyParams` | `collection` | `{factor, base, address}` |
| `nft.getContent` | `collection`, `index`, `individual_content` (base64 BOC) | `{content}` |

### DNS (1)

| Method | Params | Response |
|--------|--------|----------|
| `dns.resolve` | `domain` | `{wallet, site_adnl, has_storage, owner, nft_address, collection, editor, initialized, expiring_at, text_records?}` (`text_records` only present when non-empty) |

### Wallet (2)

| Method | Params | Response |
|--------|--------|----------|
| `wallet.getSeqno` | `address` | `{seqno}` |
| `wallet.getPublicKey` | `address` | `{public_key}` |

### SBT (2)

| Method | Params | Response |
|--------|--------|----------|
| `sbt.getAuthorityAddress` | `address` | `{authority}` |
| `sbt.getRevokedTime` | `address` | `{revoked_time}` |

### Payment (1)

| Method | Params | Response |
|--------|--------|----------|
| `payment.getChannelState` | `address` | `{status, initialized, balance_a, balance_b, key_a, key_b, channel_id, committed_seqno_a, committed_seqno_b, quarantine, closing_config}` |

### Network (1)

| Method | Params | Response |
|--------|--------|----------|
| `network.info` | | `{dht_connected, ws_clients}` |

## Push Events (18)

### Broadcast events (all connected clients)

| Event | Trigger | Data |
|-------|---------|------|
| `adnl.incomingConnection` | New inbound ADNL connection | `{peer_id, remote_addr}` |

### Owner-scoped events (owning client, or broadcast for inbound connections)

| Event | Trigger | Data |
|-------|---------|------|
| `adnl.message` | Incoming message from a peer | `{from, message}` (base64) |
| `adnl.disconnected` | Peer disconnected | `{peer}` (base64) |
| `adnl.queryReceived` | Inbound query (after `setQueryHandler`) | `{peer_id, query_id, data}` |
| `overlay.broadcast` | Overlay broadcast received | `{overlay_id, message, trusted}` |
| `overlay.message` | Overlay custom message received | `{overlay_id, message}` |
| `overlay.queryReceived` | Inbound overlay query | `{overlay_id, query_id, data}` |

### Subscription events (subscribing client only)

| Event | Source | Data |
|-------|--------|------|
| `transaction` | `subscribe.transactions`, `subscribe.multiAccount` | serialized transaction |
| `block` | `subscribe.blocks` | `{seqno, workchain, shard, root_hash, file_hash, shards}` |
| `account_state` | `subscribe.accountState` | `{address, balance, status, last_tx_lt, last_tx_hash, block_seqno}` |
| `new_transaction` | `subscribe.newTransactions` | `{account, lt, hash, block_workchain, block_shard, block_seqno}` |
| `config_changed` | `subscribe.configChanges` | `{param_id, block_seqno, old_value, new_value}` (base64 BOC) |
| `tx_confirmed` | `lite.sendAndWatch` | `{msg_hash, transaction, block}` |
| `tx_timeout` | `lite.sendAndWatch` | `{msg_hash, reason}` |
| `trace_started` | `subscribe.trace` | `{trace_id, root_tx, subscription_id}` |
| `trace_tx` | `subscribe.trace` | `{trace_id, transaction, depth, address}` |
| `trace_timeout` | `subscribe.trace` | `{trace_id, address, body_hash, depth}` |
| `trace_complete` | `subscribe.trace` | `{trace_id, total_txs, max_depth_reached, timed_out_count}` |

## Tests

```bash
# Unit tests (no network required)
go test ./wsbridge/

# E2E tests (requires bridge running)
./tonutils-bridge --addr 127.0.0.1:8081
go test -tags e2e -v ./wsbridge/ -timeout 300s

# Custom bridge address
WS_ADDR=ws://127.0.0.1:9090 go test -tags e2e -v ./wsbridge/
```

## Architecture

```
tonutils-bridge
  main.go           Bootstrap: config, liteserver pool, DHT, ADNL gateway, tunnel
  config.go         Persistent ADNL identity (ed25519 key in config.json)
  wsbridge/
    bridge.go       Core: WS lifecycle, dispatcher, sendEvent, limits
    subscribe.go    8 subscription methods (real-time push)
    trace.go        Transaction trace following
    adnl.go         9 ADNL P2P methods + disconnect/query handlers
    overlay.go      7 overlay methods + broadcast/query handlers
    dht.go          4 DHT find methods + 2 disabled store methods
    lite.go         18 liteserver query methods
    dns.go          DNS resolution
    jetton.go       Jetton metadata
    nft.go          NFT metadata
    wallet.go       Wallet queries
    sbt.go          SBT queries
    payment.go      Payment channel state
    network.go      Bridge status
    helpers.go      Shared utilities (address parsing, serialization, SSRF check)
```

## License

[MIT](LICENSE)
