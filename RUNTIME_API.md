# Go runtime API (generator target)

Generated files (`*_gen.go` in the module root and in `models/`) talk to the hand-written runtime only
through the names on this page. If the generator needs something not listed here, add it to the
runtime and to this page; never reach into unexported fields. (Generated root files share package
`grvt` with the runtime, so the compiler does not enforce this; the rule does.)

The generator is `sdk/generator/golang` (`make sdk-gen-go`, or `make sdk-gen-code` for every SDK and the docs). Its
output (not committed) is the reference for everything below. Naming and write rules shared with the other
generators live in `sdk/generator/naming`.

## 1. Models (package `models`, one struct per contract type)

```go
// OrderLeg One-line description.
type OrderLeg struct {
	// field description
	Instrument    string  `json:"instrument,omitempty"`
	SubAccountID  *uint64 `json:"sub_account_id,omitempty,string"`
	IsBuyingAsset *bool   `json:"is_buying_asset,omitempty"`
}
```

- Field names are the wire names in Go style (`goName`: `sub_account_id` -> `SubAccountID`, with the
  initialisms ID, API, URL, HTTP, JSON, IP, RPC, UUID). JSON tags are the full wire names. Two wire
  names that map to one Go name fail generation.
- Required fields use native values. Decoding rejects missing keys and accepts null only for nullable fields.
  Signed calls use `Input` models. Only declared partial streams use `Partial` payload models.
  The table below describes optional fields.

  | wire | Go | "absent" |
  |---|---|---|
  | string, asset, address, uint128, uint256 | `string` | `""` |
  | uint64 / int64 (JSON string) | `*uint64` / `*int64` with `,string` | `nil` |
  | uint8/16/32, int8/16/32, bool | `*uint8` ... `*bool` | `nil` |
  | enum | the enum type (a `string`) | `""` |
  | nested type | `*T` | `nil` |
  | list | `[]T` (values), `models.Uint64s` for lists of uint64 | `nil` |
  | any | `json.RawMessage` | `nil` |

- Enums (`models/enums_gen.go`): `type TimeInForce string`, constants `TimeInForceGoodTillTime`, a
  `TimeInForceValues` slice, `Code() (int64, bool)` (the signed integer; false for an unknown value)
  and `String()`. Unknown values decode as they are.
- `models.Uint64s` is the only hand-written name in package `models` (`models/wire.go`).
- Decoding is generated: each model gets `decode<T>`, which reads the text once with the small reader in
  `internal/wirejson` and checks required keys as it goes. Types with a required or timestamp field use
  it as their `UnmarshalJSON`; the runtime uses it for every reply and stream frame (`decodeJSON`). The
  rules are encoding/json's: keys match exactly, then without case; unknown keys are skipped (but must
  be valid JSON); a repeated key decodes again into the same field. `models/legacy_gen_test.go` holds
  encoding/json decoders as a reference, and a differential test checks both accept, reject and decode the
  same random documents for every model.
- Decode errors (`DecodeError.Err`, `UnmarshalJSON`) keep encoding/json's types: invalid JSON is a
  `*json.SyntaxError`, a value of the wrong kind or out of range a `*json.UnmarshalTypeError` (`Struct`
  is the outermost model, `Field` the path of JSON names, e.g. `bids.num_orders`). Bad timestamps give
  `*strconv.NumError`, bad decimals `ParseDecimal`'s error, bad quoted 64-bit ints encoding/json's error
  for their text. Where it differs from encoding/json: these errors and missing/null required keys carry
  the field (`OrderbookLevels.bids: OrderbookLevel.price is required`; `errors.As`/`errors.Is` still reach
  the original); the decoder stops at the first problem, so when a document has several the one reported
  can differ; a bad `,string` integer is a `*json.UnmarshalTypeError` (encoding/json gives a plain error).
  Accepted documents and decoded values are the same. A model with `UnmarshalJSON` changes its target
  only on success.
