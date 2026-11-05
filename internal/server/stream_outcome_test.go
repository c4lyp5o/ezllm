package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/registration"
	"github.com/c4lyp5o/ezllm/internal/router"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// The precedence IS the contract (docs/hop-health-design.md): a caller that hung
// up is never a truncation, and a translated stream's frame-level failure is not
// the same fact as a dead socket.
func TestStreamOutcomeLabelPrecedence(t *testing.T) {
	cases := []struct {
		name string
		res  proxy.Result
		gone bool
		want string
	}{
		{"non-stream", proxy.Result{Stream: false}, false, store.StreamOutcomeNone},
		{"clean stream", proxy.Result{Stream: true}, false, store.StreamOutcomeCompleted},
		{"translated completed", proxy.Result{Stream: true, StreamOutcome: proxy.StreamOutcomeCompleted}, false, store.StreamOutcomeCompleted},
		{"socket died", proxy.Result{Stream: true, StreamOutcome: proxy.StreamOutcomeTruncated}, false, store.StreamOutcomeTruncated},
		{"late error frame", proxy.Result{Stream: true, StreamOutcome: proxy.StreamOutcomeFailed}, false, store.StreamOutcomeFailed},
		// The artefact class: the copy error is the SYMPTOM of the hang-up, so
		// client_gone must win and keep the hop's rate clean.
		{"caller hung up mid-body", proxy.Result{Stream: true, StreamOutcome: proxy.StreamOutcomeTruncated}, true, store.StreamOutcomeClientGone},
		{"caller hung up on a failed stream", proxy.Result{Stream: true, StreamOutcome: proxy.StreamOutcomeFailed}, true, store.StreamOutcomeClientGone},
	}
	for _, tc := range cases {
		if got := streamOutcomeLabel(tc.res, tc.gone); got != tc.want {
			t.Errorf("%s: outcome = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A hop that sends headers and one SSE chunk, then drops the connection. This is
// the measured production shape (command-code resetting mid-answer), and the
// client already holds a 200 by then, so the ONLY record possible is the ledger.
func truncatingSSEUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"object":"list","data":[{"id":"gpt-6-luna","object":"model"}]}`))
			return
		}
		// Declare far more body than we send: returning early makes net/http
		// close the connection mid-body, which is exactly an unexpected EOF on
		// the gateway's read side.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTruncatedStreamIsRecordedAsTruncated(t *testing.T) {
	ctx := context.Background()
	up := truncatingSSEUpstream(t)

	dir := t.TempDir()
	db, err := store.Open(ctx, store.Options{
		Path: filepath.Join(dir, "t.sqlite"), BatchSize: 4, BatchWait: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	id, err := db.UpsertAccount(ctx, provider.Account{
		Name: "flaky", Namespace: "flaky", Kind: provider.KindOpenAICompatible,
		BaseURL: up.URL, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.AddKey(ctx, id, "primary", "sk-flaky-key"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModels(ctx, id, []provider.ModelInfo{{ID: "gpt-6-luna", OwnedBy: "flaky"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertClientToken(ctx, "hermes", "tok-infer", "infer"); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	reg := provider.NewRegistry(client)
	d := proxy.NewDispatcher(client, reg)
	d.SetRetries(0)
	h := New(Options{
		Auth: db, Resolver: router.New(db), Dispatcher: d, DB: db, MaxBodyMiB: 1,
		Registry: reg, Tester: registration.NewTester(client, reg, db),
	}).Handler()

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"flaky/gpt-6-luna","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer tok-infer")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Bounded poll: the ledger is batched, so a fixed sleep is both flakier and
	// slower than this.
	var row store.Call
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, err := db.RecentCalls(ctx, 5)
		if err != nil {
			t.Fatalf("RecentCalls: %v", err)
		}
		if len(rows) > 0 {
			row = rows[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ledger row after 2s (response status %d, body %q)", rec.Code, rec.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if row.StreamOutcome != store.StreamOutcomeTruncated {
		t.Fatalf("stream_outcome = %q, want %q (err=%q, status=%d)",
			row.StreamOutcome, store.StreamOutcomeTruncated, row.Err, row.Status)
	}
	// The prose must still say what happened — the column is the machine fact,
	// not a replacement for the human one.
	if !strings.Contains(row.Err, "stream truncated") {
		t.Errorf("err = %q, want the prose detail retained", row.Err)
	}
}
