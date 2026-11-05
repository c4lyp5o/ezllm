package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// The rollup is the operator surface: it must report the rate the ledger
// implies, exclude callers who hung up, and refuse a nonsense window instead of
// silently defaulting.
func TestHealthReportsHopStreamHealth(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, store.Options{
		Path: filepath.Join(dir, "t.sqlite"), BatchSize: 8, BatchWait: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.UpsertClientToken(ctx, "admin", "tok-admin", "infer,admin"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, outcome := range []string{store.StreamOutcomeCompleted, store.StreamOutcomeTruncated} {
		db.RecordCall(store.Call{TS: now.Add(-time.Minute), Client: "c", Account: "flaky",
			Model: "gpt-6-luna", Status: 200, Stream: true, StreamOutcome: outcome})
	}
	for i := 0; i < 4; i++ {
		db.RecordCall(store.Call{TS: now.Add(-time.Minute), Client: "c", Account: "flaky",
			Model: "gpt-6-luna", Status: 200, Stream: true, StreamOutcome: store.StreamOutcomeClientGone})
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	h := New(Options{Auth: db, DB: db, MaxBodyMiB: 1}).Handler()

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer tok-admin")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := get("/admin/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		HopStreamHealth []store.HopStream `json:"hop_stream_health"`
		StreamWindowS   int               `json:"stream_window_s"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if body.StreamWindowS != 24*3600 {
		t.Errorf("stream_window_s = %d, want the 24h default", body.StreamWindowS)
	}
	if len(body.HopStreamHealth) != 1 {
		t.Fatalf("hop_stream_health = %+v, want one hop", body.HopStreamHealth)
	}
	hop := body.HopStreamHealth[0]
	if hop.Namespace != "flaky" || hop.Model != "gpt-6-luna" {
		t.Errorf("hop = %+v, want flaky/gpt-6-luna", hop)
	}
	if hop.Streams != 6 || hop.Truncated != 1 || hop.ClientGone != 4 || hop.Completed != 1 {
		t.Errorf("hop counts = %+v, want streams=6 completed=1 truncated=1 client_gone=4", hop)
	}
	if hop.Rate != 0.5 {
		t.Errorf("truncation_rate = %v, want 0.5 (1 truncated of 2 judged; the 4 hangups are not judged)", hop.Rate)
	}

	// A nonsense window is a 400, never a silent default.
	if bad := get("/admin/health?stream_window=nope"); bad.Code != http.StatusBadRequest {
		t.Errorf("bad window status = %d, want 400 (body %s)", bad.Code, bad.Body.String())
	}
	// The DASHBOARD reads /admin/overview, not /admin/health — the field must be
	// on both or the operator sees nothing (this is how the card first shipped
	// blank).
	ov := get("/admin/overview")
	if ov.Code != http.StatusOK {
		t.Fatalf("overview status = %d", ov.Code)
	}
	if !strings.Contains(ov.Body.String(), `"hop_stream_health"`) {
		t.Errorf("overview payload lacks hop_stream_health: %.300s", ov.Body.String())
	}
	// And the window actually narrows: a 1ns window excludes every row.
	if narrow := get("/admin/health?stream_window=1ns"); narrow.Code != http.StatusOK ||
		!strings.Contains(narrow.Body.String(), `"hop_stream_health":[]`) {
		t.Errorf("narrow window = %d %s, want an empty hop list", narrow.Code, narrow.Body.String())
	}
}

// The bell must fire for a hop past the dashboard's thresholds, stay quiet for
// one under them, and fold repeats into one line.
func TestCheckHopStreamsRaisesOneFoldedBellPerFlakyHop(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{
		Path: filepath.Join(t.TempDir(), "t.sqlite"), BatchSize: 8, BatchWait: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Now().UTC()
	rec := func(acct, outcome string, n int) {
		for i := 0; i < n; i++ {
			db.RecordCall(store.Call{TS: now.Add(-time.Minute), Client: "c", Account: acct,
				Model: "m", Status: 200, Stream: true, StreamOutcome: outcome})
		}
	}
	rec("flaky", store.StreamOutcomeCompleted, 28)
	rec("flaky", store.StreamOutcomeTruncated, 2)   // 6.7% of 30 judged: alert
	rec("flaky", store.StreamOutcomeClientGone, 50) // never judged
	rec("thin", store.StreamOutcomeTruncated, 3)    // 100% but only 3 judged: too thin
	rec("steady", store.StreamOutcomeCompleted, 99)
	rec("steady", store.StreamOutcomeTruncated, 0) // 0%: quiet
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	s := New(Options{Auth: db, DB: db, MaxBodyMiB: 1})
	s.checkHopStreams(ctx, now)
	s.checkHopStreams(ctx, now) // a repeat folds

	items, _, err := db.ListNotifications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("notifications = %+v, want exactly one (flaky)", items)
	}
	n := items[0]
	if n.Kind != store.NoteStreamTruncation || n.Severity != "warn" || !strings.Contains(n.Title, "flaky") {
		t.Errorf("notification = %+v", n)
	}
	if n.Count != 2 {
		t.Errorf("count = %d, want 2 (repeat folded, not duplicated)", n.Count)
	}
}
