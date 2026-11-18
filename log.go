package grvt

import (
	"context"
	"log/slog"
	"os"
	"regexp"
	"strings"
)

// Logging goes through log/slog. Config.Logger wins; otherwise GRVT_LOG=debug (or info, warn, error)
// logs to stderr, and without it nothing is logged.
//
// Wire logs are always redacted: the gravity cookie, api keys, private keys, signature r/s and any
// 32-byte hex value.

func loggerFromEnv() *slog.Logger {
	var level slog.Level
	switch strings.ToLower(os.Getenv("GRVT_LOG")) {
	case "":
		return slog.New(discard{})
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})).With("lib", "grvt")
}

type discard struct{}

func (discard) Enabled(context.Context, slog.Level) bool  { return false }
func (discard) Handle(context.Context, slog.Record) error { return nil }
func (d discard) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discard) WithGroup(string) slog.Handler           { return d }

var (
	secretJSON = regexp.MustCompile(`(?i)("(?:api_key|private_key|r|s|gravity|cookie)"\s*:\s*)"[^"]*"`)
	cookieVal  = regexp.MustCompile(`gravity=[^;"\s]*`)
	hex32      = regexp.MustCompile(`\b0x[0-9a-fA-F]{64}\b`)
)

// Redact hides secrets in a JSON body, cookie string or log line. It truncates to limit bytes first.
func Redact(data []byte, limit int) string {
	if len(data) > limit {
		data = data[:limit]
	}
	s := secretJSON.ReplaceAllString(string(data), `$1"***"`)
	s = cookieVal.ReplaceAllString(s, "gravity=***")
	return hex32.ReplaceAllString(s, "0x***")
}

// redactHeader hides header values that are secrets.
func redactHeader(name, value string) string {
	switch strings.ToLower(name) {
	case "cookie", "set-cookie", "authorization":
		return "***"
	}
	return value
}
