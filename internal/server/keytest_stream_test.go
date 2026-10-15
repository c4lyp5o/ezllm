package server

// The step chain must MOVE (docx item 8): "this does not move or get colored
// at all for each step done". Before this, POST /admin/accounts/{id}/keys
// answered once, with all steps at the end — on a slow provider the operator
// stared at a dead spinner for 15–60s, then the chain snapped in colored.
//
// The fix is real streaming, not staged animation: the handler accepts
// `Accept: application/x-ndjson` and flushes one line per step as the test
// walks. These tests pin the wire contract:
//   - steps arrive incrementally (the first step line is readable while the
//     upstream is still answering a LATER step — a one-shot body cannot pass)
//   - the stream ends with a result line shaped like the historical 201 body
//   - a failing test ends with an error line carrying the SAME payload the
//     one-shot 422 would have sent (keyTestError is the single source)
//   - clients that do not opt in keep the exact historical JSON behavior

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// slowChainUpstream answers /models and /usage at once, then stalls the
// INFERENCE call — so any step line that arrives before the stall ends is
// proof the stream flushed early.
func slowChainUpstream(inferenceStarted chan<- struct{}, release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			w.Write([]byte(`{"object":"list","data":[{"id":"m-one"}]}`))
		case strings.HasSuffix(r.URL.Path, "/usage"):
			w.WriteHeader(http.StatusNotFound) // auth defers to inference
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			if inferenceStarted != nil {
				close(inferenceStarted)
			}
			<-release // hold the LAST step open until the test says go
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// ndjsonClient performs the opt-in streaming request and returns the
// response; the caller reads lines off resp.Body.
func ndjsonClient(t *testing.T, h *harness, accountID int64, body string) *http.Response {
	t.Helper()
	// A real listener: the in-process harness recorder cannot express
	// incremental delivery, and "bytes leave NOW" is the whole claim.
	ts := httptest.NewServer(h.handler)
	t.Cleanup(ts.Close)
	req, err := http.NewRequest("POST", fmt.Sprintf("%s/admin/accounts/%d/keys", ts.URL, accountID),
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+adminTok())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// readLine reads one NDJSON line with a deadline — a stream that buffers
// everything to the end fails here while the inference stall is still held.
func readLine(t *testing.T, r *bufio.Reader, within time.Duration) map[string]any {
	t.Helper()
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		line, err := r.ReadString('\n')
		ch <- res{line, err}
	}()
	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatalf("read line: %v", got.err)
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(got.line), &v); err != nil {
			t.Fatalf("line %q is not JSON: %v", strings.TrimSpace(got.line), err)
		}
		return v
	case <-time.After(within):
		t.Fatalf("no line within %v — the stream is buffered, not flushed", within)
	}
	return nil
}

func TestKeyTestStreamsEachStepBeforeTheTestEnds(t *testing.T) {
	inferenceStarted := make(chan struct{})
	release := make(chan struct{})
	h := newHarness(t, slowChainUpstream(inferenceStarted, release))
	acct := seededAccountID(t, h)

	resp := ndjsonClient(t, h, acct, `{"label":"primary","api_key":"dummy-key-streaming"}`)

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/x-ndjson") {
		t.Fatalf("Content-Type = %q, want application/x-ndjson", ct)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200 (headers precede the body)", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)

	// Release the inference stall only AFTER the upstream reports it started
	// — so any step line read below arrived while the test was still running.
	go func() { <-inferenceStarted; time.Sleep(50 * time.Millisecond); close(release) }()

	// The catalog step must be readable within the release window — a
	// buffered one-shot body produces no bytes until the handler returns.
	first := readLine(t, reader, 5*time.Second)
	if first["type"] != "step" {
		t.Fatalf("first line type = %v, want step", first["type"])
	}
	st, _ := first["step"].(map[string]any)
	if st == nil || st["step"] != "catalog" || st["ok"] != true {
		t.Fatalf("first streamed step = %v, want catalog ok", first["step"])
	}

	// Drain the rest: more steps, then exactly one terminal result line.
	sawInference := false
	for {
		line := readLine(t, reader, 10*time.Second)
		switch line["type"] {
		case "step":
			s, _ := line["step"].(map[string]any)
			if s != nil && s["step"] == "inference" {
				sawInference = s["ok"] == true
			}
		case "result":
			if !sawInference {
				t.Fatalf("result arrived without a streamed inference step")
			}
			test, _ := line["test"].(map[string]any)
			if test == nil || test["ok"] != true {
				t.Fatalf("result.test = %v, want ok", line["test"])
			}
			if line["key"] == nil {
				t.Fatalf("result line missing key: %v", line)
			}
			return
		default:
			t.Fatalf("unexpected line: %v", line)
		}
	}
}

