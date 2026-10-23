package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

func TestRecoveryExportQueryFailureIsNotSuccess(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	if _, err := h.db.Writer().Exec(`ALTER TABLE calls RENAME COLUMN reasoning_tokens TO unavailable_reasoning`); err != nil {
		t.Fatal(err)
	}
	code, _, body := h.do("GET", "/admin/export?include_ledger=true", adminTok(), "")
	if code < 400 {
		t.Fatalf("query failure looked successful: %d %s", code, body)
	}
}

func TestRecoveryExportMidstreamFailureHasIncompleteManifest(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	for range 2 {
		h.db.RecordCall(store.Call{TS: time.Now().UTC(), Client: "recovery", ReasoningTokens: 3})
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Writer().Exec(`UPDATE calls SET reasoning_tokens='invalid-counter' WHERE id=(SELECT MIN(id) FROM calls)`); err != nil {
		t.Fatal(err)
	}
	code, _, body := h.do("GET", "/admin/export?include_ledger=true", adminTok(), "")
	var out struct {
		Ledger     []json.RawMessage `json:"ledger"`
		Completion struct {
			Complete bool   `json:"complete"`
			Rows     int    `json:"ledger_rows"`
			Error    string `json:"error"`
		} `json:"completion"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if code != 200 || out.Completion.Complete || out.Completion.Error == "" || out.Completion.Rows != 1 || len(out.Ledger) != 1 {
		t.Fatalf("missing incomplete signal: %d %s", code, body)
	}
}

func TestRecoveryExportSuccessHasManifest(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	for _, suffix := range []string{"", "?include_ledger=true"} {
		code, _, body := h.do("GET", "/admin/export"+suffix, adminTok(), "")
		var out struct {
			Completion *struct {
				Complete  bool `json:"complete"`
				Rows      int  `json:"ledger_rows"`
				Requested bool `json:"ledger_requested"`
			} `json:"completion"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		if code != 200 || out.Completion == nil || !out.Completion.Complete || out.Completion.Rows != 0 || out.Completion.Requested != (suffix != "") {
			t.Fatalf("completion manifest: %d %s", code, body)
		}
	}
}

type recoveryFailingWriter struct {
	*httptest.ResponseRecorder
	failed       bool
	afterFailure int
}

func (w *recoveryFailingWriter) Write(p []byte) (int, error) {
	if w.failed {
		w.afterFailure++
		return 0, io.ErrClosedPipe
	}
	if bytes.Contains(p, []byte(`"client":"export-disconnect"`)) {
		w.failed = true
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(p)
}
func TestRecoveryExportDisconnectHasNoSuccessMarker(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	h.db.RecordCall(store.Call{TS: time.Now().UTC(), Client: "export-disconnect"})
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
	w := &recoveryFailingWriter{ResponseRecorder: httptest.NewRecorder()}
	h.server.handleExport(w, httptest.NewRequest("GET", "/admin/export?include_ledger=true", nil))
	if !w.failed || w.afterFailure != 0 || bytes.Contains(w.Body.Bytes(), []byte(`"completion"`)) {
		t.Fatalf("client failure produced success: %s", w.Body.String())
	}
}
