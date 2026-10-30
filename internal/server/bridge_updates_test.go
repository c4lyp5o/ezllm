package server

// The host updater's reports (bridges.go PollBridgeUpdates): a bridge card
// shows the latest update-status.json, and a new updated, blocked or
// rolled_back report rings the bell exactly once.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

func writeReport(t *testing.T, path string, u BridgeUpdate) {
	t.Helper()
	b, _ := json.Marshal(u)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func bridgeBell(t *testing.T, h *harness) []store.Notification {
	t.Helper()
	rows, _, err := h.db.ListNotifications(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestBridgeUpdatesRingTheBellOncePerReport(t *testing.T) {
	h := settingsHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	file := filepath.Join(t.TempDir(), "update-status.json")
	h.server.EnableBridge("claude-sub", "127.0.0.1:8790", bridgeSecret, file, nil)
	since := time.Date(2026, 10, 31, 2, 0, 0, 0, time.UTC)
	stamp := func(m int) string { return since.Add(time.Duration(m) * time.Minute).Format(time.RFC3339) }

	// Before the first report: nothing to show, nothing to ring.
	h.server.PollBridgeUpdates(since)
	if b, _ := h.server.bridge("claude-sub"); b.Update != nil || len(bridgeBell(t, h)) != 0 {
		t.Fatalf("no report yet: update %+v bell %d", b.Update, len(bridgeBell(t, h)))
	}

	// A report from before this process is shown but not announced.
	writeReport(t, file, BridgeUpdate{CheckedAt: stamp(-5), Result: "updated", Current: "2.1.308", Previous: "2.1.306", Candidate: "2.1.308"})
	h.server.PollBridgeUpdates(since)
	if b, _ := h.server.bridge("claude-sub"); b.Update == nil || b.Update.Current != "2.1.308" || len(bridgeBell(t, h)) != 0 {
		t.Fatalf("pre-start report: update %+v bell %d, want shown and silent", b.Update, len(bridgeBell(t, h)))
	}

	// A new blocked report rings once, however often it is read — also after
	// the operator has read it (Notify alone would add a fresh row then).
	writeReport(t, file, BridgeUpdate{CheckedAt: stamp(3), Result: "blocked", Current: "2.1.308", Candidate: "2.1.310", Detail: "flags the CLI no longer accepts: --max-turns"})
	h.server.PollBridgeUpdates(since)
	h.server.PollBridgeUpdates(since)
	if err := h.db.MarkNotificationsRead(context.Background(), nil, true); err != nil {
		t.Fatal(err)
	}
	h.server.PollBridgeUpdates(since)
	rows := bridgeBell(t, h)
	if len(rows) != 1 || rows[0].Count != 1 {
		t.Fatalf("blocked report: %d rows (first count %d), want exactly one event", len(rows), rows[0].Count)
	}
	n := rows[0]
	if n.Kind != store.NoteBridgeUpdateBlocked || n.Severity != "warn" || n.Title != "Claude Pro/Max CLI 2.1.310 held back" ||
		!strings.Contains(n.Body, "stays on 2.1.308") || !strings.Contains(n.Body, "--max-turns") {
		t.Fatalf("blocked event = %+v", n)
	}

	// Routine reports update the card and stay off the bell.
	writeReport(t, file, BridgeUpdate{CheckedAt: stamp(60), Result: "waiting", Current: "2.1.308", Candidate: "2.1.312", Detail: "2.1.312 soaking"})
	h.server.PollBridgeUpdates(since)
	if b, _ := h.server.bridge("claude-sub"); b.Update.Result != "waiting" || len(bridgeBell(t, h)) != 1 {
		t.Fatalf("waiting report: update %+v bell %d", b.Update, len(bridgeBell(t, h)))
	}

	// A rollback is an error-severity event naming where the bridge is now.
	writeReport(t, file, BridgeUpdate{CheckedAt: stamp(120), Result: "rolled_back", Current: "2.1.308", Previous: "2.1.312", Candidate: "2.1.312", Detail: "2.1.312 failed (live tool turn); back on 2.1.308"})
	h.server.PollBridgeUpdates(since)
	rows = bridgeBell(t, h)
	if len(rows) != 2 || rows[0].Kind != store.NoteBridgeRolledBack || rows[0].Severity != "error" || !strings.Contains(rows[0].Body, "back on 2.1.308") {
		t.Fatalf("rollback: %+v", rows)
	}

	// A missing or broken file keeps the last report.
	if err := os.WriteFile(file, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.server.PollBridgeUpdates(since)
	os.Remove(file)
	h.server.PollBridgeUpdates(since)
	if b, _ := h.server.bridge("claude-sub"); b.Update == nil || b.Update.Result != "rolled_back" {
		t.Fatalf("unreadable report replaced the last one: %+v", b.Update)
	}
}

func TestBridgeUpdateEventsHonourTheirToggles(t *testing.T) {
	h := settingsHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	file := filepath.Join(t.TempDir(), "update-status.json")
	h.server.EnableBridge("chatgpt-sub", "127.0.0.1:8791", bridgeSecret, file, nil)
	if _, err := h.server.settings.Patch(context.Background(), 0, map[string]json.RawMessage{"notify_bridge_updated": json.RawMessage(`false`)}); err != nil {
		t.Fatal(err)
	}
	since := time.Now().Add(-time.Minute)
	writeReport(t, file, BridgeUpdate{CheckedAt: time.Now().Format(time.RFC3339), Result: "updated", Current: "0.166.0", Previous: "0.163.0", Candidate: "0.166.0"})
	h.server.PollBridgeUpdates(since)
	if n := len(bridgeBell(t, h)); n != 0 {
		t.Fatalf("notify_bridge_updated=false still rang the bell (%d)", n)
	}
	if b, _ := h.server.bridge("chatgpt-sub"); b.Update == nil || b.Update.Current != "0.166.0" {
		t.Fatalf("a muted event must still update the card: %+v", b.Update)
	}
}

// Only a running bridge on the updater layout is read; the API carries the
// report, the sanitized diagnostics only its verdict and versions.
func TestBridgeUpdatesInTheAPIs(t *testing.T) {
	h := settingsHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	dir := t.TempDir()
	on, off := filepath.Join(dir, "on.json"), filepath.Join(dir, "off.json")
	report := BridgeUpdate{CheckedAt: "2026-10-31T03:12:40+08:00", Result: "blocked", Current: "0.163.0", Candidate: "0.166.0",
		Detail: "upstream said: quota exceeded for org-x"}
	writeReport(t, on, report)
	writeReport(t, off, report)
	h.server.EnableBridge("chatgpt-sub", "127.0.0.1:8791", bridgeSecret, on, nil)
	h.server.EnableBridge("antigravity-sub", "127.0.0.1:8792", bridgeSecret, "", nil)
	h.server.PollBridgeUpdates(time.Now())

	code, resp := jsonDo(t, h, "GET", "/admin/bridges", "tok-admin", nil)
	if code != http.StatusOK || !strings.Contains(resp, `"update":{"checked_at":"2026-10-31T03:12:40+08:00","result":"blocked","current":"0.163.0","candidate":"0.166.0","detail":"upstream said: quota exceeded for org-x"}`) {
		t.Fatalf("GET /admin/bridges = %d %s", code, resp)
	}
	if strings.Count(resp, `"update":`) != 1 {
		t.Fatalf("a bridge without an update file reported one: %s", resp)
	}
	code, _, body := h.do("GET", "/admin/settings/diagnostics", "tok-admin", "")
	if code != http.StatusOK || !strings.Contains(body, `"result":"blocked"`) || strings.Contains(body, "quota exceeded") {
		t.Fatalf("diagnostics = %d %s, want the verdict without the detail", code, body)
	}
}
