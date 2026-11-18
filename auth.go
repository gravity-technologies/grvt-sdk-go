package grvt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gravity-technologies/grvt-sdk-go/internal/behavior"
)

// API-key session.
//
// Login: POST {edge}/auth/api_key/login {"api_key": ...}
//
//	-> Set-Cookie: gravity=...; Expires=...
//	-> X-Grvt-Account-Id header, which gateways check as a sticky-session token
//
// Both go on every authenticated REST call and on the WS handshake. A WS connection keeps the session
// it was opened with, so the runtime re-logs in before expiry and moves sockets over.

const (
	CookieName    = "gravity"
	AccountHeader = "X-Grvt-Account-Id"
	LoginPath     = "/auth/api_key/login"
	// defaultSessionTTL is used when the cookie carries no expiry.
	defaultSessionTTL = behavior.SessionDefaultLifetime
	// refreshAt: re-login at this share of the cookie lifetime.
	refreshAt = behavior.SessionRenewAt
)

// SessionState is the current login. Printing or logging it (%v, %+v, %#v, slog) shows the cookie as
// <redacted>; read the Cookie field directly when you need it.
type SessionState struct {
	Cookie    string
	AccountID string
	IssuedAt  time.Time
	ExpiresAt time.Time
	// Generation bumps on every successful login.
	Generation            uint64
	FundingAccountAddress string
}

const redacted = "<redacted>"

func (s SessionState) redactedCookie() string {
	if s.Cookie == "" {
		return ""
	}
	return redacted
}

// String keeps the cookie out of %v and %+v.
func (s SessionState) String() string {
	return fmt.Sprintf("{Cookie:%s AccountID:%s IssuedAt:%s ExpiresAt:%s Generation:%d FundingAccountAddress:%s}",
		s.redactedCookie(), s.AccountID, s.IssuedAt, s.ExpiresAt, s.Generation, s.FundingAccountAddress)
}

// GoString keeps the cookie out of %#v.
func (s SessionState) GoString() string { return "grvt.SessionState" + s.String() }

// LogValue keeps the cookie out of slog output.
func (s SessionState) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("cookie", s.redactedCookie()),
		slog.String("account_id", s.AccountID),
		slog.Time("issued_at", s.IssuedAt),
		slog.Time("expires_at", s.ExpiresAt),
		slog.Uint64("generation", s.Generation),
		slog.String("funding_account_address", s.FundingAccountAddress),
	)
}

// RefreshDue is when a re-login should happen.
func (s SessionState) RefreshDue() time.Time {
	return s.IssuedAt.Add(time.Duration(float64(s.ExpiresAt.Sub(s.IssuedAt)) * refreshAt))
}

type session struct {
	apiKey   string
	loginURL string
	http     *http.Client
	log      func(msg string, args ...any)

	mu             sync.Mutex
	state          SessionState
	loginMu        sync.Mutex
	loginTask      *sessionLogin
	owner          context.Context //nolint:containedctx // client lifetime, cancelled by Close, independent of any waiter
	attemptTimeout time.Duration
}

func (s *session) enabled() bool { return s.apiKey != "" }

func (s *session) snapshot() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// headers are the cookie and account headers for an authenticated call (empty before login).
func (s *session) headers() http.Header {
	st := s.snapshot()
	h := http.Header{}
	if st.Cookie != "" {
		h.Set("Cookie", CookieName+"="+st.Cookie)
		if st.AccountID != "" {
			h.Set(AccountHeader, st.AccountID)
		}
	}
	return h
}

func (s *session) fresh() bool {
	st := s.snapshot()
	return st.Cookie != "" && time.Now().Before(st.RefreshDue())
}

// ensure logs in if there is no fresh session. Concurrent callers share one login.
type sessionLogin struct {
	done chan struct{}
	err  error
}

//nolint:contextcheck // shared login belongs to the client lifetime, not the first waiter
func (s *session) ensure(ctx context.Context) error {
	if !s.enabled() || s.fresh() {
		return nil
	}
	s.loginMu.Lock()
	if s.fresh() {
		s.loginMu.Unlock()
		return nil
	}
	task := s.loginTask
	if task == nil {
		task = &sessionLogin{done: make(chan struct{})}
		s.loginTask = task
		owner := s.owner
		if owner == nil {
			owner = context.Background()
		}
		go func() {
			task.err = s.login(owner)
			s.loginMu.Lock()
			s.loginTask = nil
			close(task.done)
			s.loginMu.Unlock()
		}()
	}
	s.loginMu.Unlock()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-task.done:
		return task.err
	}
}

func (s *session) login(ctx context.Context) error {
	if s.apiKey == "" {
		return &ConfigError{Message: "this call needs a session: set Config.APIKey"}
	}
	timeout := s.attemptTimeout
	if timeout == 0 {
		timeout = behavior.HTTPAttemptTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := json.Marshal(map[string]string{"api_key": s.apiKey})
	if err != nil {
		return &ConfigError{Message: "cannot encode login request: " + err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.loginURL, bytes.NewReader(body))
	if err != nil {
		return &ConfigError{Message: "bad edge url: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return &TransportError{Message: "login: " + err.Error(), RPC: "auth/api_key/login", Err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &TransportError{Message: "login: reading response: " + err.Error(), RPC: "auth/api_key/login", Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		msg := "login failed"
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		// 401/403 stay wrong until the user fixes the key; reconnect loops stop on these.
		return &APIError{Code: 1000, Message: msg, Status: resp.StatusCode, RPC: "auth/api_key/login", Retryable: resp.StatusCode >= 500}
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == CookieName {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value == "" {
		return &APIError{Code: 1000, Message: "login response had no gravity cookie", Status: resp.StatusCode, RPC: "auth/api_key/login"}
	}
	var b struct {
		FundingAccountAddress string `json:"funding_account_address"`
	}
	if err = json.Unmarshal(data, &b); err != nil {
		s.log("login response body undecodable; funding account address unknown", "err", err)
	}
	now := time.Now()
	expires := now.Add(defaultSessionTTL)
	switch {
	case cookie.MaxAge > 0:
		expires = now.Add(time.Duration(cookie.MaxAge) * time.Second)
	case !cookie.Expires.IsZero() && cookie.Expires.After(now):
		expires = cookie.Expires
	}
	s.mu.Lock()
	s.state = SessionState{Cookie: cookie.Value, AccountID: resp.Header.Get(AccountHeader), IssuedAt: now,
		ExpiresAt: expires, Generation: s.state.Generation + 1, FundingAccountAddress: b.FundingAccountAddress}
	gen := s.state.Generation
	s.mu.Unlock()
	s.log("logged in", "generation", gen, "valid_for", expires.Sub(now).Round(time.Second))
	return nil
}