- Lite encoding (`Config.Encoding: EncodingLite`): every gateway type (edge types have none) also gets
  `fieldLite<T>` (lite key -> field index, exact match, no case folding), `unmarshalLite<T>` and
  `liteFrom<T>`/`toLite<T>`, registered with `wirejson.RegisterLite`. Decoding is the same `decode<T>`:
  it picks `fieldLite<T>` when the reader is `wirejson.NewLiteReader`, so a lite decode is the same one
  pass as a full one. Encoding renames the text `json.Marshal` wrote, one pass with values copied as
  they are; a key with no lite name in its type is an error (a lite gateway ignores unknown keys, so a
  missed rename must not reach it). Lite names repeat across types, so both directions rename per type.
  Generation fails if a gateway type (any RPC request or response, stream frame, or struct inside one)
  lacks a lite name or repeats one.

## 2. RPCs (one `RPCSpec` var + one method + one `RequestInfo` per request type)

```go
var RPCOrderCreateOrder = RPCSpec{Method: "v1/create_order", Gateway: GatewayTrading,
	Path: "/full/v1/create_order", Auth: true, Write: true, OrderEntry: true,
	RefreshMetadataOn: []int{2062, 2064, 2065, 2066, 2067}} // order calls only

func (c *Client) CreateOrder(ctx context.Context, req *models.ApiCreateOrderRequest) (*models.ApiCreateOrderResponse, error) {
	return Call[models.ApiCreateOrderRequest, models.ApiCreateOrderResponse](ctx, c.rt, &RPCOrderCreateOrder, req,
		reqApiCreateOrderRequest, SignOrderCreateOrder) // nil for unsigned RPCs
}

var reqApiCreateOrderRequest = &RequestInfo[models.ApiCreateOrderRequest]{
	Missing:       func(r *models.ApiCreateOrderRequest) string { /* first required field not set, or "" */ },
	ClientOrderID: func(r *models.ApiCreateOrderRequest, ids ClientOrderIDProvider) uint64 { /* order writes only */ },
	// bulk orders set ClientOrderIDs instead: func(r *models.ApiBulkOrdersRequest, ids ClientOrderIDProvider) []uint64
}
```

`RPCSpec{Method, Gateway, Path, LitePath, Auth, Write, OrderEntry}`: `Method` is the JSON-RPC method (edge
routes: the path without "/"), `LitePath` the path in the lite encoding (empty for edge routes), `Gateway` is `GatewayMarketData | GatewayTrading | GatewayEdge`, `Write`
means never retried and an ambiguous failure is `*OrderStateUnknownError`, `OrderEntry` means it goes
over the trading WS with `OrderTransportWS`. `RefreshMetadataOn` (order calls only) lists the error codes
after which the runtime drops the request's `legs[].instrument` from the metadata cache. The generated `RPCs` and `Streams` slices list every spec.

`Call[Req, Resp](ctx, rt, spec, req, info, sign)` does, in order: nil request -> empty request ->
`info.Missing` (a `*ConfigError`) -> copy the request (JSON round trip) when it is signed or gets a
client order id -> `info.ClientOrderID` (or `info.ClientOrderIDs`, one per order) -> `BeforeSend` hook (only if set) -> `sign` -> encode ->
`ensureSession` if `spec.Auth` -> REST or WS -> decode `Resp`. For REST the whole body is the
response; over WS the JSON-RPC `result` is the same struct. An unknown outcome reports the client order ids
filled before the hook. `BeforeSend` may change the request but must not add, remove or reorder orders, or change their client order IDs (prices, sizes and other fields may change): if
it does, the call returns a `*ConfigError` and sends nothing.

With `EncodingLite`, a call with a `LitePath` renames the encoded body with `wirejson.ToLite[Req]` after
everything above (metadata refresh reads the full body), posts to `LitePath`, and
decodes the reply with `wirejson.UnmarshalLite`. The HTTP error body is `transport.lite.http_error_body`
(`c`, `m`, `s`). Over WS the socket dials `Environment.*WSLite` when set, else the full URL with `/ws/full` swapped for
`/ws/lite` (a URL without that ending is a `*ConfigError` at `New`), and uses the lite envelope (generated
`lite*` constants in `specs_gen.go`: request `j m p i`, reply `i r e`, error `c m`, frame `s s1 sn ps`);
subscribe params are `WSSubscribeParams` renamed the same way. Edge login and `GET /time` stay full.

