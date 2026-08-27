# tonutils-bridge

Local WebSocket bridge for TON. It exposes selected `tonutils-go` features through JSON-RPC 2.0.


## Run

```bash
go build -o tonutils-bridge .
./tonutils-bridge
```

The bridge listens on `ws://127.0.0.1:8081` by default.

Common options:

```bash
./tonutils-bridge --addr 127.0.0.1:9090
./tonutils-bridge --data-dir ./data
./tonutils-bridge --tunnel 2
```

The first run creates `config.json` and a persistent ADNL identity.

## First request

Send a JSON-RPC request over WebSocket:

```json
{"jsonrpc":"2.0","id":"1","method":"network.info","params":{}}
```

Response:

```json
{"jsonrpc":"2.0","id":"1","result":{"dht_initialized":true,"dht_connected":true,"dht_active_nodes":12,"ws_clients":1}}
```

Push events have no request ID:

```json
{"event":"block","data":{"seqno":58850000,"workchain":-1}}
```

## API

The bridge exposes 67 methods.

| Namespace | Methods |
|-----------|---------|
| `subscribe` | 8 |
| `adnl` | 9 |
| `overlay` | 9 |
| `dht` | 6 |
| `lite` | 20 |
| `jetton` | 3 |
| `nft` | 5 |
| `dns` | 1 |
| `wallet` | 2 |
| `sbt` | 2 |
| `payment` | 1 |
| `network` | 1 |

See [API reference](docs/api.md) for methods, parameters, responses and push events.

## Security

- The default listener accepts local connections only.
- Browser origins are limited to localhost by default.
- An API key can be enabled in `config.json`.
- `adnl.connect` blocks private and reserved addresses by default.
- DHT write methods are disabled by default.

See [configuration](docs/configuration.md) for all options and limits.

## Development

```bash
go build ./...
go vet ./...
go test ./wsbridge/
```

See [testing](docs/testing.md) for E2E tests and [architecture](docs/architecture.md) for the code layout.

## License

[MIT](LICENSE)
