package proxy

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/logx"
)

func parkedDispatcher(t *testing.T, level slog.Level) (*Dispatcher, *syncBuf) {
	t.Helper()
	d := newDispatcher()
	sink := &syncBuf{}
	d.SetLogger(slog.New(logx.NewHandler(sink, logx.Options{Level: level})))
	now := time.Now()
	for _, m := range []string{"m1", "m2"} {
		rt := textRoute("http://up.invalid", m)
		d.recordHop(rt, hopOutcome{failed: true, cooldown: time.Minute, strikes: 1, reason: "rate limited"}, now)
	}
	other := textRoute("http://other.invalid", "m1")
	other.Account.ID, other.Account.Namespace = 2, "other"
	d.recordHop(other, hopOutcome{failed: true, cooldown: time.Minute, strikes: 1, reason: "rate limited"}, now)
	if len(d.Cooldowns()) != 3 {
		t.Fatalf("setup: %+v", d.Cooldowns())
	}
	return d, sink
}

// Unpark clears exactly the scope asked for: one model, one namespace, or all.
func TestClearCooldownsScopes(t *testing.T) {
	d, _ := parkedDispatcher(t, slog.LevelInfo)
	if n := d.ClearCooldowns("test", "m1"); n != 1 || len(d.Cooldowns()) != 2 {
		t.Fatalf("one model: cleared %d, left %+v", n, d.Cooldowns())
	}
	if n := d.ClearCooldowns("test", ""); n != 1 || len(d.Cooldowns()) != 1 || d.Cooldowns()[0].Namespace != "other" {
		t.Fatalf("one namespace: cleared %d, left %+v", n, d.Cooldowns())
	}
	if n := d.ClearCooldowns("", ""); n != 1 || len(d.Cooldowns()) != 0 {
		t.Fatalf("all: cleared %d, left %+v", n, d.Cooldowns())
	}
}

// The clear line is Info: it appears at Info and is filtered at Warn.
func TestClearCooldownsLoggedAtInfo(t *testing.T) {
	d, sink := parkedDispatcher(t, slog.LevelInfo)
	d.ClearCooldowns("", "")
	if !strings.Contains(sink.String(), "cooldowns cleared") {
		t.Fatalf("log = %q, want the clear line at Info", sink.String())
	}
	d, sink = parkedDispatcher(t, slog.LevelWarn)
	d.ClearCooldowns("", "")
	if strings.Contains(sink.String(), "cooldowns cleared") {
		t.Fatalf("clear line leaked at Warn: %q", sink.String())
	}
}
