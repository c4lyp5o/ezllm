package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/settings"
	"github.com/c4lyp5o/ezllm/internal/store"
)

func settingsHarness(t *testing.T, upstream http.HandlerFunc) *harness {
	t.Helper()
	return newHarness(t, upstream, func(o *Options) {
		m, err := settings.New(context.Background(), o.DB, settings.Defaults(), map[string]string{"price_refresh": "environment"})
		if err != nil {
			t.Fatal(err)
		}
		o.Settings = m
		o.DB.SetOperatorSettings(m)
	})
}
func TestSettingsAPIAuthValidationConflictAndRedaction(t *testing.T) {
	h := settingsHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	for _, path := range []string{"/admin/settings", "/admin/settings/sessions", "/admin/settings/diagnostics", "/admin/backups"} {
		for _, token := range []string{"", "tok-infer"} {
			code, _, body := h.do("GET", path, token, "")
			want := 401
			if token != "" {
				want = 403
			}
			if code != want {
				t.Fatal(path, code, body)
			}
		}
	}
	code, _, body := h.do("GET", "/admin/settings", "tok-admin", "")
	if code != 200 {
		t.Fatal(code, body)
	}
	for _, bad := range []string{`{"revision":0,"values":{"retries":1},"extra":true}`, `{"revision":0,"values":{"retries":1}} {}`, `{"values":{"retries":1}}`, `{"revision":0,"values":{"retries":1,"price_refresh":false}}`, `{"revision":0,"values":{"retries":99}}`} {
		code, _, body = h.do("PATCH", "/admin/settings", "tok-admin", bad)
		if code != 400 {
			t.Fatal(code, body, bad)
		}
	}
	code, _, body = h.do("PATCH", "/admin/settings", "tok-admin", `{"revision":0,"values":{"retries":0}}`)
	if code != 200 {
		t.Fatal(code, body)
	}
	code, _, body = h.do("PATCH", "/admin/settings", "tok-admin", `{"revision":0,"values":{"retries":1}}`)
	if code != 409 {
		t.Fatal(code, body)
	}
	code, _, body = h.do("GET", "/admin/settings/diagnostics?download=true", "tok-admin", "")
	if code != 200 {
		t.Fatal(code, body)
	}
	for _, secret := range []string{h.db.Path(), "sk-upstream-key-123456", "tok-admin", "tok-infer", "key_plain", "token_hash"} {
		if strings.Contains(body, secret) {
			t.Fatalf("diagnostics leaked %q", secret)
		}
	}
	code, _, body = h.do("GET", "/admin/export", "tok-admin", "")
	if code != 200 || !strings.Contains(body, `"settings":{"retries":0}`) {
		t.Fatal(code, body)
	}
	code, _, body = h.do("POST", "/admin/backups", "tok-admin", "")
	if code != 409 {
		t.Fatal(code, body)
	}
}
func TestSettingsCookieWritesRequireSameOrigin(t *testing.T) {
	h := settingsHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	if err := h.db.CompleteDashboardSetup(context.Background(), "test-password-long-enough"); err != nil {
		t.Fatal(err)
	}
	token, err := h.db.DashboardLogin(context.Background(), "test-password-long-enough")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/admin/settings", "/admin/settings/sessions/revoke-all", "/admin/backups"} {
		method := "POST"
		if path == "/admin/settings" {
			method = "PATCH"
		}
		r := httptest.NewRequest(method, "http://gateway.test"+path, strings.NewReader(`{"revision":0,"values":{"retries":0}}`))
		r.AddCookie(&http.Cookie{Name: "ezllm_session", Value: token})
		r.Header.Set("Origin", "https://attacker.test")
		w := httptest.NewRecorder()
		h.handler.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
}
func TestSettingsRetriesSnapshotDuringLiveUpdate(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	h := settingsHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"unavailable"}}`))
	})
	// One admitted retry is retained even after the new policy disables retries.
	_, err := h.server.settings.Patch(context.Background(), 0, map[string]json.RawMessage{"retries": json.RawMessage(`1`), "cooldown_strikes": json.RawMessage(`1`)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		code, _, _ := h.do("POST", "/v1/chat/completions", "tok-infer", `{"model":"up/gpt-6-luna","messages":[]}`)
		done <- code
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request never reached upstream")
	}
	code, _, body := h.do("PATCH", "/admin/settings", "tok-admin", `{"revision":1,"values":{"retries":0}}`)
	if code != 200 {
		t.Fatal(code, body)
	}
	close(release)
	select {
	case code := <-done:
		if code != 503 {
			t.Fatal(code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request did not finish")
	}
	if calls.Load() != 2 {
		t.Fatalf("active request used changed policy: %d calls", calls.Load())
	}
	// The previous 503 may have parked the hop. Its recorded cooldown must not
	// be shortened by an unrelated policy update.
	before := h.server.dispatcher.Cooldowns()
	if len(before) != 1 {
		t.Fatal(before)
	}
	code, _, body = h.do("PATCH", "/admin/settings", "tok-admin", `{"revision":2,"values":{"cooldown_5xx":"1s"}}`)
	if code != 200 {
		t.Fatal(code, body)
	}
	after := h.server.dispatcher.Cooldowns()
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatal("existing cooldown changed", before, after)
	}
}
func TestSettingsNotificationPreferencesPreserveHistory(t *testing.T) {
	h := settingsHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	note := store.NotificationInput{Kind: store.NotePriceRefreshFailed, Title: "refresh failed"}
	if err := h.db.Notify(ctx, note); err != nil {
		t.Fatal(err)
	}
	if _, err := h.server.settings.Patch(ctx, 0, map[string]json.RawMessage{"notify_price_refresh_failed": json.RawMessage(`false`)}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.Notify(ctx, note); err != nil {
		t.Fatal(err)
	}
	rows, _, err := h.db.ListNotifications(ctx, 30)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
}

func TestSettingsJobsDoNotOverlapAndStop(t *testing.T) {
	h := settingsHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	h.server.runJob("catalog", func(ctx context.Context) bool {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		close(done)
		return true
	})
	<-started
	h.server.runJob("catalog", func(context.Context) bool { t.Error("overlapping scheduled job started"); return true })
	if !h.server.jobStatus()["catalog"].Running {
		t.Fatal("active job not visible")
	}
	close(release)
	<-done
	if !h.server.StopBackground(3 * time.Second) {
		t.Fatal("job did not finish")
	}
	state := h.server.jobStatus()["catalog"]
	if state.Running || state.LastSuccess == "" {
		t.Fatal(state)
	}
	persisted, err := h.db.JobHealth(context.Background())
	if err != nil || persisted["catalog"].LastSuccess == "" {
		t.Fatal(persisted, err)
	}
	h.server.runJob("prices", func(context.Context) bool { t.Error("job started after shutdown"); return true })
	if h.server.jobStatus()["prices"].Running {
		t.Fatal("rejected job left running flag")
	}
}

func TestSettingsDailyScheduleDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		now  string
		hour int
		want string
	}{
		{"2026-03-08T06:30:00Z", 2, "2026-03-08T07:00:00Z"}, // skipped 02:00 -> 03:00
		{"2026-11-01T04:30:00Z", 1, "2026-11-01T05:00:00Z"}, // first 01:00
		{"2026-11-01T05:30:00Z", 1, "2026-11-02T06:00:00Z"}, // no second run on repeated hour
	} {
		now, _ := time.Parse(time.RFC3339, tt.now)
		want, _ := time.Parse(time.RFC3339, tt.want)
		if got := nextDailyAt(now, tt.hour, loc); !got.Equal(want) {
			t.Errorf("%s hour %d: got %s, want %s", tt.now, tt.hour, got, want)
		}
	}
}

func TestSettingsDailyScheduleMidnightAndEuropeanDST(t *testing.T) {
	for _, tt := range []struct {
		zone, now, want string
		hour            int
	}{
		{"America/Santiago", "2026-09-06T03:30:00Z", "2026-09-06T04:00:00Z", 0},
		{"Europe/Berlin", "2026-10-25T00:30:00Z", "2026-10-26T01:00:00Z", 2},
	} {
		loc, err := time.LoadLocation(tt.zone)
		if err != nil {
			t.Fatal(err)
		}
		now, _ := time.Parse(time.RFC3339, tt.now)
		want, _ := time.Parse(time.RFC3339, tt.want)
		if got := nextDailyAt(now, tt.hour, loc); !got.Equal(want) {
			t.Errorf("%s: got %s, want %s", tt.zone, got, want)
		}
	}
}
