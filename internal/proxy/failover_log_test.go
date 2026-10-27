package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/logx"
	"github.com/c4lyp5o/ezllm/internal/provider"
)

// prepareFails wraps an adapter so PrepareRequest fails before any dial with
// whatever err returns, the way the Copilot guard refuses a wire.
type prepareFails struct {
	provider.Adapter
	err func() error
}

func (a prepareFails) PrepareRequest(context.Context, *http.Request, provider.Account, string, provider.Surface) error {
	return a.err()
}

func dispatcherFailingPrepare(t *testing.T, err func() error, level slog.Level) (*Dispatcher, *syncBuf) {
	t.Helper()
	client := &http.Client{Timeout: 30 * time.Second}
	reg := provider.NewRegistry(client)
	base, gerr := reg.Get(provider.KindOpenAICompatible)
	if gerr != nil {
		t.Fatal(gerr)
	}
	reg.Register(prepareFails{Adapter: base, err: err})
	d := NewDispatcher(client, reg)
	d.SetRetries(0)
	sink := &syncBuf{}
	d.SetLogger(slog.New(logx.NewHandler(sink, logx.Options{Level: level})))
	return d, sink
}

func wireRefusal() error {
	return fmt.Errorf("%w: test hop has no /responses endpoint", provider.ErrWireUnsupported)
}

// A refusal made before any dial says nothing about the hop's health: it must
// not clear a park a real 429 set. Before the fix it read as a success and
// deleted the cooldown, so a rate-limited hop went straight back to rotation.
func TestPreDialWireRefusalKeepsThePark(t *testing.T) {
	d := newDispatcher()
	rt := textRoute("http://up.invalid", "claude-sonnet-5")
	now := time.Now()
	d.recordHop(rt, hopOutcome{failed: true, cooldown: time.Minute, strikes: 1, reason: "rate limited"}, now)
	if len(d.Cooldowns()) != 1 {
		t.Fatalf("setup: want the hop parked, got %+v", d.Cooldowns())
	}

	oc := d.classifyHopPolicy(context.Background(), wireRefusal(), 0, nil, true)
	d.recordHop(rt, oc, now)
	if len(d.Cooldowns()) != 1 {
		t.Fatalf("Cooldowns() = %+v, want the park kept — a pre-dial refusal is no health signal", d.Cooldowns())
	}
}

// The wire-refusal line is Info: it appears at Info and is filtered at Warn.
func TestWireRefusalIsLoggedAtInfo(t *testing.T) {
	for _, c := range []struct {
		level slog.Level
		want  bool
	}{{slog.LevelInfo, true}, {slog.LevelWarn, false}} {
		d, sink := dispatcherFailingPrepare(t, wireRefusal, c.level)
		req, body := reqWithBody(`{"model":"claude-sonnet-5","messages":[]}`)
		_, _, _ = d.ForwardCandidates(context.Background(), httptest.NewRecorder(), req,
			[]Route{textRoute("http://up.invalid", "claude-sonnet-5")}, body)
		if got := strings.Contains(sink.String(), "hop cannot serve this wire, failing over"); got != c.want {
			t.Errorf("level %v: line present = %v, want %v\n%s", c.level, got, c.want, sink.String())
		}
	}
}

// The client-cancel line is Info: it appears at Info and is filtered at Warn.
func TestClientCancelIsLoggedAtInfo(t *testing.T) {
	for _, c := range []struct {
		level slog.Level
		want  bool
	}{{slog.LevelInfo, true}, {slog.LevelWarn, false}} {
		// The caller hangs up DURING the attempt, so the loop reaches the hop.
		ctx, cancel := context.WithCancel(context.Background())
		d, sink := dispatcherFailingPrepare(t, func() error { cancel(); return ctx.Err() }, c.level)
		req, body := reqWithBody(`{"model":"day","messages":[]}`)
		_, _, _ = d.ForwardCandidates(ctx, httptest.NewRecorder(), req,
			[]Route{textRoute("http://up.invalid", "day")}, body)
		if got := strings.Contains(sink.String(), "client cancelled, no hop charged"); got != c.want {
			t.Errorf("level %v: line present = %v, want %v\n%s", c.level, got, c.want, sink.String())
		}
		cancel()
	}
}
