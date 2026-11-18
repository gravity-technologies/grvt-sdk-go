package grvt

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/gravity-technologies/grvt-sdk-go/internal/behavior"
	"github.com/gravity-technologies/grvt-sdk-go/internal/wirejson"
)

// Errors returned by the SDK. Use errors.As to tell them apart:
//
//   - *APIError: the server answered with an error code.
//   - *RateLimitError: the server said "slow down" (code 1006). Unwraps to an *APIError too.
//   - *TransportError: no usable answer (connect failure, timeout on a read, socket closed).
//   - *OrderStateUnknownError: a write may or may not have reached the exchange. Never resend blindly:
//     look the order up by ClientOrderID first. It wraps its cause (an *APIError, a *TransportError with
//     Sent true, or a context error), so errors.As finds those too: check for it before the others.
//   - *StreamOverflowError: a consumer fell behind a stream that may not drop items.
//   - *OrderOutOfBoundsError: an order leg breaks its instrument's tick, size step, minimum size or
//     minimum notional. Returned before signing; nothing was sent.
//   - *SigningError, *ConfigError, *DecodeError, *FrameTooLargeError, ErrClosed.

// RateLimitCode is the rate-limit error code. Always retryable for reads.
const RateLimitCode = behavior.RateLimitCode

// ErrClosed is returned after Client.Close.
var ErrClosed = errors.New("grvt: client closed")

// APIError is an error code from the server.
type APIError struct {
	Code    int
	Message string
	// Status is the HTTP status (0 over WebSocket).
	Status int
	// RPC is the method that failed, e.g. "v1/create_order".
	RPC string
	// Retryable follows the contract rule: rate limit, a retryable code, or HTTP 502/503/504.
	Retryable bool
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("%s: [%d] %s", orQ(e.RPC), e.Code, e.Message)
	if e.Status != 0 {
		s += fmt.Sprintf(" (http %d)", e.Status)
	}
	return s
}

// RateLimitError is code 1006. errors.As also finds it as an *APIError.
type RateLimitError struct {
	Message string
	Status  int
	RPC     string
}

func (e *RateLimitError) Error() string {
	s := fmt.Sprintf("%s: rate limited: %s", orQ(e.RPC), e.Message)
	if e.Status != 0 {
		s += fmt.Sprintf(" (http %d)", e.Status)
	}
	return s
}

// Unwrap returns the same error as an *APIError.
func (e *RateLimitError) Unwrap() error {
	return &APIError{Code: RateLimitCode, Message: e.Message, Status: e.Status, RPC: e.RPC, Retryable: true}
}

// TransportError means no usable answer came back. Sent says whether the request may have left this
// process (a read can be retried either way; a write that was sent becomes OrderStateUnknownError).
type TransportError struct {
	Message string
	RPC     string
	Sent    bool
	Err     error
}

func (e *TransportError) Error() string {
	if e.RPC != "" {
		return e.RPC + ": " + e.Message
	}
	return e.Message
}

func (e *TransportError) Unwrap() error { return e.Err }

// OrderStateUnknownError: a write was in flight when the connection dropped or the call timed out.
// The order may or may not exist; look it up (e.g. by ClientOrderID). The SDK never resends a write.
type OrderStateUnknownError struct {
	RPC    string
	Reason string
	// ClientOrderID is the order's client_order_id: 0 when the write has none or carries several.
	ClientOrderID uint64
	// ClientOrderIDs lists every client_order_id the write carried (each order of a bulk call).
	ClientOrderIDs []uint64
	// Err is the original transport, API or decode error.
	Err error
}

// newOrderStateUnknown builds the error for a write that carried the given client order ids.
func newOrderStateUnknown(rpc, reason string, ids []uint64) *OrderStateUnknownError {
	e := &OrderStateUnknownError{RPC: rpc, Reason: reason, ClientOrderIDs: ids}
	if len(ids) == 1 {
		e.ClientOrderID = ids[0]
	}
	return e
}

func (e *OrderStateUnknownError) Unwrap() error { return e.Err }

func (e *OrderStateUnknownError) Error() string {
	s := fmt.Sprintf("%s: outcome unknown (%s)", e.RPC, e.Reason)
	switch {
	case len(e.ClientOrderIDs) > 1:
		s += fmt.Sprintf(", client_order_ids=%v", e.ClientOrderIDs)
	case e.ClientOrderID != 0:
		s += fmt.Sprintf(", client_order_id=%d", e.ClientOrderID)
	}
	return s
}

// StreamOverflowError: the consumer fell behind a stream that may not drop items. The queue was
// cleared and the subscription re-synced; items in between are lost.
type StreamOverflowError struct {
	Stream   string
	Selector string
	Capacity int
}

func (e *StreamOverflowError) Error() string {
	return fmt.Sprintf("%s %s: consumer fell behind %d items; re-syncing", e.Stream, e.Selector, e.Capacity)
}

// OrderOutOfBoundsError: an order leg breaks one of its instrument's limits. Returned before signing;
// nothing was sent. The SDK never rounds an order: pick one of Nearest and send again. Values are decimal
// strings without trailing zeros.
type OrderOutOfBoundsError struct {
	Instrument string
	// Leg is the index of the failing leg.
	Leg int
	// Field is the leg field checked, or "limit_price * size" for the notional.
	Field string
	// Check is "at_least" or "multiple_of".
	Check string
	// Limit is the instrument field it is checked against: tick_size, min_size or min_notional.
	Limit      string
	Value      string
	LimitValue string
	// Nearest are the nearest valid values of Field (of size, for the notional).
	Nearest []string
	// ServerCode is the gateway error the order would get, e.g. 2064 (invalid limit price tick).
	ServerCode int
	Message    string
}

