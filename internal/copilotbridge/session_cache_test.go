package copilotbridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func cacheTurn(t *testing.T, body string) ChannelTurn {
	t.Helper()
	req, err := ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return ChannelTurn{Model: req.Model, System: req.System, Prompt: req.Transcript, Tools: req.Tools, Choice: req.Choice, Attachments: req.Attachments, cacheHistory: canonicalCacheHistory([]byte(body))}
}

const cacheFirst = `{"model":"gpt-6-luna","messages":[{"role":"system","content":"brief"},{"role":"user","content":"start"}]}`
const cacheNext = `{"model":"gpt-6-luna","messages":[{"role":"system","content":"brief"},{"role":"user","content":"start"},{"role":"assistant","content":"ok"},{"role":"user","content":"next"}]}`

func TestCompletedCacheRequiresExactHistoryAndConfig(t *testing.T) {
	entry := completedFrom(cacheTurn(t, cacheFirst), Result{Text: "ok"})
	next := cacheTurn(t, cacheNext)
	if prompt, ok := entry.continuation(next); !ok || prompt != "User: next" {
		t.Fatalf("continuation = %q %v", prompt, ok)
	}
	for _, tc := range []struct {
		name string
		body string
	}{
		{"edited history", strings.Replace(cacheNext, "start", "edited", 1)},
		{"edited answer", strings.Replace(cacheNext, `"ok"`, `"different"`, 1)},
		{"changed system", strings.Replace(cacheNext, "brief", "different", 1)},
		{"changed model", strings.Replace(cacheNext, "gpt-6-luna", "other-model", 1)},
		{"auto model", strings.Replace(cacheNext, "gpt-6-luna", "auto", 1)},
		{"retry identical request", cacheFirst},
		{"tool result", strings.Replace(cacheNext, `"role":"user","content":"next"`, `"role":"tool","tool_call_id":"a","content":"next"`, 1)},
		{"changed tools", strings.Replace(cacheNext, `"messages":`, `"tools":[{"type":"function","function":{"name":"x","parameters":{"type":"object"}}}],"messages":`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := entry.continuation(cacheTurn(t, tc.body)); ok {
				t.Fatal("unsafe history reused")
			}
		})
	}
	next.Attachments = []ImageAttachment{{Type: "blob"}}
	if _, ok := entry.continuation(next); ok {
		t.Fatal("image continuation cached")
	}
	if got := canonicalCacheHistory([]byte(strings.Repeat(" ", maxSessionHistoryBytes+1))); got != nil {
		t.Fatal("oversize history retained")
	}
}
func TestSessionCacheHTTPReusesOnlyCompletedPrefixAndBillsDeltas(t *testing.T) {
	r, path := poolRig(t, "cache")
	r.SessionCache = true
	s := &Server{Runner: r, Token: "t", Models: []Model{{ID: "gpt-6-luna"}}}
	for i, body := range []string{cacheFirst, cacheNext} {
		rec := postChat(s, body)
		if rec.Code != 200 {
			t.Fatalf("turn %d: %d %s", i, rec.Code, rec.Body.String())
		}
		var out struct {
			Usage struct {
				Prompt  int     `json:"prompt_tokens"`
				Output  int     `json:"completion_tokens"`
				Premium float64 `json:"copilot_premium_requests"`
			}
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		wantPrompt := 100
		if i > 0 {
			wantPrompt = 110
		}
		if out.Usage.Prompt != wantPrompt || out.Usage.Output != 5 || out.Usage.Premium != 1 {
			t.Fatalf("turn %d billed cumulative usage: %+v", i, out.Usage)
		}
	}
	_, calls := poolCalls(t, path)
	joined := strings.Join(calls, "\n")
	if strings.Count(joined, "session.create") != 1 || strings.Count(joined, "session.send") != 2 || strings.Contains(joined, "session.delete") {
		t.Fatalf("session not retained: %v", calls)
	}
	r.Close()
	_, calls = poolCalls(t, path)
	if strings.Count(strings.Join(calls, "\n"), "session.delete") != 1 {
		t.Fatal("idle retained session not deleted at close")
	}
}
func TestSessionCacheMismatchDeletesBeforeFreshSend(t *testing.T) {
	r, path := poolRig(t, "cache-mismatch")
	r.SessionCache = true
	for _, body := range []string{cacheFirst, strings.Replace(cacheNext, "start", "edited", 1)} {
		if _, err := r.RunChannel(context.Background(), cacheTurn(t, body), nil); err != nil {
			t.Fatal(err)
		}
	}
	_, calls := poolCalls(t, path)
	joined := strings.Join(calls, "\n")
	if strings.Count(joined, "session.create") != 2 || strings.Count(joined, "session.delete") != 1 {
		t.Fatalf("did not replace unmatched session: %v", calls)
	}
}
func TestSessionCacheUsageResetNeverRetriesOrReuses(t *testing.T) {
	r, path := poolRig(t, "cache-reset")
	r.SessionCache = true
	if _, err := r.RunChannel(context.Background(), cacheTurn(t, cacheFirst), nil); err != nil {
		t.Fatal(err)
	}
	res, err := r.RunChannel(context.Background(), cacheTurn(t, cacheNext), nil)
	if err == nil || res.Usage.PromptTokens != 0 {
		t.Fatalf("reset accepted: %+v %v", res, err)
	}
	_, calls := poolCalls(t, path)
	if strings.Count(strings.Join(calls, "\n"), "session.send") != 2 {
		t.Fatalf("generation replayed: %v", calls)
	}
}
func TestSessionCacheIdleExpiryDeletesNativeSession(t *testing.T) {
	r, path := poolRig(t, "cache-expiry")
	r.SessionCache = true
	r.PoolIdleTimeout = 15 * time.Millisecond
	if _, err := r.RunChannel(context.Background(), cacheTurn(t, cacheFirst), nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, calls := poolCalls(t, path)
		if strings.Contains(strings.Join(calls, "\n"), "session.delete") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("idle native session not deleted")
}
func TestSessionCacheDisabledKeepsFreshSessions(t *testing.T) {
	r, path := poolRig(t, "normal")
	for _, body := range []string{cacheFirst, cacheNext} {
		if _, err := r.RunChannel(context.Background(), cacheTurn(t, body), nil); err != nil {
			t.Fatal(err)
		}
	}
	_, calls := poolCalls(t, path)
	joined := strings.Join(calls, "\n")
	if strings.Count(joined, "session.create") != 2 || strings.Count(joined, "session.delete") != 2 {
		t.Fatalf("default retained state: %v", calls)
	}
}

func TestSessionCacheLiveGateUsesActualContinuation(t *testing.T) {
	r, _ := poolRig(t, "cache-gate")
	r.SessionCache = true
	if err := LiveSessionCache(context.Background(), r, "gpt-6-luna"); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCacheToolBatchIsAccountedAndNeverRetained(t *testing.T) {
	r, path := poolRig(t, "cache-tools")
	r.SessionCache = true
	first := cacheTurn(t, cacheFirst)
	next := cacheTurn(t, cacheNext)
	first.Tools = channelTurn().Tools
	next.Tools = first.Tools
	if _, err := r.RunChannel(context.Background(), first, nil); err != nil {
		t.Fatal(err)
	}
	res, err := r.RunChannel(context.Background(), next, nil)
	if err != nil || len(res.ToolCalls) != 2 || res.Usage.PromptTokens != 110 || res.Usage.CompletionTokens != 5 {
		t.Fatalf("tool continuation = %+v %v", res, err)
	}
	_, calls := poolCalls(t, path)
	joined := strings.Join(calls, "\n")
	if strings.Count(joined, "session.create") != 1 || strings.Count(joined, "session.abort") != 1 || strings.Count(joined, "session.delete") != 1 {
		t.Fatalf("pending tools retained or replayed: %v", calls)
	}
}
func TestSessionCacheSelectsMatchingIdleWorker(t *testing.T) {
	r, _ := poolRig(t, "cache-pick")
	r.SessionCache = true
	first, err := r.takeWorker()
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.takeWorker()
	if err != nil {
		t.Fatal(err)
	}
	a := cacheTurn(t, cacheFirst)
	b := cacheTurn(t, strings.Replace(cacheFirst, "start", "other", 1))
	first.conn.completed = completedFrom(a, Result{Text: "ok"})
	second.conn.completed = completedFrom(b, Result{Text: "ok"})
	r.poolMu.Lock()
	r.idleWorkers = []*channelWorker{first, second}
	r.poolMu.Unlock()
	next := cacheTurn(t, cacheNext)
	got, err := r.takeWorkerFor(&next)
	if err != nil || got != first {
		t.Fatalf("matching idle session not selected: %p %v", got, err)
	}
	// No actual sessions were created in this selection-only probe.
	first.conn.completed = nil
	second.conn.completed = nil
}

func TestSessionCacheHandlesMovingHermesCacheMarkers(t *testing.T) {
	first := `{"model":"gpt-6-luna","messages":[{"role":"system","content":[{"type":"text","text":"brief","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":[{"type":"text","text":"start","cache_control":{"type":"ephemeral"}}]}]}`
	next := `{"model":"gpt-6-luna","messages":[{"role":"system","content":[{"type":"text","text":"brief"}]},{"role":"user","content":"start"},{"role":"assistant","tool_calls":null,"content":[{"type":"text","text":"ok","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":[{"type":"text","text":"next","cache_control":{"type":"ephemeral"}}]}]}`
	entry := completedFrom(cacheTurn(t, first), Result{Text: "ok"})
	if prompt, ok := entry.continuation(cacheTurn(t, next)); !ok || prompt != "User: next" {
		t.Fatalf("Hermes metadata prevented reuse: %q %v", prompt, ok)
	}
	// A field named cache_control inside actual tool arguments remains data.
	a := canonicalCacheHistory([]byte(`{"messages":[{"role":"assistant","tool_calls":[{"function":{"arguments":{"cache_control":1}}}]}]}`))
	b := canonicalCacheHistory([]byte(`{"messages":[{"role":"assistant","tool_calls":[{"function":{"arguments":{"cache_control":2}}}]}]}`))
	if string(a[0]) == string(b[0]) {
		t.Fatal("tool argument data stripped as metadata")
	}
}
func TestSessionCacheFailedCleanupDoesNotDispatchNewRequest(t *testing.T) {
	r, path := poolRig(t, "negative-delete")
	r.SessionCache = true
	if _, err := r.RunChannel(context.Background(), cacheTurn(t, cacheFirst), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunChannel(context.Background(), cacheTurn(t, strings.Replace(cacheNext, "start", "edited", 1)), nil); err == nil {
		t.Fatal("cleanup failure ignored")
	}
	_, calls := poolCalls(t, path)
	if strings.Count(strings.Join(calls, "\n"), "session.send") != 1 {
		t.Fatalf("new inference dispatched with uncertain cleanup: %v", calls)
	}
}
