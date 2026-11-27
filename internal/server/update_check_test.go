package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

func TestNewerRelease(t *testing.T) {
	for _, tc := range []struct {
		candidate, current string
		want               bool
	}{
		{"2.3.0", "2.2.0", true}, {"v2.2.1", "2.2.0", true}, {"2.10.0", "2.9.9", true}, {"3.0.0", "2.99.0", true},
		{"2.2.0", "2.2.0", false}, {"2.1.9", "2.2.0", false}, // a development build ahead of the release
		{"2.3.0-rc1", "2.2.0", false}, {"latest", "2.2.0", false}, {"", "2.2.0", false}, {"2.3", "2.2.0", false},
	} {
		if got := newerRelease(tc.candidate, tc.current); got != tc.want {
			t.Errorf("newerRelease(%q, %q) = %v", tc.candidate, tc.current, got)
		}
	}
}

func updateBell(t *testing.T, h *harness) (out []store.Notification) {
	t.Helper()
	items, _, err := h.server.db.ListNotifications(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range items {
		if n.Kind == store.NoteUpdateAvailable {
			out = append(out, n)
		}
	}
	return out
}

// fakeReleases serves body at status and records the last request it saw.
func fakeReleases(t *testing.T, h *harness, status *atomic.Int64, body *atomic.Value) *atomic.Pointer[http.Request] {
	t.Helper()
	var last atomic.Pointer[http.Request]
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last.Store(r)
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	t.Cleanup(up.Close)
	h.server.release.url = up.URL
	return &last
}

func getUpdate(t *testing.T, h *harness) map[string]any {
	t.Helper()
	code, _, raw := h.do("GET", "/admin/update", "tok-admin", "")
	var out map[string]any
	if code != http.StatusOK || json.Unmarshal([]byte(raw), &out) != nil {
		t.Fatalf("GET /admin/update = %d %s", code, raw)
	}
	return out
}

func TestUpdateCheckTellsOncePerVersion(t *testing.T) {
	h := settingsHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	var status atomic.Int64
	var body atomic.Value
	status.Store(200)
	body.Store(`{"tag_name":"v99.1.0","html_url":"https://evil.example/x"}`)
	last := fakeReleases(t, h, &status, &body)

	if u := getUpdate(t, h); u["available"] != false || u["latest"] != "" || u["current"] != Version || u["enabled"] != true {
		t.Fatalf("before any check: %v", u)
	}
	if !h.server.checkForUpdate(context.Background()) {
		t.Fatal("check failed")
	}
	u := getUpdate(t, h)
	if u["available"] != true || u["latest"] != "99.1.0" || u["checked_at"] == "" || u["url"] != releasePageURL+"99.1.0" {
		t.Fatalf("update = %v (the link must be ours, not the response's)", u)
	}
	// Nothing about this instance goes out.
	r := last.Load()
	if r.Header.Get("User-Agent") != "ezllm" || r.Header.Get("Authorization") != "" || r.URL.RawQuery != "" || strings.Contains(r.URL.String(), Version) {
		t.Errorf("request = %s %v", r.URL, r.Header)
	}
	if n := updateBell(t, h); len(n) != 1 || !strings.Contains(n[0].Title, "v99.1.0") || n[0].Severity != "info" {
		t.Fatalf("bell = %+v", n)
	}
	// Read it, then check again (a restart does): still one line, not counted up.
	if code, _, _ := h.do("POST", "/admin/notifications/read", "tok-admin", `{"all":true}`); code != http.StatusNoContent {
		t.Fatalf("mark read = %d", code)
	}
	h.server.checkForUpdate(context.Background())
	if n := updateBell(t, h); len(n) != 1 || n[0].Count != 1 {
		t.Fatalf("the same version was announced again: %+v", n)
	}
	body.Store(`{"tag_name":"v99.2.0"}`)
	h.server.checkForUpdate(context.Background())
	if n := updateBell(t, h); len(n) != 2 {
		t.Fatalf("a newer version must be announced: %+v", n)
	}
}

func TestUpdateCheckIgnoresUnusableAnswersAndKeepsTheLast(t *testing.T) {
	h := settingsHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	var status atomic.Int64
	var body atomic.Value
	status.Store(200)
	body.Store(`{"tag_name":"v` + Version + `"}`)
	fakeReleases(t, h, &status, &body)
	if !h.server.checkForUpdate(context.Background()) || getUpdate(t, h)["available"] != false {
		t.Fatal("the running release is not an update")
	}
	for _, bad := range []struct {
		status int64
		body   string
	}{{403, `{"message":"API rate limit exceeded"}`}, {200, `not json`}, {200, `{"tag_name":"v99.0.0","prerelease":true}`},
		{200, `{"tag_name":"v99.0.0","draft":true}`}, {200, `{"tag_name":"<img src=x>"}`}} {
		status.Store(bad.status)
		body.Store(bad.body)
		if h.server.checkForUpdate(context.Background()) {
			t.Errorf("%d %s counted as a successful check", bad.status, bad.body)
		}
	}
	if u := getUpdate(t, h); u["latest"] != Version {
		t.Errorf("a failed check replaced the last answer: %v", u)
	}
	if n := updateBell(t, h); len(n) != 0 {
		t.Errorf("bell = %+v", n)
	}
}

func TestUpdateCheckSwitches(t *testing.T) {
	h := settingsHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	var status atomic.Int64
	var body atomic.Value
	status.Store(200)
	body.Store(`{"tag_name":"v99.1.0"}`)
	fakeReleases(t, h, &status, &body)
	patch := func(rev int64, key, value string) {
		t.Helper()
		if _, err := h.server.settings.Patch(context.Background(), rev, map[string]json.RawMessage{key: json.RawMessage(value)}); err != nil {
			t.Fatal(err)
		}
	}
	patch(0, "notify_update_available", "false")
	h.server.checkForUpdate(context.Background())
	if n := updateBell(t, h); len(n) != 0 || getUpdate(t, h)["available"] != true {
		t.Fatalf("muting the bell must not hide the pill: bell %+v", n)
	}
	patch(1, "notify_update_available", "true")
	patch(2, "update_check", "false")
	h.server.checkForUpdate(context.Background()) // a check already running when it was switched off
	if u := getUpdate(t, h); u["enabled"] != false || u["available"] != false || u["latest"] != "" {
		t.Errorf("switched off: %v", u)
	}
	if n := updateBell(t, h); len(n) != 0 {
		t.Errorf("switched off but announced: %+v", n)
	}
	for _, token := range []string{"", "tok-infer"} {
		if code, _, _ := h.do("GET", "/admin/update", token, ""); code != 401 && code != 403 {
			t.Errorf("token %q got %d", token, code)
		}
	}
}

// Off means no request; switching it on asks straight away, not a day later.
func TestUpdateCheckSchedule(t *testing.T) {
	h := settingsHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"tag_name":"v99.1.0"}`))
	}))
	t.Cleanup(up.Close)
	h.server.release.url = up.URL
	if code, _, raw := h.do("PATCH", "/admin/settings", "tok-admin", `{"revision":0,"values":{"update_check":false}}`); code != 200 {
		t.Fatalf("patch = %d %s", code, raw)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.server.StartSettingsJobs(ctx)
	time.Sleep(50 * time.Millisecond)
	if hits.Load() != 0 {
		t.Fatal("asked while switched off")
	}
	if code, _, raw := h.do("PATCH", "/admin/settings", "tok-admin", `{"revision":1,"values":{"update_check":true}}`); code != 200 {
		t.Fatalf("patch = %d %s", code, raw)
	}
	for end := time.Now().Add(5 * time.Second); hits.Load() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("switching the check on did not ask")
		}
	}
}
