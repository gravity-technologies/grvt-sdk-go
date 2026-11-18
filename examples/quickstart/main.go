// Read testnet market data; --trade opts into one bounded order lifecycle.
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	grvt "github.com/gravity-technologies/grvt-sdk-go"
	"github.com/gravity-technologies/grvt-sdk-go/models"
	"math/big"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() { os.Exit(run()) }
func run() int {
	trade := flag.Bool("trade", false, "place and cancel one testnet order")
	fixture := flag.String("fixture-url", "", "loopback test gateway URL")
	instrument := flag.String("instrument", "BTC_USDT_Perp", "instrument")
	capText := flag.String("max-notional", "200", "maximum quote amount")
	flag.Parse()
	fail := func(err error) int { fmt.Fprintln(os.Stderr, err); return 1 }
	capValue, err := models.ParseDecimal(*capText)
	if err != nil || capValue.Rat().Sign() <= 0 {
		return fail(errors.New("max-notional must be positive"))
	}
	config := grvt.Config{Env: grvt.Testnet(), HTTPAttemptTimeout: 5 * time.Second}
	if *fixture != "" {
		u, parseErr := url.Parse(*fixture)
		if parseErr != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return fail(errors.New("fixture-url must be a loopback HTTP base URL"))
		}
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return fail(errors.New("fixture-url must use a loopback IP"))
		}
		config.Env.MarketDataURL, config.Env.TradingURL, config.Env.EdgeURL = *fixture, *fixture, *fixture
		// The local fixture gateway speaks full names only.
		config.Encoding = grvt.EncodingFull
	}
	// Check credentials before the first network request.
	if *trade {
		config.APIKey = strings.TrimSpace(os.Getenv("GRVT_API_KEY"))
		config.SubAccountID, err = strconv.ParseUint(os.Getenv("GRVT_SUB_ACCOUNT_ID"), 10, 64)
		if config.APIKey == "" || err != nil || config.SubAccountID == 0 || os.Getenv("GRVT_PRIVATE_KEY") == "" {
			return fail(errors.New("--trade requires GRVT_API_KEY, GRVT_PRIVATE_KEY and a positive GRVT_SUB_ACCOUNT_ID"))
		}
		config.Signer, err = grvt.NewPrivateKeySigner(os.Getenv("GRVT_PRIVATE_KEY"))
		if err != nil {
			return fail(err)
		}
	}
	client, err := grvt.New(config)
	if err != nil {
		return fail(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ticker, err := client.Ticker(ctx, &models.ApiTickerRequest{Instrument: *instrument})
	if err != nil {
		return fail(err)
	}
	info, err := client.GetInstrument(ctx, &models.ApiGetInstrumentRequest{Instrument: *instrument})
	if err != nil {
		return fail(err)
	}
	fmt.Println("mark price", ticker.Result.MarkPrice)
	if !*trade {
		return 0
	}
	if ticker.Result.BestBidPrice == nil {
		return fail(errors.New("ticker has no best bid"))
	}
	price, size, err := orderAmounts(*ticker.Result.BestBidPrice, info.Result.TickSize, info.Result.MinSize, info.Result.MinNotional, capValue)
	if err != nil {
		return fail(err)
	}
	var rawID [8]byte
	if _, err = rand.Read(rawID[:]); err != nil {
		return fail(err)
	}
	id := binary.BigEndian.Uint64(rawID[:]) | 1<<63
	// Keep this ID when submission is uncertain. Never resend the order.
	fmt.Println("client_order_id", id)
	_, err = client.CreateOrder(ctx, &models.ApiCreateOrderRequestInput{Order: models.OrderInput{
		SubAccountID: grvt.Ptr(config.SubAccountID), TimeInForce: models.TimeInForceGoodTillTime, PostOnly: grvt.Ptr(true),
		Metadata: &models.OrderMetadataInput{ClientOrderID: &id},
		Legs:     []models.OrderLeg{{Instrument: *instrument, Size: size, LimitPrice: &price, IsBuyingAsset: true}},
	}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "submission failed or uncertain; looking up client_order_id", id, err)
	}
	cancelled := false
	for attempt := 0; attempt < 8; attempt++ {
		result, lookupErr := grvt.ResolveOrder(ctx, client, config.SubAccountID, id)
		if lookupErr != nil {
			fmt.Fprintln(os.Stderr, "lookup failed; preserving client_order_id", id, lookupErr)
		}
		if lookupErr == nil && result.Status == grvt.OrderFound && result.Order.State != nil {
			status := result.Order.State.Status
			fmt.Println("order status", status)
			switch string(status) {
			case "CANCELLED", "FILLED":
				return 0
			case "REJECTED":
				return 3
			case "OPEN":
				if !cancelled {
					cancelled = true
					if _, cancelErr := client.CancelOrder(ctx, &models.ApiCancelOrderRequest{SubAccountID: config.SubAccountID, ClientOrderID: &id}); cancelErr != nil {
						fmt.Fprintln(os.Stderr, "cancel outcome uncertain; continue lookup", id, cancelErr)
					}
				}
			}
		}
		if attempt < 7 {
			select {
			case <-time.After(500 * time.Millisecond):
			case <-ctx.Done():
				fmt.Fprintln(os.Stderr, "still unknown; reconcile client_order_id", id)
				return 2
			}
		}
	}
	fmt.Fprintln(os.Stderr, "still unknown; reconcile client_order_id", id, "before further trading")
	return 2
}

// Exact decimals keep the order on tick and size boundaries.
func orderAmounts(bid, tick, minimum, notional, capValue models.Decimal) (models.Decimal, models.Decimal, error) {
	zero := models.Decimal{}
	if bid.Rat().Sign() <= 0 || tick.Rat().Sign() <= 0 || minimum.Rat().Sign() <= 0 || notional.Rat().Sign() < 0 {
		return zero, zero, errors.New("invalid market limits")
	}
	ticks := new(big.Rat).Quo(bid.Rat(), tick.Rat())
	whole := new(big.Int).Quo(ticks.Num(), ticks.Denom())
	whole.Sub(whole, big.NewInt(1))
	price := new(big.Rat).Mul(new(big.Rat).SetInt(whole), tick.Rat())
	if price.Sign() <= 0 {
		return zero, zero, errors.New("no positive passive price")
	}
	lots := new(big.Rat).Quo(notional.Rat(), new(big.Rat).Mul(price, minimum.Rat()))
	count, remainder := new(big.Int), new(big.Int)
	count.QuoRem(lots.Num(), lots.Denom(), remainder)
	if remainder.Sign() > 0 {
		count.Add(count, big.NewInt(1))
	}
	if count.Sign() == 0 {
		count.SetInt64(1)
	}
	size := new(big.Rat).Mul(new(big.Rat).SetInt(count), minimum.Rat())
	if new(big.Rat).Mul(price, size).Cmp(capValue.Rat()) > 0 {
		return zero, zero, errors.New("minimum order exceeds max-notional")
	}
	p := models.MustParseDecimal(price.FloatString(decimalPlaces(tick.String())))
	s := models.MustParseDecimal(size.FloatString(decimalPlaces(minimum.String())))
	return p, s, nil
}
func decimalPlaces(s string) int {
	for i, c := range s {
		if c == '.' {
			return len(s) - i - 1
		}
	}
	return 0
}
