package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/logx"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// logSink is a concurrency-safe buffer for a handler that writes from the
// server's own goroutines.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func harnessWithLog(t *testing.T, level slog.Level, upstream http.HandlerFunc) (*harness, *logSink) {
	t.Helper()
	sink := &logSink{}
	h := newHarness(t, upstream, func(o *Options) {
		o.Log = slog.New(logx.NewHandler(sink, logx.Options{Level: level}))
	})
	return h, sink
}

const okChat = `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`

func okUpstream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(okChat))
}

// accessLog already writes one line per request at info. The console must not
// gain a second one, and the line must name the REQUESTED target.
func TestAccessLineNamesTheAliasNotJustTheHop(t *testing.T) {
	body := `{"model":"up/gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`
	h, sink := harnessWithLog(t, slog.LevelInfo, okUpstream)

	if code, _, resp := h.do("POST", "/v1/chat/completions", "tok-infer", body); code != 200 {
		t.Fatalf("dispatch: %d %s", code, resp)
	}
	out := sink.String()
	if n := strings.Count(out, "INFO  request "); n != 1 {
		t.Fatalf("want exactly one request line, got %d:\n%s", n, out)
	}
	for _, want := range []string{"status=200", "model=gpt-6-luna", "alias=up/gpt-6-luna"} {
		if !strings.Contains(out, want) {
			t.Fatalf("access line is missing %q:\n%s", want, out)
		}
	}
	// The happy path carries no trail: the field appears only when a candidate
	// did not serve.
	if strings.Contains(out, "trail=") {
		t.Fatalf("a clean request grew a trail field:\n%s", out)
	}
}

// A failover is the case an operator cannot reconstruct from status alone.
// The harness's "daily" combo is bad→good, so hop 1 answers 429 and hop 2
// serves: the access line has to carry that, or "200" hides a dead provider.
func TestAccessLineCarriesTheFailoverTrail(t *testing.T) {
	sink := &logSink{}
	tu := newTwoUpstream(t, true, func(o *Options) {
		o.Log = slog.New(logx.NewHandler(sink, logx.Options{Level: slog.LevelInfo}))
	})
	code, body := tu.chat(`{"model":"daily","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)
	if code != 200 {
		t.Fatalf("failover dispatch: %d %s", code, body)
	}
	out := sink.String()
	if !strings.Contains(out, "trail=") {
		t.Fatalf("the access line lost the failover trail:\n%s", out)
	}
	if !strings.Contains(out, "429") {
		t.Fatalf("the trail does not name the refusal:\n%s", out)
	}
	if !strings.Contains(out, "alias=daily") {
		t.Fatalf("the line does not name the requested combo:\n%s", out)
	}
}

// A refused credential reaches neither the ledger nor any other log, so this
// line is the only way to see it. It stays at debug so a retry loop cannot
// flood the console, and it must never print the token itself.
func TestAuthRefusalIsLoggedWithoutTheToken(t *testing.T) {
	h, sink := harnessWithLog(t, slog.LevelDebug, okUpstream)

	if code, _, _ := h.do("POST", "/v1/chat/completions", "", `{"model":"x","messages":[]}`); code != http.StatusUnauthorized {
		t.Fatalf("missing token: %d", code)
	}
	out := sink.String()
	if !strings.Contains(out, `why="missing token"`) {
		t.Fatalf("missing-token refusal not logged:\n%s", out)
	}

	sink2 := &logSink{}
	h2 := newHarness(t, okUpstream, func(o *Options) {
		o.Log = slog.New(logx.NewHandler(sink2, logx.Options{Level: slog.LevelDebug}))
	})
	if code, _, _ := h2.do("POST", "/v1/chat/completions", "definitely-not-a-token", `{"model":"x","messages":[]}`); code != http.StatusUnauthorized {
		t.Fatalf("invalid token: %d", code)
	}
	out2 := sink2.String()
	if !strings.Contains(out2, `why="invalid token"`) {
		t.Fatalf("invalid-token refusal not logged:\n%s", out2)
	}
	if strings.Contains(out2, "definitely-not-a-token") {
		t.Fatalf("the refused token was written to the log:\n%s", out2)
	}
}

// An admin route reached with an infer-only token: the 403 is correct, but a
// human hunting "why does the dashboard say forbidden" needs the path named.
func TestAdminRoleRefusalIsLogged(t *testing.T) {
	h, sink := harnessWithLog(t, slog.LevelDebug, okUpstream)
	if code, _, _ := h.do("GET", "/admin/health", "tok-infer", ""); code != http.StatusForbidden {
		t.Fatalf("infer token on an admin route: want 403, got %d", code)
	}
	out := sink.String()
	if !strings.Contains(out, "admin refused") || !strings.Contains(out, "path=/admin/health") {
		t.Fatalf("admin role refusal not logged:\n%s", out)
	}
	if strings.Contains(out, "tok-infer") {
		t.Fatalf("the token was written to the log:\n%s", out)
	}
}

// The budget refusal is Info, but only once per client per window: a client
// hammering past its cap must not turn a diagnosis into a flood.
func TestBudgetRefusalLogsOncePerWindow(t *testing.T) {
	h, sink := harnessWithLog(t, slog.LevelInfo, okUpstream)
	ctx := context.Background()
	id, err := h.db.AddClientToken(ctx, "capped", "capped-token", "infer")
	if err != nil {
		t.Fatal(err)
	}
	cap := int64(10)
	if err = h.db.UpdateToken(ctx, id, nil, nil, &cap, "daily", nil, nil); err != nil {
		t.Fatal(err)
	}
	h.db.RecordCall(store.Call{TS: time.Now().UTC(), Client: "capped", TokensOut: 20})
	if err = h.db.Flush(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if code, _, _ := h.do("POST", "/v1/chat/completions", "capped-token", `{"model":"up/gpt-6-luna","messages":[]}`); code != http.StatusTooManyRequests {
			t.Fatalf("attempt %d: want 429, got %d", i, code)
		}
	}
	if n := strings.Count(sink.String(), "client budget reached"); n != 1 {
		t.Fatalf("budget line logged %d times, want exactly 1:\n%s", n, sink.String())
	}
}
