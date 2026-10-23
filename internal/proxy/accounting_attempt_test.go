package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAccountingAttemptBudgetPreservesSpend(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		io.WriteString(w, `{"error":{"message":"failed after work"},"usage":{"prompt_tokens":100,"completion_tokens":42}}`)
	}))
	defer up.Close()
	d := newDispatcher()
	d.SetRetries(5)
	d.SetAttemptBudget(1)
	req, body := reqWithBody(`{"model":"m","messages":[]}`)
	rt := textRoute(up.URL, "m")
	rt.Account.ID = 9
	rt.KeyID = 12
	_, res, err := d.ForwardCandidates(context.Background(), httptest.NewRecorder(), req, []Route{rt}, body)
	if err == nil || res.UpstreamAttempts != 1 || len(res.DiscardedUsage) != 1 {
		t.Fatalf("budget contract: err=%v attempts=%d debits=%+v", err, res.UpstreamAttempts, res.DiscardedUsage)
	}
	c := res.DiscardedUsage[0]
	if c.TokensIn != 100 || c.TokensOut != 42 || c.AccountID != 9 || c.KeyID != 12 || res.Usage != nil {
		t.Fatalf("lost or double-counted debit: %+v", res)
	}
}

func TestAccountingDiscardDoesNotInventSpend(t *testing.T) {
	led := &attemptLedger{}
	rt := textRoute("http://unused.invalid", "m")
	resp := &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"no reported usage"}}`)), Header: make(http.Header)}
	discardAttempt(resp, rt, Result{Status: 503}, time.Now(), led)
	if len(led.spent) != 0 {
		t.Fatalf("invented usage: %+v", led.spent)
	}
}
