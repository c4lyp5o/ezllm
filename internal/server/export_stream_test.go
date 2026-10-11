package server

// S9 regression: the export materialised the entire ledger as one
// []map[string]any and encoded it in a single write, so the handler's memory
// scaled with ledger size. It now streams, and the row cap is GONE — a DR file
// must be complete, and bounding it means silently omitting history from the
// thing you recover with.
//
// The real risk of streaming is a malformed body: one stray separator and the
// download is unparseable. So the proof here is a full round-trip — stream the
// response, parse the WHOLE thing back, and compare every count.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// exportLedgerCount is deliberately larger than the old 10k cap so a silent
// reintroduction of a LIMIT would fail this test.
const exportLedgerCount = 12000

func seedExportCalls(t *testing.T, h *harness, n int) {
	t.Helper()
	// Record in chunks and Flush each one. RecordCall is deliberately
	// non-blocking (a slow DB must never add latency to a token stream), so
	// firing 12k rows in a tight loop sheds them at the queue — which is the
	// ledger's backpressure path, not an export defect. Draining proves the
	// seed itself is complete before the export is blamed for a shortfall.
	const chunk = 250
	for i := 0; i < n; i++ {
		h.db.RecordCall(store.Call{
			TS:      time.Now().UTC().Add(-time.Duration(i) * time.Second),
			Client:  "export-test",
			Account: "up",
			Model:   "gpt-6-luna",
			Status:  200,
			Alias:   "up/gpt-6-luna",
		})
		if (i+1)%chunk == 0 {
			if err := h.db.Flush(); err != nil {
				t.Fatalf("flush at %d: %v", i+1, err)
			}
		}
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	if dropped := store.DroppedCalls(); dropped != 0 {
		t.Fatalf("seed shed %d rows at the ledger queue — the fixture is incomplete, not the export", dropped)
	}
}

// Every ledger row survives the stream, and the body is valid JSON.
func TestExportStreamsCompleteLedger(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	seedExportCalls(t, h, exportLedgerCount)

	code, hdr, body := h.do("GET", "/admin/export?include_ledger=true", "tok-admin", "")
	if code != 200 {
		t.Fatalf("export = %d: %s", code, body)
	}
	// Attachment so a browser downloads rather than renders, as before.
	if hdr.Get("Content-Disposition") == "" {
		t.Error("export must stay an attachment download")
	}

	// The whole point: it parses as one JSON document.
	var doc struct {
		ExportedAt    string           `json:"exported_at"`
		SchemaVersion int              `json:"schema_version"`
		Accounts      []map[string]any `json:"accounts"`
		Ledger        []map[string]any `json:"ledger"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("streamed export is not valid JSON (len=%d): %v", len(body), err)
	}
	if len(doc.Accounts) != 1 {
		t.Errorf("accounts = %d, want the 1 seeded account", len(doc.Accounts))
	}
	// THE assertion: no row was dropped by a cap.
	if len(doc.Ledger) != exportLedgerCount {
		t.Fatalf("ledger rows = %d, want %d — a row cap crept back in or the stream lost rows",
			len(doc.Ledger), exportLedgerCount)
	}
	// And each row kept its projection shape.
	row := doc.Ledger[0]
	for _, k := range []string{"ts", "client", "surface", "alias", "account", "model", "status", "stream",
		"ttft_ms", "total_ms", "tokens_in", "tokens_out", "cached_read", "cached_write", "reasoning"} {
		if _, ok := row[k]; !ok {
			t.Errorf("ledger row lost field %q — the projection drifted", k)
		}
	}
	if doc.SchemaVersion == 0 {
		t.Error("schema_version missing from export")
	}
}

// Without include_ledger the key must be ABSENT (omitempty), not an empty
// array — the original contract, and a streaming rewrite is exactly where that
// gets quietly broken.
func TestExportOmitsLedgerKeyWhenNotRequested(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	h.db.RecordCall(store.Call{TS: time.Now().UTC(), Client: "c", Account: "up", Model: "m", Status: 200})
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}

	code, _, body := h.do("GET", "/admin/export", "tok-admin", "")
	if code != 200 {
		t.Fatalf("export = %d: %s", code, body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if _, present := raw["ledger"]; present {
		t.Error("ledger key must be omitted when include_ledger is absent")
	}
	if _, present := raw["accounts"]; !present {
		t.Error("accounts key missing from the config half")
	}
}

// An empty ledger must still produce a valid document — the degenerate case a
// hand-rolled array writer is most likely to get wrong.
func TestExportEmptyLedgerIsValidJSON(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, _, body := h.do("GET", "/admin/export?include_ledger=true", "tok-admin", "")
	if code != 200 {
		t.Fatalf("export = %d: %s", code, body)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("empty-ledger export is not valid JSON: %v", err)
	}
	if _, ok := doc["ledger"]; !ok {
		t.Fatal("ledger key must be present (as an empty array) when requested")
	}
}
