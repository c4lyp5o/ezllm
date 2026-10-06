package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// sseFrame is one decoded SSE message.
type sseFrame struct {
	event string
	data  string
}

// readSSE reads frames until the want event arrives or the deadline passes.
// Heartbeats (": ping" comments) are skipped by design.
func readSSE(t *testing.T, br *bufio.Reader, want string, deadline time.Duration) sseFrame {
	t.Helper()
	got := sseFrame{}
	var data []string
	deadlineAt := time.Now().Add(deadline)
	for time.Now().Before(deadlineAt) {
		_ = br.Buffered() // don't block forever on a dead stream
		lineCh := make(chan string, 1)
		errCh := make(chan error, 1)
		go func() {
			line, err := br.ReadString('\n')
			if err != nil {
				errCh <- err
				return
			}
			lineCh <- line
		}()
		var line string
		select {
		case line = <-lineCh:
		case <-errCh:
			t.Fatalf("stream read error while waiting for %q", want)
		case <-time.After(time.Until(deadlineAt)):
			t.Fatalf("timeout waiting for event %q (last: %+v)", want, got)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "": // frame boundary
			if got.event == want && len(data) > 0 {
				got.data = strings.Join(data, "\n")
				return got
			}
			got, data = sseFrame{}, nil
		case strings.HasPrefix(line, ":"): // heartbeat comment
		case strings.HasPrefix(line, "event: "):
			got.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: "))
		}
	}
	t.Fatalf("deadline waiting for event %q", want)
	return got
}

// TestAdminStreamSnapshotThenLive: connect -> snapshot frame; RecordCall ->
// committed row arrives as a live `calls` frame without re-polling.
func TestAdminStreamSnapshotThenLive(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	srv := httptest.NewServer(h.handler)
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL+"/admin/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok-admin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	br := bufio.NewReader(resp.Body)

	// 1) snapshot arrives immediately (even with an empty ledger).
	snap := readSSE(t, br, "snapshot", 5*time.Second)
	var snapBody struct {
		Calls []map[string]any `json:"calls"`
	}
	if err := json.Unmarshal([]byte(snap.data), &snapBody); err != nil {
		t.Fatalf("snapshot data: %v (%s)", err, snap.data)
	}
	if snapBody.Calls == nil {
		t.Fatalf("snapshot calls = nil, want [] (payload: %s)", snap.data)
	}

	// 2) a committed row must arrive live.
	h.db.RecordCall(store.Call{
		AccountID: 1, Account: "acct", Client: "c", Model: "gpt-6-luna",
		Status: 200, TokensIn: 7, TokensOut: 9,
	})
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}

	live := readSSE(t, br, "calls", 5*time.Second)
	var liveBody struct {
		Calls []store.CallEvent `json:"calls"`
	}
	if err := json.Unmarshal([]byte(live.data), &liveBody); err != nil {
		t.Fatalf("calls data: %v (%s)", err, live.data)
	}
	if len(liveBody.Calls) != 1 {
		t.Fatalf("live calls = %d rows, want 1 (%s)", len(liveBody.Calls), live.data)
	}
	row := liveBody.Calls[0]
	if row.Model != "gpt-6-luna" || row.TokensIn != 7 || row.TokensOut != 9 || row.Account != "acct" {
		t.Fatalf("row = %+v, want model/tokens/account carried through", row)
	}
}

// TestAdminStreamRequiresAdminRole: an infer-only token must get 403 and no stream.
func TestAdminStreamRequiresAdminRole(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _, body := h.do("GET", "/admin/stream", "tok-infer", "")
	if code != http.StatusForbidden {
		t.Fatalf("infer token on /admin/stream = %d, want 403 (body %s)", code, body)
	}
}