## 3. Streams (one `StreamSpec` var + one method)

```go
var StreamOrderbookOrderbookDelta = StreamSpec{Stream: "v1.book.d", Gateway: GatewayMarketData,
	Auth: false, SnapshotFeed: false, Conflate: false}  // both true for snapshot (*.s) channels

func (c *Client) OrderbookDeltaStream(ctx context.Context, sel *models.WSOrderbookLevelsFeedSelectorV1) (*Subscription[models.WSOrderbookLevelsFeedDataV1], error) {
	if sel == nil {
		sel = &models.WSOrderbookLevelsFeedSelectorV1{}
	}
	if sel.Instrument == "" {
		return nil, missingField(StreamOrderbookOrderbookDelta.Stream, "instrument")
	}
	return Subscribe[models.WSOrderbookLevelsFeedDataV1](ctx, c.rt, &StreamOrderbookOrderbookDelta,
		Selector([]string{sel.Instrument}, []string{SelectorInt(sel.Rate), SelectorInt(sel.Depth)}))
}
```

The selector argument is the contract's `selector_type` model. `Selector(primary, secondary)` joins
the non-empty values as `p1-p2@s1-s2`; `SelectorInt(*uintN)` formats an optional integer; enums are
passed as `string(sel.Field)`. `missingField(where, field)` is the `*ConfigError` for a required
primary field. `Subscribe[F]` logs in when `spec.Auth`, subscribes, waits for the ack and returns
`*Subscription[F]` (`All()`, `Next(ctx)`, `Close()`, `Dropped()`). Every stream method gets a `Stream`
suffix (`naming.StreamSuffix` in Go form), so it never shares a name with an RPC.

The read loop routes a frame in one allocation-free pass (`decodeEnvelope`: id, stream, selector and
sequence numbers; the rest is validated and skipped) and queues the raw frame; `Next` decodes it into
`F` with the generated decoder (`decodeEnvelopeLite` and the lite decoder of `F` for a lite client).

## 4. Signed types (one `*StructType` var + message builders + one `Sign<Id>` function per signed call)

```go
var eip712Order = mustStructType("Order", []TypeDef{
	{Name: "Order", Fields: []FieldDef{{"subAccountID", "uint64"}, /* ... */ {"legs", "OrderLeg[]"}}},
	{Name: "OrderLeg", Fields: []FieldDef{{"assetID", "uint256"}, /* ... */}},
}, "0xb4b857ba...", hashEIP712Order)  // panics at package load if the type hash does not match

var typeHashOrder = [32]byte{0xb4, 0xb8, /* ... computed by the generator */}

// One generated encoder per struct: a fixed [32*(fields+1)]byte buffer filled by the put* helpers in
// eip712_encode.go (native writes, same range checks and errors as before), hashed once; arrays
// hash each item, then stream the item hashes through one keccak. Hashing allocates nothing.
func hashEIP712Order(v Value) ([32]byte, error) { /* structFields, putUint, putBool, putArray(..., hashEIP712OrderLeg), putInt, keccakInto */ }

func messageOrder(b *MessageBuilder, d *models.ApiCreateOrderRequest, meta *Meta) Value {
	return Struct{
		b.Copy(Deref(d.Order).SubAccountID, "uint64", "order.sub_account_id", nil),
		Each(Deref(d.Order).Legs, func(e *models.OrderLeg) Value { return messageOrderOrderLeg(b, e, meta) }),
		// ...
	}
}

func SignOrderCreateOrder(ctx context.Context, rt *Runtime, req *models.ApiCreateOrderRequest) error {
	// create the signature object if absent; rt.PrepareSignature(&sig.Signer, &sig.Nonce, &sig.Expiration,
	// &sig.ChainID, SignatureTTL); collect instrument/currency names; loadMetadata (generated, cached via
	// rt.Metadata()); CheckOrder(&orderChecks, req.Order, meta) for calls that carry an Order; the variant rule picks the struct type; b.Err; rt.Sign(ctx, sig.ChainID, st, msg);
	// write Signer, R, S, V back.
}
```

