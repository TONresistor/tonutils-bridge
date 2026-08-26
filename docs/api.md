# API

The bridge uses JSON-RPC 2.0 over WebSocket.

## Messages

Request:

```json
{"jsonrpc":"2.0","id":"1","method":"lite.getAccountState","params":{"address":"EQ..."}}
```

Response:

```json
{"jsonrpc":"2.0","id":"1","result":{"balance":"1592527424082320","status":"active"}}
```

Error:

```json
{"jsonrpc":"2.0","id":"1","error":{"code":-32602,"message":"invalid address"}}
```

| Code | Meaning |
|------|---------|
| `-32700` | Parse error |
| `-32601` | Method not found or namespace disabled |
| `-32602` | Invalid parameters |
| `-32603` | Internal error |

## Methods

### Subscriptions

Each method returns `subscription_id`. Use `subscribe.unsubscribe` to stop it.

| Method | Parameters | Events |
|--------|------------|--------|
| `subscribe.transactions` | `address`, `last_lt`, optional `operations[]` | `transaction` |
| `subscribe.blocks` | None | `block` |
| `subscribe.accountState` | `address` | `account_state` |
| `subscribe.newTransactions` | None | `new_transaction` |
| `subscribe.configChanges` | `params[]`, 1 to 50 config IDs | `config_changed` |
| `subscribe.multiAccount` | `accounts: [{address, last_lt?, operations[]}]`, max 100 | `transaction` |
| `subscribe.trace` | `address`, optional `last_lt`, `max_depth`, `msg_timeout_sec` | Trace events |
| `subscribe.unsubscribe` | `subscription_id` | None |

`subscribe.trace` accepts `max_depth` from 1 to 10. Its default is 3. `msg_timeout_sec` accepts 1 to 120 seconds. Its default is 10 seconds.

### ADNL

| Method | Parameters | Response |
|--------|------------|----------|
| `adnl.connect` | `address`, `key` | `{connected, peer_id, remote_addr}` |
| `adnl.connectByADNL` | `adnl_id` | `{connected, peer_id, remote_addr}` |
| `adnl.sendMessage` | `peer_id`, base64 `data` | `{sent}` |
| `adnl.ping` | `peer_id` | `{latency_ms}` |
| `adnl.disconnect` | `peer_id` | `{disconnected}` |
| `adnl.peers` | None | `{peers: [{id, addr}]}` |
| `adnl.query` | `peer_id`, base64 `data`, optional `timeout`, optional `raw` | `{data}` |
| `adnl.setQueryHandler` | `peer_id` | `{enabled}` |
| `adnl.answer` | `query_id`, base64 `data`, optional `raw` | `{answered}` |

`adnl.connectByADNL` tries up to 8 public DHT endpoints and checks liveness.

Queries use boxed TL data by default. Set `raw: true` to use the legacy `ws.rawMessage` wrapper. An inbound `query_id` is a 32-byte hex value. Copy the inbound `raw` value when answering.

### Overlay

| Method | Parameters | Response |
|--------|------------|----------|
| `overlay.join` | `overlay_id`, `peer_id` | `{joined, overlay_id}` |
| `overlay.leave` | `overlay_id` | `{left}` |
| `overlay.getPeers` | `overlay_id` | `{peers: [{id, adnl_id, overlay}]}` |
| `overlay.sendMessage` | `overlay_id`, base64 `data` | `{sent}` |
| `overlay.sendRaw` | `overlay_id`, base64 boxed TL `data` | `{sent}` |
| `overlay.broadcast` | `overlay_id`, base64 `data` | `{broadcast_id}` |
| `overlay.query` | `overlay_id`, base64 `data`, optional `timeout`, optional `raw` | `{data}` |
| `overlay.setQueryHandler` | `overlay_id`, `peer_id` | `{enabled}` |
| `overlay.answer` | `query_id`, base64 `data`, optional `raw` | `{answered}` |

`overlay.sendMessage` uses `ws.rawMessage`. `overlay.sendRaw` sends boxed TL data without that wrapper.

Broadcast payloads are limited to 1 MiB. Each WebSocket client may run 4 broadcasts at once. Untrusted broadcasts are delivered locally but are not relayed. The sender does not receive its own broadcast.

Overlay queries accept 1 to 8064 bytes. They use boxed TL data by default. Set `raw: true` for the legacy wrapper.

### DHT

| Method | Parameters | Response | Timeout |
|--------|------------|----------|---------|
| `dht.findAddresses` | `key`, base64 32 bytes | `{addresses: [{ip, port}], pubkey}` | 15s |
| `dht.findOverlayNodes` | `overlay_key`, base64 1 to 256 bytes | `{nodes: [{id, adnl_id, overlay, version}], count}` | 15s |
| `dht.findTunnelNodes` | None | `{relays: [{id, adnl_id, version}], count}` | 30s |
| `dht.findValue` | `key_id`, `name`, `index` | `{data, ttl}` | 15s |
| `dht.storeAddress` | `addresses[]`, optional `ttl`, optional `replicas` | `{stored, replicas, id_key, adnl_id}` | 15s |
| `dht.storeOverlayNodes` | `overlay_key`, `nodes[]`, optional `ttl`, optional `replicas` | `{stored, replicas, id_key, overlay_id}` | 15s |

`id` is the Ed25519 public key. `adnl_id` is its TL hash. Pass `adnl_id` to `adnl.connectByADNL` and `dht.findAddresses`.

DHT write methods are disabled by default. Set `namespaces.dht.allow_write` to `true` to enable them. They publish records signed by the persistent bridge identity. The `replicas` parameter is kept for compatibility and is ignored.

### Lite

