# grvt (Go SDK, preview)

> **Preview.** This SDK is still being stabilised. Its API can change in any 0.x release, so pin an exact
> version. It becomes stable at 1.0.0.

The Go SDK for Grvt: REST, WebSocket streams, WebSocket order entry, EIP-712 signing and login in one
package. It mirrors the Python and Rust SDKs in `sdk/languages/python` and `sdk/languages/rust`: same operations, same
behaviour. Everything that grows with the API (models, enums, error codes, RPC/stream methods, sign
functions) is generated from `sdk/contract/contract.json` by `sdk/generator/golang` into `*_gen.go`
files; the hand-written runtime stays the same size however many APIs exist.

It is its own Go module (`github.com/gravity-technologies/grvt-sdk-go`), outside the platform module.
The required Go version is in `go.mod`. During preview, we intentionally use the latest Go release
for both development and consumers (currently Go 1.27.4). This is a support policy, not a claim that
the SDK needs a specific new standard-library feature. We may raise this requirement in a 0.x release.

## Quickstart

Use the [canonical quickstart](https://developers.grvt.io/guides/quickstart) and
[download its Go example](https://developers.grvt.io/downloads/examples/go/examples/quickstart/main.go).
The example reads public testnet data by default. Add `--trade` to place, observe, and cancel one testnet order.
Trading requires `GRVT_API_KEY`, `GRVT_PRIVATE_KEY`, and `GRVT_SUB_ACCOUNT_ID`. An order can fill before cancellation.
The example uses current instrument limits and stops after bounded observation attempts.

From this SDK's directory, run:

```sh
go run ./examples/quickstart
```

Every RPC is a method directly on the client, `client.<Method>(ctx, *Request) (*Response, error)`, and every
stream is `client.<Method>Stream(ctx, *Selector) (*Subscription[Feed], error)`. Names are the contract's
`method_name` in Go style (`create_order` -> `client.CreateOrder`,
the `trade` stream -> `client.TradeStream`). The contract's namespace is only a docs category and never
appears in your code. Model types keep the contract names
(`models.ApiCreateOrderRequest`) and their JSON tags are the wire field names. Required fields are
checked before anything is signed or sent.

Errors are distinct types for `errors.As`: `*grvt.APIError` (`Code`, `Message`, `Status`, `RPC`,
`Retryable`), `*grvt.RateLimitError` (also matches `*APIError` with code 1006),
`*grvt.TransportError`, `*grvt.OrderStateUnknownError` (`ClientOrderID`, `ClientOrderIDs`), `*grvt.StreamOverflowError`,
`*grvt.SigningError`, `*grvt.ConfigError`, `*grvt.DecodeError`, `*grvt.FrameTooLargeError`, and
`grvt.ErrClosed`. Error code constants are `grvt.Code<Name>` (`grvt.CodeSignatureExpired`).

## Generated source

The public source repo includes the generated code and shared test fixtures. For development in the platform
monorepo, run `make sdk-gen-go` from its root before building this SDK. Generated source comes from the
Grvt API contract. Send proposed changes to the Grvt maintainers so they can update the monorepo.

## Layout

```
api_gen.go              GENERATED: Client, New, one Client method per RPC/stream
specs_gen.go            GENERATED: RPCSpec / StreamSpec values (RPCs, Streams)
signing_gen.go          GENERATED: EIP-712 types, message builders, Sign<Id> functions
errors_gen.go           GENERATED: Code<Name> constants and the error table
api_gen_test.go         GENERATED: every RPC and stream is reachable through the client
models/models_gen.go    GENERATED: one struct per wire type
models/enums_gen.go     GENERATED: one string type per enum, known values, signing codes
models/wire.go          Uint64s (lists of 64-bit ints as JSON strings)
runtime.go              Runtime: Call, Subscribe, signing hooks, session rotation, metadata cache access
ws.go, subscription.go  one managed socket per gateway; Subscription
protocol.go             sans-IO state: request table, sequencer, backoff (unit-tested)
rest.go, auth.go        REST calls with read retries; api-key login, cookie + X-Grvt-Account-Id
eip712.go, signer.go    EIP-712 domain + typed data; Signer + PrivateKeySigner (external signers are verified)
eip712_encode.go        helpers for the generated encoders (zero-allocation hashing)
signing.go              wire transforms (MessageBuilder), metadata cache
config.go, env.go       Config, OrderTransport, Encoding, Environment, specs; Ptr / Deref
hooks.go, log.go        BeforeSend / OnEvent; GRVT_LOG with redaction
errors.go, clock.go     error types; clock, nonces, client order ids
RUNTIME_API.md          the contract between generator (sdk/generator/golang) and runtime
*_test.go               signing cases, protocol, fake-gateway WS/REST, contract coverage, testnet smoke
examples/quickstart     runnable example
```

## Run the tests

```
go test ./...                                              # everything offline
go test -race ./...
GRVT_SMOKE=1 go test -run TestSmokeTestnet -v .            # read-only, unauthenticated testnet check (full and lite)
GRVT_TEST_ENCODING=lite go test .                          # every fake-gateway test with a lite client
go test -run '^$' -bench 'Decode|Route|Sign|Encode|Lite' -benchmem .   # hot paths, full vs lite
```

## How the main pieces behave

Same as Python and Rust:

- **Order transport** is per client: `OrderTransportREST` (default) or `OrderTransportWS`. Writes are
  never retried. If a write may have reached the exchange but no answer came back (timeout, socket
  drop, cancelled context), the call returns `*OrderStateUnknownError` with the client order id; look
  the order up (`Order.GetOrder` with `ClientOrderID`), don't resend.
- **Encoding** is per client: `EncodingLite` (default) or `EncodingFull`, the gateways' short field
  names, for both REST and WebSocket (order book frames are about a quarter smaller on testnet). Only the
  wire changes: models, errors and signing are the same, and login stays full. Lite WebSockets use
  `MarketDataWS` / `TradingWS` with `/ws/full` swapped for `/ws/lite`; set `MarketDataWSLite` /
  `TradingWSLite` on a custom `Environment` only when its URLs don't end in `/ws/full`.
- **Reads** are tried up to 3 times in all, with full-jitter backoff, on retryable errors (code 1006,
  a code the contract marks retryable, HTTP 502/503/504, transport errors).
- **Signing**: `CreateOrder` fills `client_order_id` and the signature; signing loads instrument and
  currency metadata once (cached). Over-precise decimals are a `*SigningError`, never rounded. Signed
  calls work on a copy, so your request is not changed.
- **Order checks**: before signing, each leg is checked against its instrument's tick, size step,
  `min_size` and `min_notional`. A miss returns `*OrderOutOfBoundsError` with the nearest valid values;
  nothing is sent and nothing is rounded. If the exchange still rejects an order for these limits, the
  instrument's cached metadata is dropped so the next order refetches it. `Config.CheckOrders:
  grvt.Ptr(false)` turns the check off and leaves the limits to the exchange.
- **Streams** use global sequence numbers. The snapshot frame has `sequence_number` 0, and the
  first update's `prev_sequence_number` is at or below the subscribe reply's `latest_sequence_number`. The
  first frame after a (re)subscribe reply is accepted
  when its `prev_sequence_number` is 0 or not greater than the latest sequence number known then
  (testnet sends the old sequence number there). After that, `prev_sequence_number` must equal the
  last sequence number seen; otherwise it is a gap: the runtime re-subscribes the same selector and
  drops frames until the new snapshot. No snapshot within 5s -> reconnect. On reconnect every
  subscription is re-sent.
- **Backpressure**: the read loop never waits for a consumer; each subscription has a bounded queue.
  Only snapshot streams (every frame complete, `*.s`) drop the oldest frame when the consumer falls
  behind. Every other stream (deltas, events, private) returns `*StreamOverflowError` and re-syncs.
- **Session**: api-key login on edge gives the `gravity` cookie and `X-Grvt-Account-Id`; both go on
  REST calls and the WS handshake. The client re-logs in at 80% of the cookie lifetime and sockets
  reconnect with the new cookie.
- **Hooks**: `Config.BeforeSend(*OutgoingCall)` runs before signing and may edit `call.Params` (the
  typed request) in place. `Config.OnEvent(Event)` gets connect, disconnect, reconnect, subscribe,
  gap, retry, frame, session_rotated and error events; it runs on the read goroutine and must not
  block.
- **Logging**: `log/slog`. `GRVT_LOG=debug` logs to stderr unless `Config.Logger` is set. Cookie, api
  key, private keys and r/s are redacted.

## Library choices

| need | library | why |
|---|---|---|
| WebSocket | github.com/coder/websocket v1.8.15 | context-aware, concurrent writes, small; already in the platform module |
| secp256k1 | github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.1 | RFC 6979, low-s, compact recoverable signatures; what go-ethereum uses |
| keccak | golang.org/x/crypto/sha3 v0.58.0 | `NewLegacyKeccak256` |
| decimals | math/big + string scaling | exact; no decimal library needed for "scale by 10^n and reject over-precision" |
| HTTP, JSON, logs | net/http, encoding/json, log/slog | standard library; replies and frames decode with generated one-pass decoders (`internal/wirejson`) that keep encoding/json's rules |

## Draft notes (shortcuts and TODOs)

- **Optional fields**: strings and enums use `""` for absent; every other scalar is a pointer (`nil` =
  absent) so `false` and `0` can be sent. Use `grvt.Ptr(...)`. Optional struct fields are pointers. Required fields use native values.
  Decoding rejects missing required keys, while nullable arrays accept `null`. Signed calls use `Input`
  models for SDK-filled fields. Only contract-declared partial streams use `Partial` payloads.
- **64-bit ints** use encoding/json's `,string` option: they are sent as strings and must arrive as
  strings (Rust also accepts numbers and hex). Lists of them (`models.Uint64s`) accept both.
- **Exact values:** decimal fields use `models.Decimal`. `ParseDecimal` validates text; `Rat()` returns an
  exact `big.Rat` for arithmetic. Use `MustParseDecimal` only for known constants. Timestamps use native
  `time.Time`; the generated JSON codec preserves Unix nanoseconds and rejects values outside the wire range.
- **Re-subscribe quirk**: the first delta after a subscribe ack may carry a `prev_sequence_number`
  from before that ack (seen on testnet after a re-subscribe on the same socket). The sequencer
  accepts any prev at or below `latest_sequence_number` for that first delta only; a prev past it is
  still a gap. Python and Rust use the same first-frame rule.
- **Flat names must be unique**: methods are not grouped by namespace, so the generator fails if two calls
  (or two streams) in different namespaces share a `method_name`.
- **Session rotation on WS is simplified** (break-before-make, as in Python and Rust): after a re-login
  a socket waits for no writes in flight, then reconnects with the new cookie.
- **Frame-size check exists but is off** (`Environment.WSMaxFrameBytes` is 0 for named environments);
  when set, an oversized frame is `*FrameTooLargeError` and nothing is sent. REST fallback is a TODO.
- **Book deltas are never conflated.** The local `OrderBook` helper merges levels.
  Queue overflow invalidates continuity. See [rebuild a book after a gap](https://developers.grvt.io/guides/recovery#rebuild-a-book-after-a-gap).
- **Private-stream resume** is not possible until the server supports it; callers backfill via REST.
- **Read/write access is explicit in the contract.** Unclassified calls stop generation; only reads can retry.
- **Signing scope** matches Python and Rust: Order / OrderWithBuilderFee (also each order of
  `BulkOrders`, signed on its own), Transfer V1-V3, Withdrawal V1-V2, WalletLogin. Algo orders, vault
  and session keys are not bound in the contract.
- **Authentication belongs to the session runtime.** Set `Config.APIKey` on the client to log in.
  Raw edge login routes are not generated client methods; wallet session login is not supported.
- **Client order ids** use the wall clock (`TimeCounterClientOrderID`), not `Config.Clock`.
- **`Config.HTTPClient`** is also used for the WebSocket handshake, so do not set `Timeout` on it.
- No Bazel BUILD files; the module's own `go.mod` keeps it out of the platform build.

## Timeouts

`Config.CallTimeout` defaults to 30 seconds for the whole RPC, including authentication, metadata, signing, retries, and the response.
An earlier deadline on the caller's context takes precedence.
`Config.HTTPAttemptTimeout` caps each HTTP attempt at 10 seconds by default, within the remaining total limit.
Replace the old `RequestTimeout` setting with `HTTPAttemptTimeout`.

## Uncertain orders and stream gaps

Use the [recovery helpers and gap ranges](https://developers.grvt.io/guides/recovery) to look up uncertain orders and detect lost stream continuity.
A missing order stays unknown. The helpers never resend an order.

## Pagination

Use `Pages` to follow cursors without fetching ahead. Pass an empty starting cursor for the first page.
Keep filters fixed in the callback.

```go
fetch := func(ctx context.Context, cursor string) (*models.ApiTradeHistoryResponse, error) {
    return client.TradeHistory(ctx, &models.ApiTradeHistoryRequest{
        Instrument: "BTC_USDT_Perp", Cursor: cursor,
    })
}
for page, err := range grvt.Pages(ctx, fetch, "") {
    if err != nil { return err }
    for _, trade := range page.Result { fmt.Println(trade) }
}
```

An empty page can still have a next cursor. Breaking the loop stops further calls; the context cancels a fetch.
A repeated cursor returns `PaginationCycleError`. Fetch errors pass through without extra retries.
Cursors do not guarantee a stable snapshot while records change.

## Default sub-account

Set `Config.SubAccountID` to fill omitted request and stream account IDs. Zero leaves the client default unset.
A zero account in a request or selector means omitted; an `OrderInput` uses `nil` for omitted.
Explicit nonzero IDs take precedence. Bulk orders first inherit the request account, then the client default.
Transfer source and destination IDs stay explicit. Defaults are applied to copies before signing.
Without a default, calls that need an account still require one.

## Recovery helpers

The SDK exposes lookup-only order resolution, history backfill with unproven continuity,
and explicit cancellation-countdown refresh.
Session rotation reports that authenticated WebSockets must reconnect.
See the [recovery recipes](https://developers.grvt.io/guides/recovery), including [rebuilding a local order book](https://developers.grvt.io/guides/recovery#rebuild-a-book-after-a-gap), before restoring trading after a gap.
