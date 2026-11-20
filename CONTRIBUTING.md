# Contributing to the Go SDK

This page is for people changing the SDK itself. To use the SDK, start with the [README](README.md).

## How the source is built

Most of the code is generated from the Grvt API contract: models, enums, error codes, one `Client` method per
RPC and stream, and the EIP-712 sign functions. Generated files end in `_gen.go`; the generated
`internal/behavior` package holds shared defaults such as timeouts, retry counts and backoff limits. The
hand-written runtime (sockets, sessions, signing, retries) stays the same size however many APIs exist.

- Don't edit `*_gen.go` files by hand. The published module includes them, but they are regenerated from the
  contract. Send proposed changes to the Grvt maintainers so they can update the contract or generator.
- Maintainers regenerate with `make sdk-gen-go` from the root of the platform repository, before building.
- [RUNTIME_API.md](RUNTIME_API.md) is the contract between the generator and the runtime: the only names
  generated code may use. If the generator needs something new, add it to the runtime and to that page.

Rules the generator enforces:

- **Method names are flat.** Methods are not grouped by API namespace, so generation fails if two calls (or two
  streams) in different namespaces share a `method_name`.
- **Every call is marked read or write in the contract.** An unmarked call stops generation. Only reads are
  ever retried.

## Layout

```
api_gen.go              GENERATED: Client, New, one Client method per RPC/stream
specs_gen.go            GENERATED: RPCSpec / StreamSpec values
signing_gen.go          GENERATED: EIP-712 types, message builders, Sign<Id> functions
errors_gen.go           GENERATED: Code<Name> constants and the error table
models/models_gen.go    GENERATED: one struct per wire type
models/enums_gen.go     GENERATED: one string type per enum, known values, signing codes
models/wire.go          Uint64s (lists of 64-bit ints as JSON strings)
runtime.go              Call, Subscribe, signing hooks, session rotation, metadata cache access
ws.go, subscription.go  one managed socket per gateway; Subscription
protocol.go             socket state without I/O: request table, sequencer, backoff (unit-tested)
rest.go, auth.go        REST calls with read retries; API-key login, cookie + X-Grvt-Account-Id
eip712.go, signer.go    EIP-712 domain and typed data; Signer, PrivateKeySigner (external signers are verified)
eip712_encode.go        helpers for the generated encoders (zero-allocation hashing)
signing.go              wire transforms (MessageBuilder), metadata cache
checks.go               order checks against instrument limits
recovery.go, backfill.go, countdown.go, orderbook.go, pagination.go   recovery and paging helpers
config.go, env.go       Config, OrderTransport, Encoding, Environment; Ptr / Deref
hooks.go, log.go        BeforeSend / OnEvent; GRVT_LOG with redaction
errors.go, clock.go     error types; clock, nonces, client order IDs
*_test.go               signing cases, protocol, fake-gateway WS/REST, contract coverage, testnet smoke
examples/quickstart     runnable example
```

## Run the tests

```sh
go test ./...                                              # everything, offline
go test -race ./...
GRVT_SMOKE=1 go test -run TestSmokeTestnet -v .            # read-only, unauthenticated testnet check (full and lite)
go test -run '^$' -bench 'Decode|Route|Sign|Encode|Lite' -benchmem .   # hot paths, full vs lite
```

Run the example from this directory. It reads public testnet data; `--trade` places, watches and cancels one
testnet order and needs `GRVT_API_KEY`, `GRVT_PRIVATE_KEY` and `GRVT_SUB_ACCOUNT_ID`. The order can fill before
it is cancelled.

```sh
go run ./examples/quickstart
```

## Library choices

| Need | Library | Why |
|---|---|---|
| WebSocket | github.com/coder/websocket | context-aware, concurrent writes, small |
| secp256k1 | github.com/decred/dcrd/dcrec/secp256k1/v4 | RFC 6979, low-s, compact recoverable signatures; what go-ethereum uses |
| keccak | golang.org/x/crypto/sha3 | `NewLegacyKeccak256` |
| decimals | math/big and string scaling | exact; "scale by 10^n and reject over-precision" needs no decimal library |
| HTTP, JSON, logs | net/http, encoding/json, log/slog | standard library; replies and frames decode with generated one-pass decoders (`internal/wirejson`) that keep encoding/json's rules |

Versions are pinned in `go.mod`.

## Design notes

- **Go version.** During the preview the module requires the latest Go release (set in `go.mod`). This is a
  support policy, not a need for a specific new standard-library feature, and a 0.x release may raise it.
- **Own module.** The SDK has its own `go.mod`, so it stays out of the platform build (no Bazel BUILD files).
- **Session rotation is break-before-make.** After a re-login, a socket waits until no write is in flight, then
  reconnects with the new cookie. It never holds two connections at once.
- **Frame-size check.** `Environment.WSMaxFrameBytes` is 0 for the named environments because the deployed
  limit is not published. Falling back to REST for an oversized frame is a TODO: the runtime can't yet tell
  which RPCs are safe to reroute.
- **What users can't do yet** (private-stream resume, wallet login) is listed in [Not supported yet](https://developers.grvt.io/sdks/go#not-supported-yet); keep it there.
- **Client order IDs** use the wall clock, not `Config.Clock`.
