package codexbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func waitTemplatesDeleted(t *testing.T, r *Runner) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		r.cache.mu.Lock()
		n, fail := r.cache.count, r.cache.cleanupFailures
		r.cache.mu.Unlock()
		if fail != 0 {
			t.Fatalf("template cleanup failed: %d", fail)
		}
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("template cleanup did not finish")
}
func TestForkCacheReusesOnlyEmptyTemplates(t *testing.T) {
	r, home := fakeRunner(t, "ok")
	r.ForkCache = true
	req := simpleRequest()
	for i := 0; i < 2; i++ {
		req.Input = []UserInput{{Type: "text", Text: strings.Repeat("different request", i+1)}}
		res, err := r.Run(context.Background(), req, nil)
		if err != nil || res.Text == "" {
			t.Fatalf("%+v %v", res, err)
		}
		if r.ParkedTurns() != 0 {
			t.Fatal("unexpected tool park")
		}
	}
	if r.CacheHits() != 1 {
		t.Fatal("fresh request did not reuse template")
	}
	if spawnCount(t, home) != 2 {
		t.Fatal("cache kept or duplicated CLI processes")
	}
	raw, err := os.ReadFile(filepath.Join(home, "cache_rpc.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	calls := string(raw)
	if strings.Count(calls, `"method":"thread/inject_items"`) != 1 || strings.Count(calls, `"method":"thread/fork"`) != 2 || strings.Count(calls, `"method":"thread/resume"`) != 1 {
		t.Fatal(calls)
	}
	if strings.Contains(calls, "different request") {
		t.Fatal("caller dialogue leaked into parent")
	}
	raw, _ = os.ReadFile(filepath.Join(home, "turn_start.json"))
	if !strings.Contains(string(raw), "different requestdifferent request") {
		t.Fatal("fork did not receive complete caller input")
	}
	r.Shutdown()
	waitTemplatesDeleted(t, r)
}
func TestTemplateKeySeparatesConfiguration(t *testing.T) {
	r := &Runner{}
	req := simpleRequest()
	req.Tools = []ToolSpec{{Name: "t", Parameters: json.RawMessage(`{"b":2,"a":1}`)}}
	key, ok := r.templateKey(req)
	if !ok {
		t.Fatal("invalid key")
	}
	req.Input = []UserInput{{Type: "text", Text: "other caller"}}
	req.Tools[0].Parameters = json.RawMessage(`{ "a":1, "b":2 }`)
	other, _ := r.templateKey(req)
	if key != other {
		t.Fatal("equivalent configuration lost affinity")
	}
	for _, change := range []func(*Request){func(q *Request) { q.Model += "other" }, func(q *Request) { q.Instructions += "other" }, func(q *Request) { q.Effort = "high" }, func(q *Request) { q.Summary = "none" }, func(q *Request) { q.OutputSchema = json.RawMessage(`{"type":"object"}`) }, func(q *Request) { q.Tools = nil }} {
		q := req
		change(&q)
		k, _ := r.templateKey(q)
		if k == key {
			t.Fatal("different configuration shared template")
		}
	}
	r.SandboxMode = SandboxWritable
	k, _ := r.templateKey(req)
	if k == key {
		t.Fatal("sandbox shared template")
	}
}
func TestTemplateScratchIsScrubbedAndOldTimerCannotEvictBorrower(t *testing.T) {
	r, _ := fakeRunner(t, "ok")
	r.ForkCache = true
	if _, err := r.Run(context.Background(), simpleRequest(), nil); err != nil {
		t.Fatal(err)
	}
	key, _ := r.templateKey(simpleRequest())
	r.cache.mu.Lock()
	old := r.cache.idle[key][0]
	r.cache.mu.Unlock()
	for _, name := range []string{"cwd", "home", "tmp"} {
		if _, err := os.Stat(filepath.Join(old.root, name)); !os.IsNotExist(err) {
			t.Fatal("scratch retained")
		}
	}
	next := r.borrowTemplate(simpleRequest())
	r.expireTemplate(old)
	if next == nil || next.root != old.root {
		t.Fatal("lease not reused")
	}
	next.release()
	r.Shutdown()
	waitTemplatesDeleted(t, r)
	if _, err := os.Stat(old.root); !os.IsNotExist(err) {
		t.Fatal("template directory leaked")
	}
}
func TestForkCacheFiltersParentEvents(t *testing.T) {
	st := newTurnState(nil)
	st.threadID = "child"
	st.handle("thread/tokenUsage/updated", json.RawMessage(`{"threadId":"parent","tokenUsage":{"total":{"inputTokens":999}}}`))
	st.handle("turn/completed", json.RawMessage(`{"threadId":"parent","turn":{"status":"completed"}}`))
	if st.finished || st.usage.InputTokens != 0 {
		t.Fatal("parent affected child")
	}
	_, err := st.onRequest(json.RawMessage(`1`), "item/tool/call", json.RawMessage(`{"threadId":"parent","callId":"bad","tool":"t","arguments":{}}`))
	if err == nil || len(st.calls) != 0 {
		t.Fatal("parent callback accepted")
	}
}
func TestForkCacheToolContinuationKeepsLease(t *testing.T) {
	r, home := fakeRunner(t, "tools")
	r.ForkCache = true
	req := simpleRequest()
	req.Tools = []ToolSpec{{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)}}
	res, err := r.Run(context.Background(), req, nil)
	if err != nil || len(res.ToolCalls) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	r.cache.mu.Lock()
	idle := len(r.cache.idle)
	r.cache.mu.Unlock()
	if idle != 0 {
		t.Fatal("pending template became idle")
	}
	req.ToolResults = map[string]string{res.ToolCalls[0].ID: "sunny"}
	res, err = r.Run(context.Background(), req, nil)
	if err != nil || !strings.Contains(res.Text, "sunny") {
		t.Fatalf("%+v %v", res, err)
	}
	if spawnCount(t, home) != 1 {
		t.Fatal("tool continuation spawned instead of resuming")
	}
	req.ToolResults = nil
	if _, err = r.Run(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if r.CacheHits() != 1 {
		t.Fatal("completed tool request did not return template")
	}
	r.Shutdown()
	waitTemplatesDeleted(t, r)
}

func TestTemplateBoundsExpiryAndShutdown(t *testing.T) {
	r, _ := fakeRunner(t, "ok")
	r.ForkCache = true
	r.MaxConcurrent = 1
	r.MaxParked = 1
	first := r.borrowTemplate(simpleRequest())
	second := r.borrowTemplate(simpleRequest())
	if first == nil || second == nil || r.borrowTemplate(simpleRequest()) != nil {
		t.Fatal("template credit cap failed")
	}
	first.release()
	second.release()
	waitTemplatesDeleted(t, r)
	for _, expired := range []bool{false, true} {
		if _, err := r.Run(context.Background(), simpleRequest(), nil); err != nil {
			t.Fatal(err)
		}
		key, _ := r.templateKey(simpleRequest())
		r.cache.mu.Lock()
		l := r.cache.idle[key][0]
		r.cache.mu.Unlock()
		if expired {
			r.expireTemplate(l)
			waitTemplatesDeleted(t, r)
		} else {
			next := r.borrowTemplate(simpleRequest())
			next.created = time.Now().Add(-templateAge)
			next.good.Store(true)
			next.release()
			waitTemplatesDeleted(t, r)
		}
	}
	r.Shutdown()
	if r.borrowTemplate(simpleRequest()) != nil {
		t.Fatal("cache reopened after shutdown")
	}
}

func TestForkCacheCancellationDiscardsTemplate(t *testing.T) {
	r, _ := fakeRunner(t, "hang")
	r.ForkCache = true
	r.Timeout = 50 * time.Millisecond
	if _, err := r.Run(context.Background(), simpleRequest(), nil); err == nil {
		t.Fatal("expected deadline")
	}
	r.Shutdown()
	waitTemplatesDeleted(t, r)
}

func TestForkCacheChangedToolConfigurationReplays(t *testing.T) {
	r, home := fakeRunner(t, "tools")
	r.ForkCache = true
	req := simpleRequest()
	req.Tools = []ToolSpec{{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)}}
	res, err := r.Run(context.Background(), req, nil)
	if err != nil || len(res.ToolCalls) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	req.ToolResults = map[string]string{res.ToolCalls[0].ID: "sunny"}
	req.Model = "different-model"
	res, err = r.Run(context.Background(), req, nil)
	if err != nil || len(res.ToolCalls) == 0 {
		t.Fatalf("changed configuration resumed stale turn: %+v %v", res, err)
	}
	if spawnCount(t, home) < 2 {
		t.Fatal("changed model did not spawn")
	}
	r.Shutdown()
	waitTemplatesDeleted(t, r)
}

func TestCacheModeAndUsageValidation(t *testing.T) {
	for _, tc := range []struct{ value, want string }{{"", "auto"}, {"auto", "auto"}, {" AUTO ", "auto"}, {" ON ", "on"}, {"off", "off"}} {
		mode, err := ForkCacheModeFromEnv(func(string) string { return tc.value })
		if err != nil || mode != tc.want {
			t.Fatalf("%q: %q %v", tc.value, mode, err)
		}
	}
	for _, v := range []string{"true", "1", "unknown"} {
		if _, err := ForkCacheModeFromEnv(func(string) string { return v }); err == nil {
			t.Fatalf("accepted typo %q", v)
		}
	}

	for _, u := range []Usage{{}, {InputTokens: 10, CachedTokens: 11}, {InputTokens: 10, OutputTokens: -1}, {InputTokens: 10, OutputTokens: 1, ReasoningTokens: 2}} {
		if validTemplateUsage(u) {
			t.Fatalf("invalid usage %+v", u)
		}
	}
}

func TestCacheStatusExposesOnlyCounts(t *testing.T) {
	r, _ := fakeRunner(t, "ok")
	r.ForkCache = true
	if _, err := r.Run(context.Background(), simpleRequest(), nil); err != nil {
		t.Fatal(err)
	}
	s := &Server{Runner: r}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/status", nil))
	var result struct {
		Cache CacheStats `json:"fork_cache"`
	}
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Cache.Idle != 1 || !result.Cache.Enabled {
		t.Fatal(w.Body.String())
	}
	if strings.Contains(w.Body.String(), "thr-1") || strings.Contains(w.Body.String(), "template-") {
		t.Fatal("identity exposed")
	}
	r.Shutdown()
	waitTemplatesDeleted(t, r)
}
func TestCachePreflightRequiresExperimentalForkShape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	bin := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
[ "$1 $2 $3 $4" = "app-server generate-json-schema --experimental --out" ] || exit 1
mkdir -p "$5/v2"
echo '["thread/fork","thread/resume","thread/inject_items","thread/delete"]' > "$5/ClientRequest.json"
echo '{"experimentalPredictionMode":true,"ephemeral":true,"excludeTurns":true}' > "$5/v2/ThreadForkParams.json"
echo '{"dynamicTools":[],"ephemeral":true}' > "$5/v2/ThreadStartParams.json"
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := RunCachePreflight(context.Background(), bin, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte(strings.ReplaceAll(script, "experimentalPredictionMode", "removedPredictionMode")), 0700); err != nil {
		t.Fatal(err)
	}
	if err := RunCachePreflight(context.Background(), bin, t.TempDir()); err == nil || !strings.Contains(err.Error(), "experimentalPredictionMode") {
		t.Fatalf("%v", err)
	}
}

