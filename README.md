# Grvt Go SDK

The Go SDK for the Grvt exchange: REST calls, WebSocket streams, order entry, EIP-712 signing and login in one
module.

> **Preview.** Any 0.x release can change the API. Pin an exact version.

## Install

```sh
go get github.com/gravity-technologies/grvt-sdk-go
```

The SDK is in preview: any 0.x release can change the API, and your `go.mod` pins the version you get. Needs
Go 1.27.4 or newer.

## Quickstart

Create a client and read the BTC perpetual's mark price on testnet:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	grvt "github.com/gravity-technologies/grvt-sdk-go"
	"github.com/gravity-technologies/grvt-sdk-go/models"
)

func main() {
	ctx := context.Background()

	// The API key logs in; the signer signs orders with your signing key. Leave both out for public data only.
	signer, err := grvt.NewPrivateKeySigner(os.Getenv("GRVT_PRIVATE_KEY"))
	if err != nil {
		log.Fatal(err)
	}
	client, err := grvt.New(grvt.Config{Env: grvt.Testnet(), APIKey: os.Getenv("GRVT_API_KEY"), Signer: signer})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	t, err := client.Ticker(ctx, &models.ApiTickerRequest{Instrument: "BTC_USDT_Perp"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(t.Result.MarkPrice) // the exchange's fair price, used for margin
}
```

Place an order and handle the errors that matter for trading:

```go
resp, err := client.CreateOrder(ctx, &models.ApiCreateOrderRequestInput{Order: order})
var unknown *grvt.OrderStateUnknownError
var bounds *grvt.OrderOutOfBoundsError
var apiErr *grvt.APIError
switch {
case errors.As(err, &unknown):
	// The order may or may not exist. Look it up; never resend it.
	_, err = grvt.ResolveOrder(ctx, client, sub, unknown.ClientOrderID)
	return err
case errors.As(err, &bounds):
	return fmt.Errorf("not sent, breaks %s: %w", bounds.Limit, err)
case errors.As(err, &apiErr):
	return fmt.Errorf("rejected with code %d: %w", apiErr.Code, err)
case err != nil:
	return err
}
fmt.Println("placed", resp.Result.Metadata.ClientOrderID)
```

Building `order` is shown in [Place an order](https://developers.grvt.io/sdks/go#place-an-order). For a
runnable program that places, watches and cancels one testnet order, see the
[Quickstart guide](https://developers.grvt.io/guides/quickstart#go).

## Documentation

- [Go SDK page](https://developers.grvt.io/sdks/go): client setup, streams, orders, errors, production behaviour
- [Quickstart](https://developers.grvt.io/guides/quickstart)
- [Signing](https://developers.grvt.io/guides/signing)
- [Errors and retries](https://developers.grvt.io/guides/errors-and-retries)
- [Recovery recipes](https://developers.grvt.io/guides/recovery)
- [WebSocket](https://developers.grvt.io/guides/websocket)
- [API reference](https://developers.grvt.io/reference/index)

## Go notes

- One `*grvt.Client` is safe for concurrent use. Share it across goroutines; calls that need a login share one.
- Every call takes a `context.Context`, bounded by `Config.CallTimeout`. Cancelling a write after it may
  have left returns `*grvt.OrderStateUnknownError`; it never closes the shared WebSocket.
- `Client.Close()` ends every subscription and fails WebSocket calls still waiting for a reply (writes with
  `*grvt.OrderStateUnknownError`). Later calls return `grvt.ErrClosed`. An HTTP call already running is
  bounded by its own context, not by `Close`.
- `Config.OnEvent` runs on the socket's read goroutine. Return quickly; a blocked hook stalls every stream on
  that socket.
- Generated `client_order_id`s are unique within one process only; separate processes rarely collide but
  can. If several processes trade one sub-account, set your own IDs per order or through
  `Config.ClientOrderIDs`.

## Known limitations

See [Not supported yet](https://developers.grvt.io/sdks/go#not-supported-yet) on the Go SDK page.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and [RUNTIME_API.md](RUNTIME_API.md).

## License

MIT. See [LICENSE](LICENSE).
