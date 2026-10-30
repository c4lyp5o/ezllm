package server

// External push channels (push.go): the API never returns a secret, the
// worker delivers new bell events that meet a channel's minimum severity, a
// folded repeat waits a day, and a failure becomes the channel's status and
// one warn line — never a bell event.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/notify"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// hookRecorder is a webhook endpoint that keeps what it was sent.
type hookRecorder struct {
	mu     sync.Mutex
	got    []notify.Message
	auth   []string
	status int
}

func newHookRecorder(t *testing.T) (*hookRecorder, *httptest.Server) {
	rec := &hookRecorder{status: http.StatusNoContent}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m notify.Message
		_ = json.Unmarshal(b, &m)
		rec.mu.Lock()
		rec.got, rec.auth = append(rec.got, m), append(rec.auth, r.Header.Get("Authorization"))
		status := rec.status
		rec.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func (r *hookRecorder) titles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []string{}
	for _, m := range r.got {
		out = append(out, m.Title)
	}
	return out
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func fastPushBackoff(t *testing.T) {
	old := notify.Backoff
	notify.Backoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { notify.Backoff = old })
}

func addWebhook(t *testing.T, h *harness, url, min string) int64 {
	t.Helper()
	code, resp := jsonDo(t, h, "POST", "/admin/notification-channels", "tok-admin", map[string]any{
		"kind": "webhook", "name": "ops hook", "min_severity": min, "secrets": map[string]string{"url": url, "bearer": "bearer-s3cret"}})
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, resp)
	}
	var v struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal([]byte(resp), &v)
	return v.ID
}

func TestNotificationChannelsAPINeverReturnsSecrets(t *testing.T) {
	h := settingsHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	id := addWebhook(t, h, "https://hooks.example.com/in?key=url-s3cret", "warn")

	code, resp := jsonDo(t, h, "GET", "/admin/notification-channels", "tok-admin", nil)
	if code != http.StatusOK || strings.Contains(resp, "s3cret") {
		t.Fatalf("GET = %d, carries a secret: %s", code, resp)
	}
	for _, want := range []string{`"target":"https://hooks.example.com"`, `"secrets_set":["bearer","url"]`, `"max":10`, `"kinds":{`} {
		if !strings.Contains(resp, want) {
			t.Fatalf("GET lacks %s: %s", want, resp)
		}
	}

	// An edit keeps absent secrets, clears one set to "", and resets nothing else.
	path := "/admin/notification-channels/" + itoa64(id)
	code, resp = jsonDo(t, h, "PATCH", path, "tok-admin", map[string]any{"name": "ops hook 2", "min_severity": "error", "secrets": map[string]string{"bearer": ""}})
	if code != http.StatusOK || strings.Contains(resp, "s3cret") || !strings.Contains(resp, `"secrets_set":["url"]`) || !strings.Contains(resp, `"min_severity":"error"`) {
		t.Fatalf("PATCH = %d %s", code, resp)
	}
	c, _ := h.db.NotificationChannel(context.Background(), id)
	if c.Secrets()["url"] != "https://hooks.example.com/in?key=url-s3cret" {
		t.Fatal("an edit without the url lost the stored url")
	}

	for _, bad := range []struct {
		body map[string]any
		says string
	}{
		{map[string]any{"kind": "ntfy"}, "kind cannot change"},
		{map[string]any{"name": ""}, "name must be"},
		{map[string]any{"min_severity": "fatal"}, "min_severity must be"},
		{map[string]any{"secrets": map[string]string{"url": ""}}, "webhook needs url"},
	} {
		if code, resp := jsonDo(t, h, "PATCH", path, "tok-admin", bad.body); code != http.StatusBadRequest || !strings.Contains(resp, bad.says) {
			t.Errorf("PATCH %v = %d %s, want 400 %q", bad.body, code, resp, bad.says)
		}
	}
	if code, resp := jsonDo(t, h, "POST", "/admin/notification-channels", "tok-admin", map[string]any{"kind": "telegram", "name": "t",
		"config": map[string]string{"chat_id": "42"}, "secrets": map[string]string{"bot_token": "not-a-token"}}); code != http.StatusBadRequest || strings.Contains(resp, "not-a-token") {
		t.Fatalf("bad telegram token = %d %s, want 400 without echoing it", code, resp)
	}
	if code, _ := jsonDo(t, h, "GET", "/admin/notification-channels", "tok-infer", nil); code != http.StatusForbidden && code != http.StatusUnauthorized {
		t.Fatalf("an inference token listed the channels: %d", code)
	}
	if code, _ := jsonDo(t, h, "DELETE", path, "tok-admin", nil); code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", code)
	}
	if code, _ := jsonDo(t, h, "DELETE", path, "tok-admin", nil); code != http.StatusNotFound {
		t.Fatalf("second DELETE = %d", code)
	}
}

func TestPushDeliversBellEventsByMinimumSeverity(t *testing.T) {
	h := settingsHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	rec, hook := newHookRecorder(t)
	id := addWebhook(t, h, hook.URL, "warn")
	h.server.StartPush(context.Background())

	h.server.notify(store.NotificationInput{Kind: store.NoteFreeModelsAdded, Severity: "info", Title: "info stays on the bell"})
	h.server.notify(store.NotificationInput{Kind: store.NoteBridgeUpdateBlocked, Severity: "warn", Title: "Claude Pro/Max CLI 2.1.310 held back", Body: "stays on 2.1.308"})
	waitFor(t, "a delivery", func() bool { return len(rec.titles()) >= 1 })
	time.Sleep(50 * time.Millisecond)
	if got := rec.titles(); len(got) != 1 || got[0] != "Claude Pro/Max CLI 2.1.310 held back" {
		t.Fatalf("delivered %v, want only the warn event (info is below the channel's minimum)", got)
	}
	rec.mu.Lock()
	m, auth := rec.got[0], rec.auth[0]
	rec.mu.Unlock()
	if m.Kind != store.NoteBridgeUpdateBlocked || m.Severity != "warn" || m.Body != "stays on 2.1.308" || auth != "Bearer bearer-s3cret" {
		t.Fatalf("payload %+v auth %q", m, auth)
	}
	waitFor(t, "the delivery status", func() bool {
		c, _ := h.db.NotificationChannel(context.Background(), id)
		return c.LastStatus == "ok" && c.LastSentAt != nil
	})
}

