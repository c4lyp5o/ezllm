package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

func bell(t *testing.T, h *harness) (items []store.Notification, unread int) {
	t.Helper()
	code, _, body := h.do("GET", "/admin/notifications", "tok-admin", "")
	if code != 200 {
		t.Fatalf("GET /admin/notifications = %d %s", code, body)
	}
	var out struct {
		Items  []store.Notification `json:"items"`
		Unread int                  `json:"unread"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out.Items, out.Unread
}

func byKind(items []store.Notification, kind string) []store.Notification {
	var out []store.Notification
	for _, n := range items {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

// The bell tells the operator about free models coming and going, and about
// a new free model that failed every probe.
func TestCatalogEventsRaiseNotifications(t *testing.T) {
	var mu sync.Mutex
	catalog := `{"data":[{"id":"gpt-6-luna"},{"id":"tiny:free"},{"id":"broken-free"}]}`
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			mu.Lock()
			_, _ = w.Write([]byte(catalog))
			mu.Unlock()
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), `"broken-free"`) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"no such model"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}, func(o *Options) { o.ComboAutoProbe = true })
	acct := seededAccountID(t, h)
	ctx := context.Background()

	h.server.refreshCatalogsOnce(ctx)
	deadline := time.Now().Add(20 * time.Second)
	for len(byKind(must(bell(t, h)), store.NoteFreeModelBroken)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no free_model_broken notification after the probe")
		}
		time.Sleep(50 * time.Millisecond)
	}
	for !sweepIdle(h, acct) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	items, _ := bell(t, h)
	added := byKind(items, store.NoteFreeModelsAdded)
	if len(added) != 1 || !strings.Contains(added[0].Title, "2 new free models") {
		t.Fatalf("free_models_added = %+v, want one '2 new free models' entry", added)
	}
	broken := byKind(items, store.NoteFreeModelBroken)
	if len(broken) != 1 || !strings.Contains(broken[0].Body, "broken-free") || strings.Contains(broken[0].Body, "tiny:free") {
		t.Fatalf("free_model_broken = %+v, want only broken-free", broken)
	}

	// Next refresh: one free model arrives, one leaves.
	mu.Lock()
	catalog = `{"data":[{"id":"gpt-6-luna"},{"id":"broken-free"},{"id":"fresh:free"}]}`
	mu.Unlock()
	h.server.refreshCatalogsOnce(ctx)
	items, _ = bell(t, h)
	if got := byKind(items, store.NoteFreeModelsAdded); len(got) != 2 || !strings.Contains(got[0].Title, "1 new free model on") {
		t.Fatalf("second refresh free_models_added = %+v", got)
	}
	gone := byKind(items, store.NoteFreeModelsRemoved)
	if len(gone) != 1 || !strings.Contains(gone[0].Body, "tiny:free") {
		t.Fatalf("free_models_removed = %+v, want tiny:free", gone)
	}
	// Let the second refresh's probe finish before the harness closes.
	settle := time.Now().Add(10 * time.Second)
	for !sweepIdle(h, acct) && time.Now().Before(settle) {
		time.Sleep(50 * time.Millisecond)
	}
}

func must(items []store.Notification, _ int) []store.Notification { return items }

// A failing refresh is one notification with a counter, not one per pass.
func TestCatalogRefreshFailureFoldsIntoOneNotification(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	ctx := context.Background()
	h.server.refreshCatalogsOnce(ctx)
	h.server.refreshCatalogsOnce(ctx)
	items, unread := bell(t, h)
	failed := byKind(items, store.NoteCatalogRefreshFailed)
	if len(failed) != 1 || failed[0].Count != 2 || failed[0].Severity != "warn" || unread != 1 {
		t.Fatalf("refresh failures = %+v (unread %d), want one warn entry with count 2", failed, unread)
	}
}

// A capped token hammering past its budget raises ONE notification.
func TestBudgetReachedNotifiesOnce(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	bucket := store.CountBucket(time.Now(), "monthly", time.Local)
	if _, err := h.db.Writer().ExecContext(ctx, `UPDATE client_tokens SET cap_tokens=10, cap_window='monthly' WHERE name='hermes'`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Writer().ExecContext(ctx,
		`INSERT INTO client_usage_counters (client, bucket, tokens, calls, updated_at) VALUES ('hermes', ?, 10, 1, '2026-01-01T00:00:00Z')`, bucket); err != nil {
		t.Fatal(err)
	}
	budgetNoted.Delete("hermes\x00" + bucket)
	for i := 0; i < 3; i++ {
		if code, _, _ := h.do("GET", "/v1/models", "tok-infer", ""); code != http.StatusTooManyRequests {
			t.Fatalf("capped request = %d, want 429", code)
		}
	}
	got := byKind(must(bell(t, h)), store.NoteClientBudgetReached)
	if len(got) != 1 || got[0].Count != 1 || !strings.Contains(got[0].Title, `"hermes"`) {
		t.Fatalf("budget notifications = %+v, want exactly one for hermes", got)
	}
}

func TestNotificationsReadAPI(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	for _, title := range []string{"one", "two"} {
		if err := h.db.Notify(ctx, store.NotificationInput{Kind: "test", Title: title}); err != nil {
			t.Fatal(err)
		}
	}
	items, unread := bell(t, h)
	if len(items) != 2 || unread != 2 {
		t.Fatalf("items=%d unread=%d", len(items), unread)
	}
	if code, _, body := h.do("POST", "/admin/notifications/read", "tok-admin", `{}`); code != http.StatusBadRequest {
		t.Fatalf("empty read = %d %s, want 400", code, body)
	}
	if code, _, _ := h.do("POST", "/admin/notifications/read", "tok-admin", `{"ids":[`+strconv.FormatInt(items[0].ID, 10)+`]}`); code != http.StatusNoContent {
		t.Fatalf("read one = %d", code)
	}
	if _, unread = bell(t, h); unread != 1 {
		t.Fatalf("unread after reading one = %d", unread)
	}
	if code, _, _ := h.do("POST", "/admin/notifications/read", "tok-admin", `{"all":true}`); code != http.StatusNoContent {
		t.Fatalf("read all = %d", code)
	}
	if _, unread = bell(t, h); unread != 0 {
		t.Fatalf("unread after read all = %d", unread)
	}
	if code, _, _ := h.do("GET", "/admin/notifications", "tok-infer", ""); code != http.StatusForbidden {
		t.Fatalf("an infer-only token read the bell: %d", code)
	}
}
