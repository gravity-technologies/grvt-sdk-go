package grvt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"

	"github.com/gravity-technologies/grvt-sdk-go/internal/behavior"
)

// REST calls: POST {gateway}{path} with a JSON body.
//
// Success: HTTP 200, the body is the RPC's response struct.
// Failure: body {"code": int, "message": str, "status": int}.
// Reads retry with full jitter on retryable failures; writes are sent exactly once.

// ReadAttempts is how many times a read is tried in all.
const ReadAttempts = behavior.ReadAttempts

// newReadBackoff is the backoff between read retries.
func newReadBackoff() *Backoff {
	return newBackoff(behavior.ReadBackoffBase, behavior.ReadBackoffCap)
}

func (rt *Runtime) restCall(ctx context.Context, spec *RPCSpec, body []byte, clientOrderIDs []uint64) ([]byte, error) {
	backoff := newReadBackoff()
	for attempt := 1; ; attempt++ {
		data, err := rt.restOnce(ctx, spec, body)
		if err == nil {
			return data, nil
		}
		if spec.Write {
			var tr *TransportError
			if errors.As(err, &tr) && tr.Sent {
				return nil, unknownWrite(spec, err, clientOrderIDs)
			}
			var api *APIError
			if errors.As(err, &api) {
				if rt.errors.uncertain(api) {
					return nil, unknownWrite(spec, err, clientOrderIDs)
				}
			}
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		if !behavior.RetryRead(isRetryable(err), attempt) {
			return nil, err
		}
		delay := backoff.Next()
		rt.hooks.emit(func() Event {
			return Event{Kind: EventRetry, RPC: spec.Method, Attempt: attempt, Delay: delay, Err: err.Error()}
		})
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, context.Cause(ctx)
		case <-t.C:
		}
	}
}

func (rt *Runtime) restOnce(ctx context.Context, spec *RPCSpec, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, rt.httpAttemptTimeout)
	defer cancel()
	var sent atomic.Bool
	transport := rt.http.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	// Only the standard transport's tracing proves a connect failure was unsent.
	// An opaque RoundTripper may dispatch without emitting trace callbacks.
	if _, standard := transport.(*http.Transport); !standard {
		sent.Store(true)
	}
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteHeaders: func() { sent.Store(true) }})
	path := spec.Path
	lite := rt.wireLite(spec)
	if lite {
		path = spec.LitePath
	}
	url := rt.env.restURL(spec.Gateway, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, &ConfigError{Message: "bad url " + url + ": " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	if spec.Auth {
		for k, v := range rt.session.headers() {
			req.Header[k] = v
		}
	}
	if rt.log.Enabled(ctx, slog.LevelDebug) {
		h := map[string]string{}
		for k := range req.Header {
			h[k] = redactHeader(k, req.Header.Get(k))
		}
		rt.log.Debug("POST", "url", url, "headers", h, "body", Redact(body, 2000))
	}
	if deadlineErr := deadlineError(ctx); deadlineErr != nil {
		return nil, &TransportError{Message: deadlineErr.Error(), RPC: spec.Method, Sent: false, Err: deadlineErr}
	}
	resp, err := rt.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			err = errors.Join(err, context.Cause(ctx))
		}
		return nil, &TransportError{Message: err.Error(), RPC: spec.Method, Sent: sent.Load(), Err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &TransportError{Message: "reading response: " + err.Error(), RPC: spec.Method, Sent: true, Err: err}
	}
	if rt.log.Enabled(ctx, slog.LevelDebug) {
		rt.log.Debug("reply", "rpc", spec.Method, "status", resp.StatusCode, "body", Redact(data, 2000))
	}
	if resp.StatusCode == http.StatusOK {
		return data, nil
	}
	if lite {
		return nil, rt.errors.responseErrorKeys(data, resp.StatusCode, spec.Method, liteHTTPErrorCode, liteHTTPErrorMessage)
	}
	return nil, rt.errors.responseError(data, resp.StatusCode, spec.Method)
}
