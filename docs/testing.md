# Testing

## Local checks

```bash
go build ./...
go vet ./...
go test ./wsbridge/ -count=1
```

CI runs these three checks. It does not run E2E tests.

## E2E tests

Start the bridge:

```bash
./tonutils-bridge --addr 127.0.0.1:8081
```

Run E2E tests that do not send TON:

```bash
go test -tags e2e -v ./wsbridge/ -timeout 300s -skip '^TestE2E_LiteSendReal$'
```

Use another address with `WS_ADDR`:

```bash
WS_ADDR=ws://127.0.0.1:9090 go test -tags e2e -v ./wsbridge/ -timeout 300s -skip '^TestE2E_LiteSendReal$'
```

These tests use the live TON network. DHT, ADNL and Overlay peers can become unavailable.

Set `PAYMENT_CHANNEL_ADDR` to test a current `ton-payment-network` v1.3.1 channel. The test is skipped when it is not set.

## Real transfer test

`TestE2E_LiteSendReal` sends three self-transfers of 0.01 TON.

Copy `wsbridge/testdata/.wallet.json.example` to `wsbridge/testdata/.wallet.json`. Set a base64 Ed25519 seed and its wallet address. The wallet needs at least 0.05 TON.

Run only this test:

```bash
go test -tags e2e -v ./wsbridge/ -timeout 300s -run '^TestE2E_LiteSendReal$'
```

Never commit `.wallet.json`.
