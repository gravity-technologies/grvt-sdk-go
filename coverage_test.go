package grvt_test

// Every gateway RPC and stream in contract.json has its own method directly on the client (not grouped
// by namespace), with the contract's request/selector type and response/feed type. Checked by reflection, independently of the generator.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	grvt "github.com/gravity-technologies/grvt-sdk-go"
)

var initialisms = map[string]string{"id": "ID", "api": "API", "url": "URL", "http": "HTTP", "json": "JSON",
	"ip": "IP", "rpc": "RPC", "uuid": "UUID", "eth": "Eth"}

func goName(s string) string {
	var b strings.Builder
	for _, p := range strings.Split(s, "_") {
		if p == "" {
			continue
		}
		if v, ok := initialisms[p]; ok {
			b.WriteString(v)
		} else {
			b.WriteString(strings.ToUpper(p[:1]) + p[1:])
		}
	}
	return b.String()
}

func TestEveryContractOperationHasAMethod(t *testing.T) {
	data, err := grvt.ReadShared("contract/contract.json")
	if err != nil {
		t.Fatal(err)
	}
	type op struct {
		Signs       json.RawMessage `json:"signs"`
		PayloadMode string          `json:"payload_mode"`
		Namespace   string          `json:"namespace"`
		MethodName  string          `json:"method_name"`
		Request     string          `json:"request"`
		Response    string          `json:"response"`
		Selector    string          `json:"selector_type"`
		Feed        string          `json:"feed_type"`
	}
	type field struct {
		Type         string `json:"type"`
		ServerFilled bool   `json:"server_filled"`
	}
	var c struct {
		Types []struct {
			Name   string  `json:"name"`
			Fields []field `json:"fields"`
		} `json:"types"`
		RPCs    []op `json:"rpcs"`
		Streams []op `json:"streams"`
		Edge    struct {
			Routes []op `json:"routes"`
		} `json:"edge"`
	}
	if err = json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	client, err := grvt.New(grvt.Config{Env: grvt.Testnet()})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	cv := reflect.ValueOf(client)
	if cv.MethodByName("LoginAPIKey").IsValid() {
		t.Fatal("login belongs to the session runtime")
	}
	taken := map[string]string{}
	check := func(o op, method, in, out string) {
		id := o.Namespace + "." + o.MethodName
		if prev, dup := taken[method]; dup {
			t.Errorf("%s and %s are both Client.%s", prev, id, method)
			return
		}
		taken[method] = id
		m := cv.MethodByName(method)
		if !m.IsValid() {
			t.Errorf("%s: no method Client.%s", id, method)
			return
		}
		mt := m.Type()
		gotIn, gotOut := mt.In(1).Elem().Name(), mt.Out(0).Elem().Name()
		if gotIn != in || !strings.Contains(mt.Out(0).String(), out) {
			t.Errorf("%s: %s(%s) %s, want %s -> %s", id, method, gotIn, gotOut, in, out)
		}
	}
	fields := map[string][]field{}
	for _, typ := range c.Types {
		fields[typ.Name] = typ.Fields
	}
	var hasServerFilled func(string, map[string]bool) bool
	hasServerFilled = func(name string, seen map[string]bool) bool {
		if seen[name] {
			return false
		}
		seen[name] = true
		for _, f := range fields[name] {
			if f.ServerFilled || hasServerFilled(f.Type, seen) {
				return true
			}
		}
		return false
	}
	n := 0
	for _, o := range c.RPCs {
		request := o.Request
		if len(o.Signs) > 0 || hasServerFilled(o.Request, map[string]bool{}) {
			request += "Input"
		}
		check(o, goName(o.MethodName), request, o.Response)
		n++
	}
	for _, o := range c.Streams {
		feed := o.Feed
		if o.PayloadMode == "partial" {
			feed = "Partial" + feed
		}
		check(o, goName(o.MethodName)+"Stream", o.Selector, feed) // every stream method ends in Stream
		n++
	}
	if len(c.RPCs)+len(c.Edge.Routes) != len(grvt.RPCs) || len(c.Streams) != len(grvt.Streams) {
		t.Fatalf("%d operations; specs have %d RPCs and %d streams", n, len(grvt.RPCs), len(grvt.Streams))
	}
}
