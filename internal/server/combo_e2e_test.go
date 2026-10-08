package server

// End-to-end proof that a combo ROUTES. Before this milestone a combo name was
// CRUD-only: tables, admin API and dashboard all worked, and the inference
// path 404'd because router.Resolve had "combo name (M4) — not implemented".
// These tests drive the real HTTP handler with a real store, two accounts and
// two fake upstreams.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/registration"
	"github.com/c4lyp5o/ezllm/internal/router"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// twoUpstream is a harness with TWO accounts pointing at two DIFFERENT
// upstreams, so failover is a real hop change rather than a shuffle.
type twoUpstream struct {
	handler http.Handler
	db      *store.DB
	bad     *httptest.Server // answers 429 to chat, 200 to /models
	good    *httptest.Server // answers 200
	// hit records which upstream received the chat call and with what model.
	badHits  []string
	goodHits []string
}

// newTwoUpstream builds the harness. badQuota makes the first upstream reject
// chat completions with 429 while still serving a valid /models catalog — the
// realistic "key works, quota spent" state that failover exists to survive.
func newTwoUpstream(t *testing.T, badQuota bool) *twoUpstream {
	t.Helper()
	tu := &twoUpstream{}

	badBody := `{"ok":false,"model":"from-bad"}`
	if badQuota {
		badBody = `{"error":{"message":"quota exhausted","type":"rate_limit_error","code":"rate_limit_exceeded"}}`
	}
	tu.bad = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Write([]byte(`{"object":"list","data":[{"id":"gpt-6-luna","object":"model","owned_by":"bad"}]}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		tu.badHits = append(tu.badHits, string(b))
		if badQuota {
			w.WriteHeader(http.StatusTooManyRequests)
		}
		w.Write([]byte(badBody))
	}))
	tu.good = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Write([]byte(`{"object":"list","data":[{"id":"gpt-6-luna","object":"model","owned_by":"good"}]}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		tu.goodHits = append(tu.goodHits, string(b))
		w.Write([]byte(`{"ok":true,"model":"from-good"}`))
	}))

	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{
		Path:      filepath.Join(dir, "t.sqlite"),
		BatchSize: 4, BatchWait: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name, ns, base string) int64 {
		id, err := db.UpsertAccount(context.Background(), provider.Account{
			Name: name, Namespace: ns, Kind: provider.KindOpenAICompatible,
			BaseURL: base, Enabled: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := db.AddKey(context.Background(), id, "primary", "sk-"+ns+"...key"); err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertModels(context.Background(), id, []provider.ModelInfo{
			{ID: "gpt-6-luna", OwnedBy: ns},
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	idBad := mk("bad", "bad", tu.bad.URL)
	idGood := mk("good", "good", tu.good.URL)

	if err := db.UpsertClientToken(context.Background(), "hermes", "tok-infer", "infer"); err != nil {
		t.Fatal(err)
	}
	// Admin role is separate from infer: POST /admin/combos must reject the
	// infer token (403), so the harness needs a token that actually has it.
	if err := db.UpsertClientToken(context.Background(), "admin", "tok-admin", "infer,admin"); err != nil {
		t.Fatal(err)
	}
	// day→night: hop 1 is the daytime provider, hop 2 the night one.
	if _, err := db.UpsertCombo(context.Background(), store.Combo{
		Name: "daily", Strategy: "failover", Enabled: true,
		Hops: []store.ComboHop{
			{AccountID: idBad, ModelID: "gpt-6-luna", Weight: 1},
			{AccountID: idGood, ModelID: "gpt-6-luna", Weight: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	registry := provider.NewRegistry(client)
	// Retries are pinned OFF here on purpose: this harness asserts ONE hit per
	// hop so a failover bug cannot hide behind the retry loop. The retry policy
	// itself (attempts, backoff, which statuses qualify) is covered in
	// internal/proxy — keeping that counter out of this test means a change to
	// retry.retries cannot silently rewrite what "failover worked" means.
	dispatcher := proxy.NewDispatcher(client, registry)
	dispatcher.SetRetries(0)
	s := New(Options{
		Auth: db, Resolver: router.New(db),
		Dispatcher: dispatcher,
		DB:         db, MaxBodyMiB: 1,
		Registry: registry, Tester: registration.NewTester(client, registry, db),
	})
	tu.handler = s.Handler()
	tu.db = db
	t.Cleanup(func() { db.Close(); tu.bad.Close(); tu.good.Close() })
	return tu
}

func (tu *twoUpstream) chat(body string) (int, string) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tok-infer")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	tu.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// THE headline: a bare combo name used to 404. It must now route, and when the
// first hop is out of quota the client must see ONLY the second hop's answer —
// no 429, no trace of the failed provider.
func TestComboRoutesAndFailsOverInvisibly(t *testing.T) {
	tu := newTwoUpstream(t, true /* bad hop answers 429 */)

	code, body := tu.chat(`{"model":"daily","messages":[{"role":"user","content":"hi"}]}`)

	if code != http.StatusOK {
		t.Fatalf("combo request = %d, want 200 (failover) — body: %s", code, body)
	}
	if !strings.Contains(body, "from-good") {
		t.Errorf("want the GOOD hop's body, got %s", body)
	}
	if strings.Contains(body, "quota exhausted") {
		t.Errorf("the failed hop's error LEAKED to the client: %s", body)
	}
	if len(tu.badHits) != 1 {
		t.Errorf("bad upstream calls = %d, want 1 (it must have been tried)", len(tu.badHits))
	}
	if len(tu.goodHits) != 1 {
		t.Errorf("good upstream calls = %d, want 1", len(tu.goodHits))
	}
}

// Each hop must receive ITS OWN model. Sending hop 1's body to hop 2 would be
// a silent wrong-model answer, which is worse than an error.
func TestComboSendsRightModelToRightHop(t *testing.T) {
	tu := newTwoUpstream(t, true)

	_, body := tu.chat(`{"model":"daily","messages":[{"role":"user","content":"hi"}]}`)
	if len(tu.goodHits) != 1 {
		t.Fatalf("good hits = %d", len(tu.goodHits))
	}
	if !strings.Contains(tu.goodHits[0], `"model":"gpt-6-luna"`) {
		t.Errorf("good hop got %s, want the model it actually serves", tu.goodHits[0])
	}
	if !strings.Contains(body, "from-good") {
		t.Errorf("body = %s", body)
	}
}

// When the first hop is HEALTHY it must be used — failover that always jumps
// to hop 2 would quietly abandon the account you configured first.
func TestComboUsesFirstHopWhenHealthy(t *testing.T) {
	tu := newTwoUpstream(t, false /* both healthy */)

	code, body := tu.chat(`{"model":"daily","messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusOK || !strings.Contains(body, "from-bad") {
		t.Fatalf("want hop 1's answer, got %d: %s", code, body)
	}
	if len(tu.goodHits) != 0 {
		t.Errorf("healthy hop 1 means hop 2 must NOT be called; it got %d calls", len(tu.goodHits))
	}
}

// An unknown bare name must 404 (not 500) and hint at the combos that exist.
func TestUnknownComboName404sWithHints(t *testing.T) {
	tu := newTwoUpstream(t, false)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"dialy","messages":[]}`))
	req.Header.Set("Authorization", "Bearer tok-infer")
	rec := httptest.NewRecorder()
	tu.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
	// writeErrExtra merges the extra keys directly onto the error object.
	var payload struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	avail, _ := payload.Error["available"].([]any)
	found := false
	for _, a := range avail {
		if a == "daily" {
			found = true
		}
	}
	if !found {
		t.Errorf("hints %v must include the real combo name (typo = transposition)", avail)
	}
}

// Regression: POST /admin/combos WITHOUT an `enabled` field must create an
// ENABLED combo (schema default), not Go's false zero-value. The old decode
// produced a combo that reads back normally in the dashboard and then fails
// every inference request with "combo is disabled" — invisible until traffic.
func TestComboCreateOmitsEnabledDefaultsOn(t *testing.T) {
	tu := newTwoUpstream(t, false)

	req := httptest.NewRequest("POST", "/admin/combos", strings.NewReader(
		`{"name":"apionly","strategy":"failover","hops":[]}`))
	req.Header.Set("Authorization", "Bearer tok-admin")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	tu.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !created.Enabled {
		t.Error("omitted `enabled` created a DISABLED combo — API callers get a silently broken route")
	}
}

// An explicit false must still disable. Fixing the default must not remove
// the ability to turn a combo off.
func TestComboExplicitFalseStillDisables(t *testing.T) {
	tu := newTwoUpstream(t, false)

	req := httptest.NewRequest("POST", "/admin/combos", strings.NewReader(
		`{"name":"off","strategy":"failover","enabled":false,"hops":[]}`))
	req.Header.Set("Authorization", "Bearer tok-admin")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	tu.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Enabled bool `json:"enabled"`
	}
	json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Enabled {
		t.Error(`explicit "enabled":false must disable the combo`)
	}
}

// Direct ns/model routes must be untouched by all of this.
func TestDirectRouteStillWorks(t *testing.T) {
	tu := newTwoUpstream(t, false)

	code, body := tu.chat(`{"model":"good/gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusOK || !strings.Contains(body, "from-good") {
		t.Fatalf("direct route = %d: %s", code, body)
	}
	if len(tu.goodHits) != 1 {
		t.Errorf("good hits = %d, want 1", len(tu.goodHits))
	}
}

// Regression for the attribution bug found in LIVE testing: after failover the
// ledger row must name the hop that actually served, not hop 1.
//
// The first live run recorded `alias=daynight account=deadprobe status=200`
// when deadprobe had just answered 429 and stubtwo did the work. Usage reports
// and (eventually) per-account caps read that column, so a wrong value there
// meters the wrong budget.
func TestLedgerCreditsTheHopThatServed(t *testing.T) {
	tu := newTwoUpstream(t, true /* hop 1 answers 429 */) // hop1=bad, hop2=good

	code, body := tu.chat(`{"model":"daily","messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, body)
	}

	// Wait for the ledger batch to flush (BatchWait=20ms, BatchSize=4).
	var account, alias string
	var status int
	for i := 0; i < 100; i++ {
		rows, err := tu.db.Reader().QueryContext(context.Background(),
			`SELECT COALESCE(account,''), alias, status FROM calls
			 WHERE alias='daily' ORDER BY id DESC LIMIT 1`)
		if err == nil {
			if rows.Next() {
				_ = rows.Scan(&account, &alias, &status)
			}
			rows.Close()
			if account != "" {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if alias != "daily" {
		t.Fatalf("no ledger row for the combo call (alias=%q)", alias)
	}
	if account != "good" {
		t.Errorf(`ledger credited account %q, want "good" — the hop that ANSWERED`, account)
	}
	if status != http.StatusOK {
		t.Errorf("ledger status = %d, want 200", status)
	}
}

// The account that failed must NOT be credited with a success.
func TestFailedHopIsNeverCreditedWithSuccess(t *testing.T) {
	tu := newTwoUpstream(t, true)

	if code, body := tu.chat(`{"model":"daily","messages":[{"role":"user","content":"hi"}]}`); code != 200 {
		t.Fatalf("status = %d: %s", code, body)
	}
	for i := 0; i < 100; i++ {
		time.Sleep(20 * time.Millisecond)
		var n int
		_ = tu.db.Reader().QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM calls WHERE account='bad' AND alias='daily' AND status=200`).Scan(&n)
		if n > 0 {
			t.Fatalf("the FAILED hop was credited with %d success row(s)", n)
		}
		var total int
		_ = tu.db.Reader().QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM calls WHERE alias='daily'`).Scan(&total)
		if total >= 1 {
			// row exists under 'good' → done
			var good int
			_ = tu.db.Reader().QueryRowContext(context.Background(),
				`SELECT COUNT(*) FROM calls WHERE account='good' AND alias='daily'`).Scan(&good)
			if good >= 1 {
				return
			}
		}
	}
	t.Error("expected a ledger row crediting account 'good'")
}