func TestFailedDeletionRetainsCreditAndStopsOrphanGrowth(t *testing.T) {
	r, _ := fakeRunner(t, "cleanup_error")
	r.ForkCache = true
	r.MaxConcurrent = 1
	r.MaxParked = 1
	for i := uint64(1); i <= 2; i++ {
		if _, err := r.Run(context.Background(), simpleRequest(), nil); err != nil {
			t.Fatal(err)
		}
		key, _ := r.templateKey(simpleRequest())
		r.cache.mu.Lock()
		l := r.cache.idle[key][0]
		r.cache.mu.Unlock()
		r.expireTemplate(l)
		deadline := time.Now().Add(3 * time.Second)
		for r.CacheStats().CleanupFailures < i && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if stats := r.CacheStats(); stats.CleanupFailures != i || stats.Credits != int(i) {
			t.Fatalf("%+v", stats)
		}
		if _, err := os.Stat(l.root); !os.IsNotExist(err) {
			t.Fatal("scratch leaked after failed native delete")
		}
	}
	if r.borrowTemplate(simpleRequest()) != nil {
		t.Fatal("cleanup failures allowed unbounded templates")
	}
	r.Shutdown()
}

func TestAutomaticCacheSelectionBeforeInference(t *testing.T) {
	unsupported := fmt.Errorf("experimental prediction fork unavailable")
	for _, tc := range []struct {
		mode, channel                        string
		supported, enabled, wantError, probe bool
	}{
		{"auto", ChannelAppServer, true, true, false, true},
		{"auto", ChannelAppServer, false, false, false, true},
		{"auto", ChannelExec, false, false, false, false},
		{"on", ChannelAppServer, true, true, false, true},
		{"on", ChannelAppServer, false, false, true, true},
		{"on", ChannelExec, false, false, true, false},
		{"off", ChannelAppServer, true, false, false, false},
	} {
		called := false
		selected, err := selectForkCache(tc.mode, tc.channel, func() error {
			called = true
			if !tc.supported {
				return unsupported
			}
			return nil
		})
		if selected.Mode != tc.mode || selected.Enabled != tc.enabled || (err != nil) != tc.wantError || called != tc.probe {
			t.Fatalf("%+v: %+v err=%v probe=%v", tc, selected, err, called)
		}
		if !selected.Enabled && !tc.wantError && selected.Reason == "" {
			t.Fatal("disabled reason missing")
		}
	}
	for _, tc := range []struct{ value, want string }{{"", "auto"}, {"auto", "auto"}, {"on", "ephemeral-forks"}, {"off", "off"}} {
		policy, err := ForkCachePolicy(func(string) string { return tc.value })
		if err != nil || policy != tc.want {
			t.Fatalf("%q: %q %v", tc.value, policy, err)
		}
	}
}

func TestAutomaticCacheDefaultResolvesCapabilitiesWithoutInference(t *testing.T) {
	// Missing executable stands in for an unsupported experimental schema. This
	// optional capability failure must not prevent auto/off startup.
	bin := filepath.Join(t.TempDir(), "missing-codex")
	selected, err := ResolveForkCache(context.Background(), bin, t.TempDir(), ChannelAppServer, func(string) string { return "" })
	if err != nil || selected.Mode != "auto" || selected.Enabled || selected.Reason == "" {
		t.Fatalf("%+v %v", selected, err)
	}
	if _, err = ResolveForkCache(context.Background(), bin, t.TempDir(), ChannelAppServer, func(string) string { return "on" }); err == nil {
		t.Fatal("required mode ignored missing capability")
	}
	selected, err = ResolveForkCache(context.Background(), bin, t.TempDir(), ChannelAppServer, func(string) string { return "off" })
	if err != nil || selected.Enabled {
		t.Fatalf("%+v %v", selected, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = ResolveForkCache(ctx, bin, t.TempDir(), ChannelAppServer, func(string) string { return "" }); err != context.Canceled {
		t.Fatalf("ignored startup cancellation: %v", err)
	}
}
