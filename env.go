package grvt

import (
	"fmt"
	"strings"
)

// Gateway is where an RPC or stream is served.
type Gateway int

const (
	GatewayMarketData Gateway = iota
	GatewayTrading
	GatewayEdge
)

func (g Gateway) String() string {
	switch g {
	case GatewayMarketData:
		return "md"
	case GatewayTrading:
		return "td"
	default:
		return "edge"
	}
}

// RPCSpec describes one RPC or edge route (generated in specs_gen.go).
type RPCSpec struct {
	// Method is the JSON-RPC method, e.g. "v1/create_order" (for edge routes, the path without "/").
	Method  string
	Gateway Gateway
	// Path is the REST path, e.g. "/full/v1/create_order".
	Path string
	// LitePath is the REST path in the lite encoding, e.g. "/lite/v1/create_order"; empty for edge
	// routes, which have no lite form.
	LitePath string
	// Auth: needs the session cookie.
	Auth bool
	// Write: never retried; an ambiguous failure is *OrderStateUnknownError.
	Write bool
	// OrderEntry: goes over the trading WebSocket when the client uses OrderTransportWS.
	OrderEntry bool
	// RefreshMetadataOn: error codes that mean the cached instrument limits may be stale. The runtime
	// drops the request's leg instruments from the metadata cache; the order itself is never resent.
	RefreshMetadataOn []int
}

// StreamSpec describes one stream (generated in specs_gen.go).
type StreamSpec struct {
	// Stream is the channel, e.g. "v1.book.d".
	Stream  string
	Gateway Gateway
	Auth    bool
	// SnapshotFeed: every frame is complete (e.g. v1.ticker.s), so a gap needs no re-sync.
	SnapshotFeed bool
	// Conflate: old items may be dropped for a slow consumer: snapshot streams only. Every other
	// stream (deltas, events, private) gives the consumer a *StreamOverflowError and re-syncs.
	Conflate bool
}

// Environment says where the SDK connects and which chain id it signs for. Use Testnet() or Production(),
// or build one (a local gateway, a test server).
type Environment struct {
	Name string
	// MarketDataURL, TradingURL and EdgeURL are the REST bases (edge serves login).
	MarketDataURL string
	TradingURL    string
	EdgeURL       string
	MarketDataWS  string
	TradingWS     string
	// MarketDataWSLite and TradingWSLite are the WebSocket URLs for the lite encoding (Config.Encoding).
	// Empty, each is the full URL with its ending /ws/full swapped for /ws/lite; set one only when that
	// rule does not fit.
	MarketDataWSLite string
	TradingWSLite    string
	// ChainID is the EIP-712 domain chainId.
	ChainID uint64
	// WSMaxFrameBytes is the largest WS frame the gateway accepts; 0 means do not check. The deployed
	// value is not published yet, so the named environments leave it 0.
	WSMaxFrameBytes int
}

// Validate reports a missing URL or chain id. Every field of a custom Environment is used as given: nothing
// falls back to a named environment, so a zero ChainID or an empty URL is an error, not a default.
func (e *Environment) Validate() error {
	var missing []string
	for _, f := range []struct{ name, value string }{
		{"MarketDataURL", e.MarketDataURL}, {"TradingURL", e.TradingURL}, {"EdgeURL", e.EdgeURL},
		{"MarketDataWS", e.MarketDataWS}, {"TradingWS", e.TradingWS},
	} {
		if strings.TrimSpace(f.value) == "" {
			missing = append(missing, f.name)
		}
	}
	if e.ChainID == 0 {
		missing = append(missing, "ChainID")
	}
	if len(missing) > 0 {
		return &ConfigError{Message: fmt.Sprintf("environment %q is missing %s: a custom Environment must set every URL and the chain id",
			e.Name, strings.Join(missing, ", "))}
	}
	return nil
}

// fullWSPath is the full WebSocket path that a lite URL is derived from.
const fullWSPath = "/ws/full"

// wsURL is the gateway's WebSocket URL. For lite it is the explicit lite URL when set, else the full URL
// with its ending /ws/full swapped for /ws/lite; a full URL without that ending is a *ConfigError.
func (e *Environment) wsURL(g Gateway, lite bool) (string, error) {
	url, explicit, field := e.MarketDataWS, e.MarketDataWSLite, "MarketDataWSLite"
	if g != GatewayMarketData {
		url, explicit, field = e.TradingWS, e.TradingWSLite, "TradingWSLite"
	}
	if !lite {
		return url, nil
	}
	if strings.TrimSpace(explicit) != "" {
		return explicit, nil
	}
	base, ok := strings.CutSuffix(url, fullWSPath)
	if !ok {
		return "", &ConfigError{Message: fmt.Sprintf("environment %q: %s does not end in %s, so it has no lite URL; set %s",
			e.Name, url, fullWSPath, field)}
	}
	return base + LiteWSPath, nil
}

func (e *Environment) restURL(g Gateway, path string) string {
	switch g {
	case GatewayMarketData:
		return e.MarketDataURL + path
	case GatewayTrading:
		return e.TradingURL + path
	}
	return e.EdgeURL + path
}
