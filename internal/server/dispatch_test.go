package server

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/ratelimit"
)

func transcriptionBody(t *testing.T, model string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", model)
	fw, _ := mw.CreateFormFile("file", "a.wav")
	_, _ = fw.Write([]byte("RIFF....WAVE"))
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

// M2: the multipart surfaces kept their own post-dispatch copy and drifted —
// a request refused by the account's request budget answered 502 (an outage)
// instead of 429 with next_allowed, so clients retried at once.
func TestMultipartRateLimitedIs429(t *testing.T) {
	upstreamCalls := 0
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hello"}`))
	}, func(o *Options) { o.Dispatcher.SetLimiter(ratelimit.New(time.Now)) })
	acct := seededAccountID(t, h)
	if _, err := h.db.Writer().ExecContext(context.Background(),
		`UPDATE accounts SET rate_limit=1, rate_window='1h' WHERE id=?`, acct); err != nil {
		t.Fatal(err)
	}

	send := func() *httptest.ResponseRecorder {
		body, ctype := transcriptionBody(t, "up/gpt-6-luna")
		req := httptest.NewRequest("POST", "/v1/audio/transcriptions", body)
		req.Header.Set("Authorization", "Bearer tok-infer")
		req.Header.Set("Content-Type", ctype)
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := send(); rec.Code != http.StatusOK {
		t.Fatalf("first transcription = %d %s, want 200 (inside the budget)", rec.Code, rec.Body.String())
	}
	rec := send()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-budget transcription = %d %s, want 429", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Type string `json:"type"`
			Rule struct {
				Kind        string `json:"kind"`
				NextAllowed string `json:"next_allowed"`
			} `json:"rule"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("429 body: %v %s", err, rec.Body.String())
	}
	if env.Error.Type != "model_unavailable" || env.Error.Rule.Kind != "rate" || env.Error.Rule.NextAllowed == "" {
		t.Fatalf("429 must name the rate rule and next_allowed: %s", rec.Body.String())
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream calls = %d, want 1 (the refusal makes no upstream call)", upstreamCalls)
	}
}

func TestAppendNote(t *testing.T) {
	for _, c := range [][3]string{{"", "", ""}, {"a", "", "a"}, {"", "b", "b"}, {"a", "b", "a; b"}} {
		if got := appendNote(c[0], c[1]); got != c[2] {
			t.Errorf("appendNote(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

// M5 end to end: a least_used combo spreads consecutive requests over its
// hops instead of always taking the first.
func TestLeastUsedComboSpreadsRequests(t *testing.T) {
	var served []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		served = append(served, body.Model)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	acct := seededAccountID(t, h)
	if err := h.db.UpsertModels(context.Background(), acct, []provider.ModelInfo{{ID: "gpt-6-luna"}, {ID: "gpt-6-sol"}}); err != nil {
		t.Fatal(err)
	}
	// Unique per run: least_used state is process-wide, keyed by combo name.
	name := "spread-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	code, resp := jsonDo(t, h, "POST", "/admin/combos", "tok-admin", map[string]any{
		"name": name, "strategy": "least_used", "enabled": true,
		"hops": []map[string]any{{"account_id": acct, "model_id": "gpt-6-luna"}, {"account_id": acct, "model_id": "gpt-6-sol"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create combo: %d %s", code, resp)
	}
	for i := 0; i < 4; i++ {
		c, _, b := h.do("POST", "/v1/chat/completions", "tok-infer", `{"model":"`+name+`","messages":[{"role":"user","content":"hi"}]}`)
		if c != 200 {
			t.Fatalf("request %d = %d %s", i, c, b)
		}
	}
	want := []string{"gpt-6-luna", "gpt-6-sol", "gpt-6-luna", "gpt-6-sol"}
	for i := range want {
		if i >= len(served) || served[i] != want[i] {
			t.Fatalf("served = %v, want %v (alternating by recent traffic)", served, want)
		}
	}
}
