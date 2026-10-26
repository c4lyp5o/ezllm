package registration

// ProbeAll lifts the 6-model sample cap for the "test all models" action, and
// (unlike the sample) keeps catalog order so the UI can report progress
// against the list it shows.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func eightModels() []provider.ModelInfo {
	var ms []provider.ModelInfo
	for _, id := range []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8"} {
		ms = append(ms, provider.ModelInfo{ID: id})
	}
	return ms
}

// probeLog answers 200 JSON everywhere and records the model of every probe.
type probeLog struct {
	mu     sync.Mutex
	models []string
}

func (l *probeLog) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodPost {
		var in struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		l.mu.Lock()
		l.models = append(l.models, in.Model)
		l.mu.Unlock()
	}
	_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
}

func (l *probeLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.models))
	copy(out, l.models)
	return out
}

func (l *probeLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.models = nil
}

func newSweepTester(t *testing.T) (*Tester, *probeLog, string) {
	t.Helper()
	log := &probeLog{}
	srv := httptest.NewServer(http.HandlerFunc(log.handler))
	t.Cleanup(srv.Close)
	reg := provider.NewRegistry(srv.Client())
	return NewTester(srv.Client(), reg, nil), log, srv.URL
}

func TestProbeAllSweepsEveryModelInCatalogOrder(t *testing.T) {
	tester, log, url := newSweepTester(t)
	acct := provider.Account{Name: "t", Namespace: "t", Kind: provider.KindOpenAICompatible,
		BaseURL: url, Enabled: true}
	models := eightModels()

	// Default behaviour: the sample stays capped at DefaultProbeMaxModels.
	res := tester.ProtocolSweep(context.Background(), acct, "k", models,
		Options{Surfaces: []provider.Surface{provider.SurfaceChat}})
	if res == nil {
		t.Fatal("no result from the sampled sweep")
	}
	if got := len(log.snapshot()); got > DefaultProbeMaxModels {
		t.Fatalf("default sweep probed %d models, want <= %d (the sample cap)", got, DefaultProbeMaxModels)
	}

	// ProbeAll: every model, in the order the caller handed them over.
	log.reset()
	res = tester.ProtocolSweep(context.Background(), acct, "k", models, Options{
		Surfaces: []provider.Surface{provider.SurfaceChat}, ProbeAll: true,
	})
	if res == nil || res.Tested != len(models) {
		t.Fatalf("Tested = %+v, want %d", res, len(models))
	}
	got := log.snapshot()
	if len(got) != len(models) {
		t.Fatalf("ProbeAll probed %d models, want %d (the whole catalog)", len(got), len(models))
	}
	want := []string{"m1", "m2", "m3", "m4", "m5", "m6", "m7", "m8"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order[%d] = %q, want %q (catalog order, not a shuffle): %v", i, got[i], want[i], got)
		}
	}
}