| Method | Parameters | Response | Timeout |
|--------|------------|----------|---------|
| `lite.getMasterchainInfo` | None | `{seqno, workchain, shard, root_hash, file_hash}` | 10s |
| `lite.getAccountState` | `address` | Account state and optional code and data | 10s |
| `lite.runMethod` | `address`, `method`, `params[]` | `{exit_code, stack[]}` | 10s |
| `lite.emulateMessage` | `address`, `boc`, optional `type`, optional `amount` | Emulation result | 10s |
| `lite.emulateTransaction` | `address`, `boc`, optional `ignore_chksig` | Transaction emulation and fees | 10s |
| `lite.sendMessage` | `boc` | `{hash, status}` | 10s |
| `lite.sendMessageWait` | `boc` | `{hash, status}` | 60s |
| `lite.getTransactions` | `address`, `limit`, optional `last_lt`, optional `last_hash` | `{transactions}` | 10s |
| `lite.getTransaction` | `address`, `lt` | Serialized transaction | 10s |
| `lite.findTxByInMsgHash` | `address`, `msg_hash` | Serialized transaction | 10s |
| `lite.findTxByOutMsgHash` | `address`, `msg_hash` | Serialized transaction | 10s |
| `lite.getTime` | None | `{time}` | 10s |
| `lite.lookupBlock` | `workchain`, `shard`, `seqno` | Block ID | 10s |
| `lite.getBlockTransactions` | `workchain`, `shard`, `seqno`, `count`, optional `after` | `{transactions, incomplete, next_after}` | 10s |
| `lite.getShards` | None | `{shards}` | 10s |
| `lite.getBlockchainConfig` | Optional `params[]` | `{params}` | 10s |
| `lite.getBlockData` | `workchain`, `shard`, `seqno` | `{boc}` | 10s |
| `lite.getBlockHeader` | `workchain`, `shard`, `seqno` | Block ID and `header_boc` | 10s |
| `lite.getLibraries` | `hashes[]` | `{libraries}` | 10s |
| `lite.sendAndWatch` | `boc` | Watch ID and message hash | 180s |

`lite.sendMessageWait` waits longer for the liteserver response. It does not wait for on-chain confirmation.

`lite.emulateMessage` runs compute and action logic without broadcasting. `lite.emulateTransaction` runs the full transaction phases and supports existing uninitialized accounts when the message carries their `StateInit`. Both use verified account and config state. Some block context remains synthetic, so on-chain results may differ.

Set `ignore_chksig` to `true` only for local fee estimation. It makes signature checks succeed during that emulation call, defaults to `false`, and never changes `lite.sendMessage` or any broadcast path.

### Tokens and contracts

| Method | Parameters | Response |
|--------|------------|----------|
| `jetton.getData` | `address` | `{total_supply, mintable, admin, content}` |
| `jetton.getWalletAddress` | `jetton_master`, `owner` | `{wallet_address}` |
| `jetton.getBalance` | `jetton_wallet` | `{balance, owner, jetton_master}` |
| `nft.getData` | `address` | NFT item data |
| `nft.getCollectionData` | `address` | NFT collection data |
| `nft.getAddressByIndex` | `collection`, `index` | `{address}` |
| `nft.getRoyaltyParams` | `collection` | `{factor, base, address}` |
| `nft.getContent` | `collection`, `index`, `individual_content` | `{content}` |
| `dns.resolve` | `domain` | DNS, wallet, site, storage and NFT data |
| `wallet.getSeqno` | `address` | `{seqno}` |
| `wallet.getPublicKey` | `address` | `{public_key}` |
| `sbt.getAuthorityAddress` | `address` | `{authority}` |
| `sbt.getRevokedTime` | `address` | `{revoked_time}` |
| `payment.getChannelState` | `address` | Payment channel state |
| `network.info` | None | `{dht_initialized, dht_connected, dht_active_nodes, ws_clients}` |

`dns.resolve` may return `wallet`, `site_adnl`, `storage_bag_id`, `has_storage`, `owner`, `nft_address`, `collection`, `editor`, `initialized`, `expiring_at` and `text_records`.

## Push events

### Global

| Event | Data |
|-------|------|
| `adnl.incomingConnection` | `{peer_id, remote_addr}` |

### Owner scoped

| Event | Data |
|-------|------|
| `adnl.message` | `{from, message}` |
| `adnl.disconnected` | `{peer}` |
| `adnl.queryReceived` | `{peer_id, query_id, data, raw}` |
| `overlay.broadcast` | `{overlay_id, message, trusted}` |
| `overlay.message` | `{overlay_id, message}` |
| `overlay.queryReceived` | `{overlay_id, query_id, data, raw}` |

Inbound connections have no owner. Their events are sent to all clients.

### Subscription scoped

| Event | Source | Data |
|-------|--------|------|
| `transaction` | Transaction subscriptions | Serialized transaction |
| `block` | `subscribe.blocks` | Block ID and shards |
| `account_state` | `subscribe.accountState` | Account state and block seqno |
| `new_transaction` | `subscribe.newTransactions` | Account, transaction and block IDs |
| `config_changed` | `subscribe.configChanges` | Old and new base64 BOCs |
| `tx_confirmed` | `lite.sendAndWatch` | Message hash, transaction and block |
| `tx_timeout` | `lite.sendAndWatch` | Message hash and reason |
| `trace_started` | `subscribe.trace` | Trace ID, root transaction and subscription ID |
| `trace_tx` | `subscribe.trace` | Trace ID, transaction, depth and address |
| `trace_timeout` | `subscribe.trace` | Trace ID, address, message hash, body hash and depth |
| `trace_complete` | `subscribe.trace` | Counts and maximum depth |

`trace_timeout.body_hash` is kept for compatibility. Use `message_hash` for full message matching.