func (e *OrderOutOfBoundsError) Error() string { return e.Message }

// SigningError: a request could not be signed (missing field, over-precise decimal, unknown
// metadata). Always returned before anything is signed or sent.
type SigningError struct{ Message string }

func (e *SigningError) Error() string { return "signing: " + e.Message }

// ConfigError: the client is missing something the call needs (signer, api key, a required field) or
// was given a bad option.
type ConfigError struct{ Message string }

func (e *ConfigError) Error() string { return e.Message }

// DecodeError: the response did not match the expected type.
type DecodeError struct {
	RPC string
	Err error
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("%s: cannot decode response: %v", e.RPC, e.Err)
}
func (e *DecodeError) Unwrap() error { return e.Err }

// FrameTooLargeError: the encoded WS frame is larger than the server accepts. Nothing was sent.
type FrameTooLargeError struct{ Size, Limit int }

func (e *FrameTooLargeError) Error() string {
	return fmt.Sprintf("frame is %d bytes, server limit %d; use OrderTransportREST for this call", e.Size, e.Limit)
}

func orQ(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// ErrorInfo is one row of the contract's error table.
type ErrorInfo struct {
	Code      int
	Name      string
	Status    int
	Message   string
	Retryable bool
}

// ErrorTable is the contract's error table, sorted by code (generated: errorTable).
type ErrorTable []ErrorInfo

// Get returns the row for code.
func (t ErrorTable) Get(code int) (ErrorInfo, bool) {
	i := sort.Search(len(t), func(i int) bool { return t[i].Code >= code })
	if i < len(t) && t[i].Code == code {
		return t[i], true
	}
	return ErrorInfo{}, false
}

// Retryable is the contract rule: the rate-limit code, a code the table marks retryable, or a read retry
// status.
func (t ErrorTable) Retryable(code, status int) bool {
	info, ok := t.Get(code)
	return behavior.ReadRetryable(code, status, ok && info.Retryable)
}

func (t ErrorTable) apiError(code int, message string, status int, rpc string) error {
	if code == RateLimitCode {
		return &RateLimitError{Message: message, Status: status, RPC: rpc}
	}
	return &APIError{Code: code, Message: message, Status: status, RPC: rpc, Retryable: t.Retryable(code, status)}
}

// isRetryable reports whether a read may be retried after err.
func isRetryable(err error) bool {
	var rl *RateLimitError
	var tr *TransportError
	if errors.As(err, &rl) || errors.As(err, &tr) {
		return true
	}
	var api *APIError
	return errors.As(err, &api) && api.Retryable
}

func missingField(where, field string) error {
	return &ConfigError{Message: fmt.Sprintf("%s: %s is required", where, field)}
}

// unknownWrite retains the cause so callers can inspect HTTP status, code or cancellation.
func unknownWrite(spec *RPCSpec, cause error, ids []uint64) *OrderStateUnknownError {
	e := newOrderStateUnknown(spec.Method, cause.Error(), ids)
	e.Err = cause
	return e
}

// responseError is the single parser for REST and WebSocket error envelopes.
func (t ErrorTable) responseError(data []byte, status int, rpc string) error {
	var body struct {
		Code    *int    `json:"code"`
		Message *string `json:"message"`
	}
	if json.Unmarshal(data, &body) != nil || body.Code == nil || body.Message == nil {
		return t.apiError(0, "malformed error reply", status, rpc)
	}
	return t.apiError(*body.Code, *body.Message, status, rpc)
}

// responseErrorKeys is responseError for a body whose code and message have other names (the lite
// encoding). Keys match exactly; anything else in the body is skipped.
func (t ErrorTable) responseErrorKeys(data []byte, status int, rpc, codeKey, messageKey string) error {
	code, message, ok := errorBody(data, codeKey, messageKey)
	if !ok {
		return t.apiError(0, "malformed error reply", status, rpc)
	}
	return t.apiError(code, message, status, rpc)
}

func errorBody(data []byte, codeKey, messageKey string) (code int, message string, ok bool) {
	r := wirejson.NewReader(data)
	if r.Next() != '{' || r.OpenObject() != nil {
		return 0, "", false
	}
	var hasCode, hasMessage bool
	for first := true; ; first = false {
		key, more, err := r.Member(first)
		if err != nil {
			return 0, "", false
		}
		if !more {
			break
		}
		switch string(key) {
		case codeKey:
			hasCode, err = r.Next() != 'n', wirejson.DecodeInt(&r, &code)
		case messageKey:
			hasMessage, err = r.Next() != 'n', wirejson.DecodeString(&r, &message)
		default:
			err = r.Skip()
		}
		if err != nil {
			return 0, "", false
		}
	}
	return code, message, r.End() == nil && hasCode && hasMessage
}

// uncertain reports whether a write that failed with e may have reached the exchange.
func (t ErrorTable) uncertain(e *APIError) bool {
	info, _ := t.Get(e.Code) // a code the table does not have gets status 0: none
	return behavior.WriteOutcomeUnknown(e.Status, info.Status)
}
