package grvt

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gravity-technologies/grvt-sdk-go/internal/behavior"
)

// The runtime's shared defaults and rules equal sdk/contract/behavior.json (internal/behavior). A failure
// here means the runtime holds its own copy of a shared constant: use the generated one instead.

func TestSharedClientDefaults(t *testing.T) {
	rt, err := NewRuntime(Config{Env: Testnet()}, errorTable)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	checks := []struct {
		name      string
		got, want any
	}{
		{"CallTimeout", rt.callTimeout, behavior.CallTimeout},
		{"HTTPAttemptTimeout", rt.httpAttemptTimeout, behavior.HTTPAttemptTimeout},
		{"session attempt timeout", rt.session.attemptTimeout, behavior.HTTPAttemptTimeout},
		{"WSRequestTimeout", rt.wsRequestTimeout, behavior.WSRequestTimeout},
		{"QueueSize", rt.queueSize, behavior.QueueSize},
		{"session retry delay", rt.sessionRetryDelay, behavior.SessionRetryDelay},
		{"read backoff base", newReadBackoff().Base, behavior.ReadBackoffBase},
		{"read backoff cap", newReadBackoff().Cap, behavior.ReadBackoffCap},
		{"reconnect backoff base", newReconnectBackoff().Base, behavior.ReconnectBackoffBase},
		{"reconnect backoff cap", newReconnectBackoff().Cap, behavior.ReconnectBackoffCap},
		{"md resync timeout", time.Duration(rt.md.resyncTimeout.Load()), behavior.ResyncTimeout},
		{"td resync timeout", time.Duration(rt.td.resyncTimeout.Load()), behavior.ResyncTimeout},
		{"md idle timeout", rt.md.idleTimeout, behavior.WSIdleTimeout},
		{"td idle timeout", rt.td.idleTimeout, behavior.WSIdleTimeout},
		{"ReadAttempts", ReadAttempts, behavior.ReadAttempts},
		{"refreshAt", refreshAt, behavior.SessionRenewAt},
		{"defaultSessionTTL", defaultSessionTTL, behavior.SessionDefaultLifetime},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestClientsShareDefaultOrderIDs(t *testing.T) {
	provider := func(ids ClientOrderIDProvider) ClientOrderIDProvider {
		t.Helper()
		c, err := New(Config{Env: Testnet(), ClientOrderIDs: ids})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c.Runtime().clientOrderIDs
	}
	if provider(nil) != provider(nil) {
		t.Fatal("default clients have separate order ID providers")
	}
	custom := &TimeCounterClientOrderID{}
	if provider(custom) != custom {
		t.Fatal("client ignored its order ID provider")
	}
}

func TestSharedClientOrderIDLayout(t *testing.T) {
	var ids TimeCounterClientOrderID
	before := uint64(time.Now().Unix())
	a, b := ids.NextClientOrderID(), ids.NextClientOrderID()
	after := uint64(time.Now().Unix())
	for _, n := range []uint64{a, b} {
		seconds := n >> behavior.ClientOrderIDCounterBits & behavior.ClientOrderIDSecondsMask
		if n>>63 != 1 || seconds < before || seconds > after {
			t.Fatalf("id %d: top bit %d, seconds %d not in [%d, %d]", n, n>>63, seconds, before, after)
		}
	}
	if b&behavior.ClientOrderIDCounterMask != (a+1)&behavior.ClientOrderIDCounterMask {
		t.Fatalf("counter did not step: %d then %d", a, b)
	}
}

func TestSharedErrorRules(t *testing.T) {
	if RateLimitCode != behavior.RateLimitCode {
		t.Fatalf("RateLimitCode = %d", RateLimitCode)
	}
	for _, s := range behavior.ReadRetryStatuses() {
		if !errorTable.Retryable(99999, s) {
			t.Errorf("status %d is not retryable", s)
		}
	}
	if errorTable.Retryable(99999, 500) {
		t.Error("status 500 is retryable")
	}
	const known = 1003 // a contract code with HTTP status 400
	low, high := behavior.WriteRejectedStatusMin, behavior.WriteRejectedStatusMax
	for status, want := range map[int]bool{low: false, high: false, low - 1: true, high + 1: true} {
		if got := errorTable.uncertain(&APIError{Code: known, Status: status}); got != want {
			t.Errorf("status %d: uncertain = %t, want %t", status, got, want)
		}
	}
}

// -- options and errors: the "options" and "errors" tables of behavior.json ----------------------------------

type sharedTables struct {
	Options []struct {
		Name string
		Min  *int
	}
	Errors []struct {
		Kind, Parent string
		Fields       []string
		Names        map[string]string
		Renamed      map[string]map[string]string
	}
	Events []struct {
		Kind    string
		Fields  []string
		Renamed map[string]map[string]string
	}
}

func loadSharedTables(t *testing.T) sharedTables {
	t.Helper()
	data, err := ReadShared("contract/behavior.json")
	if err != nil {
		t.Fatal(err)
	}
	var s sharedTables
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

var goInitialisms = map[string]string{"rpc": "RPC", "id": "ID", "ids": "IDs", "http": "HTTP", "ws": "WS"}

// goName is a snake_case name in Go style: client_order_ids -> ClientOrderIDs.
func goName(snake string) string {
	var b strings.Builder
	for _, p := range strings.Split(snake, "_") {
		if up, ok := goInitialisms[p]; ok {
			b.WriteString(up)
		} else {
			b.WriteString(strings.ToUpper(p[:1]) + p[1:])
		}
	}
	return b.String()
}

// Each shared option is a Config field. Zero means the default (behavior.json, the option's go extra); a
// value below the minimum, or a negative duration, is a ConfigError.
func TestSharedOptions(t *testing.T) {
	for _, o := range loadSharedTables(t).Options {
		t.Run(o.Name, func(t *testing.T) {
			field, ok := reflect.TypeFor[Config]().FieldByName(goName(o.Name))
			if !ok {
				t.Fatalf("Config has no %s", goName(o.Name))
			}
			below, lowest := int64(-1), int64(1)
			if o.Min != nil {
				below, lowest = int64(*o.Min)-1, int64(*o.Min)
				if below == 0 {
					below = -1 // 0 means the default in Go, so test the next value down
				}
			}
			build := func(v int64) error {
				cfg := Config{Env: Testnet()}
				reflect.ValueOf(&cfg).Elem().FieldByIndex(field.Index).SetInt(v)
				rt, err := NewRuntime(cfg, errorTable)
				if err == nil {
					rt.Close()
				}
				return err
			}
			var cfgErr *ConfigError
			if err := build(below); !errors.As(err, &cfgErr) {
				t.Errorf("%s = %d: err = %v, want a *ConfigError", field.Name, below, err)
			}
			if err := build(lowest); err != nil {
				t.Errorf("%s = %d: %v", field.Name, lowest, err)
			}
		})
	}
}

// One value of each kind, to check its fields and parent.
var errorSamples = map[string]error{
	"APIError":               &APIError{},
	"RateLimitError":         &RateLimitError{},
	"TransportError":         &TransportError{},
	"OrderStateUnknownError": &OrderStateUnknownError{},
	"OrderOutOfBoundsError":  &OrderOutOfBoundsError{},
	"StreamOverflowError":    &StreamOverflowError{},
	"FrameTooLargeError":     &FrameTooLargeError{},
	"DecodeError":            &DecodeError{},
	"SigningError":           &SigningError{},
	"ConfigError":            &ConfigError{},
	"ErrClosed":              ErrClosed,
	"PaginationCycleError":   &PaginationCycleError{},
	"BookError":              &BookError{},
}

// exportedErrors lists the package's exported error types (with an Error method) and Err variables.
func exportedErrors(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var names []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Name.Name != "Error" || d.Recv == nil || len(d.Recv.List) != 1 {
					continue
				}
				typ := d.Recv.List[0].Type
				if star, ok := typ.(*ast.StarExpr); ok {
					typ = star.X
				}
				if id, ok := typ.(*ast.Ident); ok && id.IsExported() {
					names = append(names, id.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if v, ok := spec.(*ast.ValueSpec); ok {
						for _, n := range v.Names {
							if strings.HasPrefix(n.Name, "Err") && n.IsExported() {
								names = append(names, n.Name)
							}
						}
					}
				}
			}
		}
	}
	slices.Sort(names)
	return names
}

func TestSharedErrors(t *testing.T) {
	tables := loadSharedTables(t)
	var want []string
	byKind := map[string]string{}
	for _, e := range tables.Errors {
		want = append(want, e.Names["go"])
		byKind[e.Kind] = e.Names["go"]
	}
	slices.Sort(want)
	if got := exportedErrors(t); !slices.Equal(got, want) {
		t.Fatalf("exported errors %v, behavior.json lists %v", got, want)
	}
	for _, e := range tables.Errors {
		name := e.Names["go"]
		sample, ok := errorSamples[name]
		if !ok {
			t.Errorf("no sample of %s", name)
			continue
		}
		// Go has no inheritance: a parent is what errors.As also finds.
		if e.Parent != "" {
			parent := reflect.New(reflect.TypeOf(errorSamples[byKind[e.Parent]]))
			if !errors.As(sample, parent.Interface()) {
				t.Errorf("errors.As does not find %s as %s", name, byKind[e.Parent])
			}
		}
		typ := reflect.TypeOf(sample)
		if typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		for _, f := range e.Fields {
			if renamed, ok := e.Renamed["go"][f]; ok {
				f = renamed
			} else {
				f = goName(f)
			}
			if typ.Kind() != reflect.Struct {
				t.Errorf("%s has no field %s", name, f)
			} else if _, ok := typ.FieldByName(f); !ok {
				t.Errorf("%s has no field %s", name, f)
			}
		}
	}
}

// Event is one flat struct: its fields are Kind plus every field of every event in behavior.json, in Go style
// (the go extras in the table); each EventKind constant is one of the table's kinds.
func TestSharedEvents(t *testing.T) {
	tables := loadSharedTables(t)
	kinds := []string{}
	want := []string{"Kind"}
	for _, e := range tables.Events {
		kinds = append(kinds, e.Kind)
		for _, f := range e.Fields {
			name, ok := e.Renamed["go"][f]
			if !ok {
				name = goName(f)
			}
			if !slices.Contains(want, name) {
				want = append(want, name)
			}
		}
	}
	var got []string
	for f := range reflect.TypeFor[Event]().Fields() {
		got = append(got, f.Name)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("Event has fields %v, behavior.json's events have %v", got, want)
	}
	constants := []EventKind{EventConnect, EventReconnect, EventDisconnect, EventSubscribe, EventGap, EventRetry,
		EventFrame, EventSessionRotated, EventError}
	var names []string
	for _, k := range constants {
		names = append(names, string(k))
	}
	slices.Sort(names)
	slices.Sort(kinds)
	if !slices.Equal(names, kinds) {
		t.Errorf("EventKind constants %v, behavior.json's events are %v", names, kinds)
	}
}
