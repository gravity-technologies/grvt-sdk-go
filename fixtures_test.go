package grvt

import (
	"encoding/json"
	"strings"
)

// Test transport defaults use complete wire models. Explicit malformed responses
// bypass these helpers so decoder tests continue to exercise missing fields.
type wireTestContract struct {
	Types []fixtureType `json:"types"`
	Enums []struct {
		Name   string
		Values []struct{ Name string }
	} `json:"enums"`
	RPCs []struct {
		HTTPPath string `json:"http_path"`
		WSMethod string `json:"ws_method"`
		Response string
	} `json:"rpcs"`
	Streams []struct {
		Channel string
		Feed    string `json:"feed_type"`
	} `json:"streams"`
}

var fixtureContract = func() wireTestContract {
	var c wireTestContract
	b, err := ReadShared("contract/contract.json")
	if err != nil {
		panic(err)
	}
	if err = json.Unmarshal(b, &c); err != nil {
		panic(err)
	}
	return c
}()

type fixtureType struct {
	Name   string
	Fields []struct {
		Name, Type, Kind string
		Optional         bool
		ArrayDepth       int `json:"array_depth"`
	}
}

func wireFixture(name string, overrides map[string]any) map[string]any {
	out := map[string]any{}
	for _, t := range fixtureContract.Types {
		if t.Name != name {
			continue
		}
		for _, f := range t.Fields {
			if f.Optional {
				continue
			}
			var v any = "1"
			switch {
			case f.ArrayDepth > 0:
				v = []any{}
			case f.Kind == "struct":
				v = wireFixture(f.Type, nil)
			case f.Kind == "enum":
				for _, e := range fixtureContract.Enums {
					if e.Name == f.Type {
						v = e.Values[0].Name
						break
					}
				}
			case f.Type == "bool":
				v = false
			case f.Type == "any":
				v = map[string]any{}
			case f.Type == "uint8" || f.Type == "uint16" || f.Type == "uint32" || f.Type == "int8" || f.Type == "int16" || f.Type == "int32":
				v = 1
			}
			out[f.Name] = v
		}
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}
func responseFixture(route string) map[string]any {
	for _, r := range fixtureContract.RPCs {
		if strings.HasSuffix(route, r.HTTPPath) || route == r.WSMethod {
			return wireFixture(r.Response, nil)
		}
	}
	return map[string]any{}
}
func fixtureJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
func feedFixture(channel string) any {
	for _, s := range fixtureContract.Streams {
		if s.Channel == channel {
			for _, t := range fixtureContract.Types {
				if t.Name == s.Feed {
					for _, f := range t.Fields {
						if f.Name == "feed" {
							if f.ArrayDepth > 0 {
								return []any{}
							}
							return wireFixture(f.Type, map[string]any{"instrument": "X"})
						}
					}
				}
			}
		}
	}
	return map[string]any{}
}
func instrumentFixture() string {
	return fixtureJSON(map[string]any{"result": wireFixture("InstrumentDisplay", map[string]any{"instrument": "BTC_USDT_Perp", "instrument_hash": "0x030501", "base_decimals": 9, "quote_decimals": 6, "tick_size": "0.1", "min_size": "0", "min_notional": "0"})})
}