// A failing streamed test ends with an error line carrying the SAME payload
// the one-shot 422 sends — keyTestError is the single source for both.
func TestKeyTestStreamErrorMatchesOneShotPayload(t *testing.T) {
	// Bad-key upstream: catalog rejects with 401 + provider text.
	badUpstream := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"Invalid API key provided."}}`))
	}
	h := newHarness(t, badUpstream)
	acct := seededAccountID(t, h)

	// One-shot reference: the historical 422 body.
	code, resp := addKey(t, h, acct, map[string]any{"label": "bad", "api_key": "dummy-key-rejected"})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("one-shot must 422, got %d: %s", code, resp)
	}
	want := errObj(t, resp)

	// Streaming run against the same upstream.
	streamed := ndjsonClient(t, h, acct, `{"label":"bad","api_key":"dummy-key-rejected"}`)
	if streamed.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", streamed.StatusCode)
	}
	reader := bufio.NewReader(streamed.Body)
	var got map[string]any
	var gotStatus float64
	steps := 0
	for {
		line := readLine(t, reader, 10*time.Second)
		switch line["type"] {
		case "step":
			steps++
		case "error":
			got, _ = line["error"].(map[string]any)
			gotStatus, _ = line["status"].(float64)
		case "result":
			t.Fatalf("failing test emitted a result line: %v", line)
		}
		if got != nil {
			break
		}
	}
	if got == nil {
		t.Fatalf("stream never emitted an error line")
	}
	if int(gotStatus) != http.StatusUnprocessableEntity {
		t.Fatalf("error.status = %v, want 422", gotStatus)
	}
	for _, k := range []string{"message", "type", "code", "step", "steps"} {
		if _, ok := got[k]; !ok {
			t.Errorf("streamed error missing %q: %v", k, got)
		}
	}
	if got["message"] != want["message"] || got["step"] != want["step"] || got["type"] != want["type"] {
		t.Fatalf("streamed error drifted from the one-shot payload:\n got=%v\nwant=%v", got, want)
	}
}

// The opt-out contract: a client without the Accept header keeps the
// historical one-shot behavior (single JSON body, real HTTP status).
func TestKeyTestWithoutStreamHeaderKeepsOneShotJSON(t *testing.T) {
	badUpstream := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"Invalid API key provided."}}`))
	}
	h := newHarness(t, badUpstream)
	acct := seededAccountID(t, h)
	code, resp := addKey(t, h, acct, map[string]any{"label": "bad", "api_key": "dummy-key-rejected"})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("no-Accept client must keep the one-shot 422, got %d: %s", code, resp)
	}
	if strings.Contains(resp, `"type":"step"`) || strings.Contains(resp, `"type":"result"`) {
		t.Fatalf("one-shot body leaked the streaming shape: %s", resp)
	}
	// Body must be a single JSON object (the NDJSON stream is multi-line).
	if strings.Count(strings.TrimSpace(resp), "\n") > 0 {
		t.Fatalf("one-shot body must be one JSON object, got multiple lines: %q", resp)
	}
}