Messages are positional (`Struct{...}` in field order). `Deref(p)` reads a nested field through a nil
pointer as an empty struct. Transform errors go to `MessageBuilder.Err` (a `*SigningError`) and the
sign function returns it before anything is signed.

| transform | code |
|---|---|
| `copy`, `sig.nonce`, `sig.expiration` | `b.Copy(path, "<eip type>", "<where>", def)` (`def`: `nil`, `false`, `"0"`, `int64(n)`) |
| `address` | `b.Address(path, where)` (absent = zero address) |
| `enumCode` | `b.EnumCode(path, where)` (absent = 0; an unknown value is an error) |
| `scaled` fixed | `b.Scaled(path, 9, where, def)` (rejects over-precision, never rounds; `def` "" = none) |
| `scaled` instrument_base_decimals | `b.Scaled(path, b.InstrumentBaseDecimals(meta, instPath), ...)` |
| `scaled` instrument_quote_decimals | `b.Scaled(path, b.InstrumentQuoteDecimals(meta, instPath), ...)` |
| `scaled` currency_balance_decimals | `b.Scaled(path, b.CurrencyBalanceDecimals(meta, curPath), ...)` |
| `assetKey` | `b.AssetKey(meta, path)` |
| `currencyId` | `b.CurrencyID(meta, path)` |
| `mainAccount` | `b.MainAccount(meta)` (`loadMetadata(..., true)` loads it once per client with `loadMainAccount`) |
| `each` | `Each(list, func(e *models.T) Value { return message<Type><Nested>(b, e, meta) })` |

Variant rules the generator knows: `builder` (`AddressSet(path)`), `wallet_types` and
`currency_width` (`meta.Currency(symbol)` -> `CurrencyMeta.ID`), and a single type. An unknown kind
fails generation. Runtime names used: `mustStructType`, `structFields`, `fieldErr`,
`putUint`, `putInt`, `putBool`, `putAddress`, `putString`, `putBytes`, `putFixedBytes`, `putStruct`, `putArray`,
`item*`, `keccakInto`, `TypeDef`, `FieldDef`, `Value`, `Struct`,
`MessageBuilder`, `Each`, `Deref`, `AddressSet`, `Meta` (`Instruments`, `Currencies`, `MainAccount`, `Currency`),
`InstrumentMeta`, `CurrencyMeta`, `Runtime.Metadata()` (`Instrument`, `Currency`, `PutCurrency`, `MainAccount`),
`Runtime.PrepareSignature`, `Runtime.Sign`, `SigningError`, `StructType`, `CheckOrder`, `OrderChecks`,
`OrderCheckRule`, `BaseDecimals`. The generated `orderChecks` var is the contract's `order_checks`.
`CheckOrder(&checks, order, meta)` takes a `models.OrderInput` value (dereference an `*OrderInput` first) and
reads its legs through generated field projections, without encoding the order to JSON. It returns `*OrderOutOfBoundsError` for the first leg outside its instrument's limits (read
from `InstrumentMeta.Limits`, which `loadInstrument` fills) and `*SigningError` for a malformed value. It
never changes the order. If several fields in the same leg are malformed, it reports the first field
in alphabetical order; valid values then follow contract rule order.

