package grvt

import (
	"errors"
	"strings"
	"testing"
)

// A custom Environment is used exactly as given: every URL and the chain id are set independently, and a
// missing one is a ConfigError at New, never a silent fallback to a named environment.
func TestCustomEnvironment(t *testing.T) {
	custom := Environment{
		Name:          "custom",
		MarketDataURL: "https://md.gateway.example.com",
		TradingURL:    "https://td.gateway.example.com",
		EdgeURL:       "https://edge.gateway.example.com",
		MarketDataWS:  "wss://md.gateway.example.com/ws/full",
		TradingWS:     "wss://td.gateway.example.com/ws/full",
		ChainID:       4242,
	}
	var cfgErr *ConfigError
	c, err := New(Config{Env: custom, Encoding: EncodingFull})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := c.Runtime().Env(); got != custom {
		t.Fatalf("env %+v, want %+v", got, custom)
	}
	if got := c.Runtime().Domain().ChainID; got != 4242 {
		t.Fatalf("signing chain id %d, want 4242", got)
	}

	for field, unset := range map[string]func(*Environment){
		"MarketDataURL": func(e *Environment) { e.MarketDataURL = "" },
		"TradingURL":    func(e *Environment) { e.TradingURL = "" },
		"EdgeURL":       func(e *Environment) { e.EdgeURL = "" },
		"MarketDataWS":  func(e *Environment) { e.MarketDataWS = "" },
		"TradingWS":     func(e *Environment) { e.TradingWS = "" },
		"ChainID":       func(e *Environment) { e.ChainID = 0 },
	} {
		env := custom
		unset(&env)
		_, err := New(Config{Env: env, Encoding: EncodingFull})
		if !errors.As(err, &cfgErr) || !strings.Contains(err.Error(), field) {
			t.Errorf("%s unset: want a ConfigError naming it, got %v", field, err)
		}
	}

	for _, name := range []string{"testnet", "production"} {
		if _, err := EnvironmentNamed(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, name := range []string{"staging", "prod"} {
		if _, err := EnvironmentNamed(name); err == nil {
			t.Errorf("unexpected named environment %q", name)
		}
	}
}

// The lite WebSocket URLs come from the full ones unless set; one that can't be derived is a ConfigError at New.
func TestCustomEnvironmentLiteWS(t *testing.T) {
	custom := Environment{
		Name:          "custom",
		MarketDataURL: "https://md.gateway.example.com",
		TradingURL:    "https://td.gateway.example.com",
		EdgeURL:       "https://edge.gateway.example.com",
		MarketDataWS:  "wss://md.gateway.example.com/ws/full",
		TradingWS:     "wss://td.gateway.example.com/ws/full",
		ChainID:       4242,
	}
	urls := func(env Environment) (string, string) {
		t.Helper()
		c, err := New(Config{Env: env}) // lite is the default
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return c.Runtime().Socket(GatewayMarketData).url, c.Runtime().Socket(GatewayTrading).url
	}
	if md, td := urls(custom); md != "wss://md.gateway.example.com/ws/lite" || td != "wss://td.gateway.example.com/ws/lite" {
		t.Errorf("derived lite URLs %q %q", md, td)
	}
	explicit := custom
	explicit.TradingWSLite = "wss://other/lite"
	if md, td := urls(explicit); md != "wss://md.gateway.example.com/ws/lite" || td != "wss://other/lite" {
		t.Errorf("an explicit lite URL should win: %q %q", md, td)
	}

	odd := custom
	odd.MarketDataWS = "wss://md.gateway.example.com/stream"
	var cfgErr *ConfigError
	if _, err := New(Config{Env: odd}); !errors.As(err, &cfgErr) || !strings.Contains(err.Error(), "does not end in /ws/full") ||
		!strings.Contains(err.Error(), "set MarketDataWSLite") {
		t.Fatalf("underivable lite URL: want a ConfigError naming the field, got %v", err)
	}
	c, err := New(Config{Env: odd, Encoding: EncodingFull})
	if err != nil {
		t.Fatalf("a full client needs no lite URL: %v", err)
	}
	c.Close()
	odd.MarketDataWSLite = "wss://md/lite"
	if md, _ := urls(odd); md != "wss://md/lite" {
		t.Errorf("explicit lite URL %q", md)
	}
}
