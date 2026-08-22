# Configuration

The bridge stores its configuration in `config.json` under `--data-dir`.

The file uses schema version 2. Version 1 files are migrated on startup. Missing values receive their defaults.

## Command line

Command-line options override `config.json`.

| Option | Default | Purpose |
|--------|---------|---------|
| `--addr` | `127.0.0.1:8081` | WebSocket listen address |
| `--config` | Public TON config | TON global config file |
| `--data-dir` | `.` | Directory for `config.json` and the ADNL key |
| `--tunnel` | `0` | Tunnel sections. Use 2 or more to enable |
| `--verbosity` | `2` | Log level from 0 to 3 |

## Main fields

| Field | Default | Purpose |
|-------|---------|---------|
| `listen` | `127.0.0.1:8081` | WebSocket listen address |
| `max_clients` | `100` | Maximum WebSocket clients |
| `allowed_origins` | Localhost origins | Allowed browser origin hosts |
| `api_key` | Empty | Optional WebSocket API key |
| `websocket` | Object | WebSocket timeouts and limits |
| `namespaces` | Object | Per-namespace settings |

When `api_key` is set, connect with `?api_key=<key>`.

Set `allowed_origins` to `["*"]` to accept any browser origin. Requests without an `Origin` header are accepted.

## Limits

| Setting | Default |
|---------|---------|
| Clients | 100 |
| Message size | 1 MiB |
| Requests in flight per client | 100 |
| ADNL peers per client | 20 |
| Overlays per client | 10 |
| Subscriptions per client | 50 |
| Pending query lifetime | 30 seconds |
| Write timeout | 10 seconds |
| Ping interval | 54 seconds |
| Pong deadline | 60 seconds |

`websocket.max_inflight` must be greater than `namespaces.subscribe.max_subscriptions`. The bridge rejects an invalid configuration at startup.

## Namespaces

Each namespace has an `enabled` field and may have a `timeout` field. A disabled namespace returns error `-32601`.

Extra settings:

| Namespace | Settings |
|-----------|----------|
| `lite` | `send_wait_timeout`, `watch_timeout` |
| `subscribe` | `max_subscriptions`, `max_multi_accounts`, `max_config_params` |
| `subscribe_trace` | `max_depth`, `default_depth`, `max_msg_timeout`, `default_msg_timeout`, `max_resolvers` |
| `adnl` | `max_peers`, `query_max_timeout`, `ssrf_protection` |
| `overlay` | `max_overlays`, `query_max_timeout` |
| `dht` | `tunnel_timeout`, `allow_write` |

## Security defaults

`adnl.connect` rejects private, loopback, link-local and reserved addresses when `namespaces.adnl.ssrf_protection` is enabled.

`adnl.connectByADNL` always accepts public unicast addresses only. DHT records are untrusted.

`dht.storeAddress` and `dht.storeOverlayNodes` are disabled by default. Enable them with `namespaces.dht.allow_write`. They sign records with the persistent bridge identity.