The SDK signs only these generated types: `StructType` has no public constructor and there is no
generic hashing. The `Signer` interface gets `(ctx, domain, struct type, message)`; a wallet/KMS signer
can call `StructType.TypedData` for eth_signTypedData_v4 JSON (`Name()`, `Types()`, `TypeHash()`,
`EncodeType()`, `HashStruct`, `Digest` are also available). `Runtime.Sign` signs with a
`*PrivateKeySigner` directly; for any other signer it computes the generated digest, and rejects the
returned signature with a `*SigningError` unless it is low-s (s at most half the curve order), recovers to
`Signer.Address()`, and has a signer field that is empty or equal to that address. Some KMS backends (AWS
KMS, for one) return a high s: normalize it (s = n - s, flip v) before returning.

## 5. Errors

```go
const CodeSignatureExpired = 2001
var errorTable = ErrorTable{{Code: 1000, Name: "Unauthorized", Status: 401, Message: "...", Retryable: false}, /* sorted by code */}
```

`New` passes `errorTable` to `NewRuntime`; the runtime uses it for the retry rule (`rate_limit_code`, a
code the table marks retryable, or a status in `read_retry_statuses`; both keys live in the generated behavior module).

## 6. Client shell

```go
type Client struct {
	rt *Runtime
}
func New(cfg Config) (*Client, error)  // NewRuntime(cfg, errorTable)
func (c *Client) Runtime() *Runtime
func (c *Client) Close() error
// plus one Client method per RPC and stream: client.CreateOrder, client.TradeStream, ...
```

An RPC or stream whose Go name is `Runtime` or `Close` fails generation. `Config` fields: `Env`, `APIKey`, `Signer`,
`OrderTransport`, `Encoding`, `CheckOrders`, `Clock`, `Nonces`, `ClientOrderIDs`, `HTTPClient`, `BeforeSend`, `OnEvent`,
`CallTimeout`, `HTTPAttemptTimeout`, `WSRequestTimeout`, `ResyncTimeout`, `IdleTimeout`, `QueueSize`, `Logger`.

## 7. Generated test

`api_gen_test.go` calls every RPC with its required fields set against the fake gateway in
`fakegateway_test.go` (`newReachabilityServer(t, enc)`, `env()`, `lastWirePath()`), in both encodings
(`encodings`), expects signed calls to stop at signing (no signer configured), and checks every stream
rejects a missing required selector field. In lite the fake gateway (`lite_test.go`, written from
`contract.json`) fails the test on any request key that is not a lite name of its type.

## Call deadlines

`Config.CallTimeout` bounds the whole RPC, including hooks, signing metadata, login,
retry delays and waiting for a reply. An earlier deadline or cancellation on the
caller's `context.Context` wins. `HTTPAttemptTimeout` limits each HTTP attempt,
including login, and `WSRequestTimeout` caps a reply or subscribe. Defaults come from
`internal/behavior` (built from `contract/behavior.json`); zero config values select them, and negative values are invalid.

Cancellation before dispatch sends no write. Cancellation after a write may have
left the process returns `OrderStateUnknownError` with the client order
IDs and the original cause (`errors.Is` still finds context cancellation).
Metadata and login loads belong to the client: cancelling one waiter does not
cancel another caller's load. Closing the client cancels these shared loads.
Custom synchronous hooks and signers must return or observe their context; the SDK
cannot interrupt blocking code. It checks the context again before dispatch, so
late preparation cannot send a write.

A custom `HTTPClient.Transport` must honor the request context. Once a write is
handed to an opaque transport, a failure with no trustworthy reply is uncertain.
The standard Go HTTP transport supplies tracing that also identifies failures
which occurred before dispatch.

WebSocket calls wait for the shared writer using the caller's context. Cancellation while queued
returns `TransportError` with `Sent: false` and leaves the connection open. Once handed to the writer,
the caller can return an uncertain outcome while the runtime completes the active frame. The writer
checks the caller's deadline again before starting I/O, so canceled queued work cannot send later.
A started frame uses the runtime context to avoid closing a connection shared by other calls.

## HTTP redirects and retries

The SDK never follows a redirect or retries a write, and a custom HTTP transport must not either: the rule and
why are in [What the SDK retries](https://developers.grvt.io/guides/errors-and-retries#what-the-sdk-retries).