// A recurring event folds into one unread row; its push repeats once a day.
func TestPushFoldedRepeatsWaitADay(t *testing.T) {
	h := settingsHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	rec, hook := newHookRecorder(t)
	addWebhook(t, h, hook.URL, "warn")
	h.server.StartPush(context.Background())
	ev := store.NotificationInput{Kind: store.NoteCatalogRefreshFailed, Severity: "warn", Title: "catalog refresh failed", DedupKey: "catalog:7"}

	h.server.notify(ev)
	h.server.notify(ev)
	waitFor(t, "the first push", func() bool { return len(rec.titles()) >= 1 })
	time.Sleep(50 * time.Millisecond)
	if n := len(rec.titles()); n != 1 {
		t.Fatalf("a folded repeat pushed again within a day: %d pushes", n)
	}
	h.server.push.mu.Lock()
	h.server.push.last["catalog:7"] = time.Now().Add(-25 * time.Hour)
	h.server.push.mu.Unlock()
	h.server.notify(ev)
	waitFor(t, "the next day's push", func() bool { return len(rec.titles()) >= 2 })
	if n := len(rec.titles()); n != 2 {
		t.Fatalf("%d pushes, want 2", n)
	}
}

func TestPushFailureIsChannelStatusAndOneWarnLine(t *testing.T) {
	fastPushBackoff(t)
	for _, c := range []struct {
		level slog.Level
		want  bool
	}{{slog.LevelWarn, true}, {slog.LevelError, false}} {
		h, sink := harnessWithLog(t, c.level, func(w http.ResponseWriter, r *http.Request) {})
		rec, hook := newHookRecorder(t)
		rec.status = http.StatusBadGateway
		id := addWebhook(t, h, hook.URL+"/in?key=url-s3cret", "warn")
		h.server.StartPush(context.Background())
		h.server.notify(store.NotificationInput{Kind: "backup_failed", Severity: "error", Title: "Backup failed"})

		waitFor(t, "the failed status", func() bool {
			ch, _ := h.db.NotificationChannel(context.Background(), id)
			return ch.LastStatus == "failed"
		})
		ch, _ := h.db.NotificationChannel(context.Background(), id)
		if ch.LastError != "the channel answered 502" || len(rec.titles()) != notify.Attempts {
			t.Fatalf("status %q after %d attempts", ch.LastError, len(rec.titles()))
		}
		rows, _, _ := h.db.ListNotifications(context.Background(), 10)
		if len(rows) != 1 {
			t.Fatalf("a failed push raised its own bell event: %d rows", len(rows))
		}
		log := sink.String()
		if strings.Contains(log, "notification push failed") != c.want {
			t.Fatalf("level %v: push failure logged=%v, want %v: %s", c.level, !c.want, c.want, log)
		}
		if strings.Contains(log, "s3cret") {
			t.Fatalf("the log carries a secret: %s", log)
		}
	}
}

func TestNotificationChannelTest(t *testing.T) {
	h := settingsHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	rec, hook := newHookRecorder(t)

	// A draft is sent to without being saved.
	code, resp := jsonDo(t, h, "POST", "/admin/notification-channels/test", "tok-admin", map[string]any{
		"kind": "webhook", "name": "draft", "secrets": map[string]string{"url": hook.URL}})
	if code != http.StatusOK || resp != "{\"ok\":true}\n" || len(rec.titles()) != 1 || rec.titles()[0] != "ezllm test notification" {
		t.Fatalf("draft test = %d %q, delivered %v", code, resp, rec.titles())
	}
	if list, _ := h.db.ListNotificationChannels(context.Background()); len(list) != 0 {
		t.Fatal("testing a draft saved it")
	}

	// A saved channel tested as stored records its status; with edits it does not.
	id := addWebhook(t, h, hook.URL, "warn")
	if code, resp = jsonDo(t, h, "POST", "/admin/notification-channels/test", "tok-admin", map[string]any{"id": id}); code != http.StatusOK || !strings.Contains(resp, `"ok":true`) {
		t.Fatalf("saved test = %d %s", code, resp)
	}
	c, _ := h.db.NotificationChannel(context.Background(), id)
	if c.LastStatus != "ok" {
		t.Fatalf("a saved channel's test left status %q", c.LastStatus)
	}
	rec.mu.Lock()
	rec.status = http.StatusUnauthorized
	rec.mu.Unlock()
	code, resp = jsonDo(t, h, "POST", "/admin/notification-channels/test", "tok-admin", map[string]any{"id": id, "secrets": map[string]string{"bearer": "new"}})
	if code != http.StatusOK || !strings.Contains(resp, `"ok":false`) || !strings.Contains(resp, "refused the message (401)") {
		t.Fatalf("edited test = %d %s", code, resp)
	}
	if c, _ = h.db.NotificationChannel(context.Background(), id); c.LastStatus != "ok" {
		t.Fatalf("a test with unsaved edits overwrote the saved status: %q", c.LastStatus)
	}
}

func itoa64(n int64) string { b, _ := json.Marshal(n); return string(b) }
