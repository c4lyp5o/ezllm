package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/c4lyp5o/ezllm/internal/registration"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// Subscription bridges, as the dashboard sees them. main.go starts each bridge
// (cmd/ezllm/claudebridge.go, codexbridge.go, agybridge.go) and reports it here; the Add
// Provider dialog reads GET /admin/bridges to offer a ready-made preset with a
// live "running / off" state, and names `bridge` on the key add so the server
// fills in the shared secret itself — nobody has to cat a token file on the
// host. The token never leaves the process: it is not in any response.
// Runbooks: docs/claude-bridge-runbook.md, docs/codex-bridge-runbook.md,
// docs/agy-bridge-runbook.md.

// Bridge is one loopback subscription surface.
type Bridge struct {
	ID      string `json:"id"`       // claude-sub | chatgpt-sub | antigravity-sub
	Label   string `json:"label"`    // dashboard preset name
	Kind    string `json:"kind"`     // account kind that dials it
	BaseURL string `json:"base_url"` // where that account points
	// UsageConvention is what the account must declare for honest cost
	// (the Claude bridge emits the native Messages envelope).
	UsageConvention string `json:"usage_convention"`
	Enabled         bool   `json:"enabled"`
	Env             string `json:"env"`     // the variable that turns it on
	Runbook         string `json:"runbook"` // setup steps, repo-relative
	// Status is the bridge's own /status snapshot from boot (CLI version,
	// login, plan …). Empty when the bridge is off.
	Status map[string]any `json:"status,omitempty"`
	// Update is the host updater's latest report on this bridge's CLI
	// (scripts/bridge_update.py, update-status.json in the bridge root), read
	// once a minute. Nil when the bridge is not on the updater layout or the
	// updater has not run.
	Update *BridgeUpdate `json:"update,omitempty"`
	token  string
	// updateFile is where Update is read from; raised is the checked_at of
	// the last report already turned into a bell event (or seen at boot).
	updateFile string
	raised     string
}

// BridgeUpdate is one update-status.json: what the updater last did.
type BridgeUpdate struct {
	CheckedAt string `json:"checked_at"`
	Result    string `json:"result"` // updated | blocked | rolled_back | up_to_date | waiting | error
	Current   string `json:"current,omitempty"`
	Previous  string `json:"previous,omitempty"`
	Candidate string `json:"candidate,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// bridgeRegistry starts with every bridge off so the endpoint can always tell
// a new operator what exists and how to turn it on.
type bridgeRegistry struct {
	mu   sync.RWMutex
	list []Bridge
}

func newBridgeRegistry() *bridgeRegistry {
	return &bridgeRegistry{list: []Bridge{
		{
			ID: "claude-sub", Label: "Claude Pro/Max", Kind: "anthropic-compatible",
			BaseURL: "http://127.0.0.1:8790/v1", UsageConvention: "anthropic",
			Env: "EZLLM_CLAUDE_BRIDGE", Runbook: "docs/claude-bridge-runbook.md",
		},
		{
			ID: "chatgpt-sub", Label: "ChatGPT Plus/Pro", Kind: "openai-compatible",
			BaseURL: "http://127.0.0.1:8791/v1", UsageConvention: "auto",
			Env: "EZLLM_CODEX_BRIDGE", Runbook: "docs/codex-bridge-runbook.md",
		},
		{
			ID: "antigravity-sub", Label: "Google Antigravity", Kind: "openai-compatible",
			BaseURL: "http://127.0.0.1:8792/v1", UsageConvention: "auto",
			Env: "EZLLM_AGY_BRIDGE", Runbook: "docs/agy-bridge-runbook.md",
		},
	}}
}

// EnableBridge records a running bridge: its listener address, the shared
// secret its account key must carry, the updater's status file ("" when the
// bridge is not on the updater layout) and its boot status. Unknown ids are
// ignored (the registry lists only what the dashboard knows how to offer).
func (s *Server) EnableBridge(id, addr, token, updateFile string, status map[string]any) {
	s.bridges.mu.Lock()
	defer s.bridges.mu.Unlock()
	for i := range s.bridges.list {
		b := &s.bridges.list[i]
		if b.ID != id {
			continue
		}
		b.Enabled = true
		if addr != "" {
			b.BaseURL = "http://" + addr + "/v1"
		}
		b.token = token
		b.updateFile = updateFile
		b.Status = status
	}
}

// bridgeUpdateEvents are the updater results that ring the bell; the others
// (up_to_date, waiting, error) only show on the bridge card.
var bridgeUpdateEvents = map[string]struct{ kind, severity string }{
	"updated":     {store.NoteBridgeUpdated, "info"},
	"blocked":     {store.NoteBridgeUpdateBlocked, "warn"},
	"rolled_back": {store.NoteBridgeRolledBack, "error"},
}

// PollBridgeUpdates reads every running bridge's update-status.json into its
// Update and rings the bell for a new updated, blocked or rolled_back report.
// A report written before since is shown but not announced: the updater
// writes its outcome after the restart it causes, so a report older than this
// process was announced by the one before it (or never needed to be). A
// missing or unreadable file keeps the last report.
func (s *Server) PollBridgeUpdates(since time.Time) {
	var events []store.NotificationInput
	s.bridges.mu.Lock()
	for i := range s.bridges.list {
		b := &s.bridges.list[i]
		if !b.Enabled || b.updateFile == "" {
			continue
		}
		raw, err := os.ReadFile(b.updateFile)
		if err != nil {
			continue
		}
		var u BridgeUpdate
		if json.Unmarshal(raw, &u) != nil || u.Result == "" {
			continue
		}
		b.Update = &u
		ev, ok := bridgeUpdateEvents[u.Result]
		if !ok || u.CheckedAt == b.raised {
			continue
		}
		b.raised = u.CheckedAt
		if at, err := time.Parse(time.RFC3339, u.CheckedAt); err != nil || at.Before(since) {
			continue
		}
		events = append(events, bridgeUpdateNotice(b.ID, b.Label, ev.kind, ev.severity, u))
	}
	s.bridges.mu.Unlock()
	for _, n := range events {
		s.notify(n)
	}
}

func bridgeUpdateNotice(id, label, kind, severity string, u BridgeUpdate) store.NotificationInput {
	n := store.NotificationInput{Kind: kind, Severity: severity, DedupKey: "bridge:" + id + ":" + u.Candidate + ":" + u.Result,
		Data: map[string]any{"bridge": id, "result": u.Result, "current": u.Current, "previous": u.Previous, "candidate": u.Candidate}}
	switch u.Result {
	case "updated":
		n.Title = label + " CLI updated to " + u.Candidate
		n.Body = "Promoted from " + u.Previous + " and proved in the running gateway."
	case "blocked":
		n.Title = label + " CLI " + u.Candidate + " held back"
		n.Body = "The release failed the update gate; the bridge stays on " + u.Current + "."
	default:
		n.Title = label + " CLI " + u.Candidate + " rolled back"
		n.Body = "The release failed after promotion; the bridge is back on " + u.Current + "."
	}
	if u.Detail != "" {
		n.Body += " " + u.Detail
	}
	return n
}

// WatchBridgeUpdates polls the updater's reports now and then every interval
// until shutdown. Reports from before this process started are not announced.
func (s *Server) WatchBridgeUpdates(ctx context.Context, interval time.Duration) {
	since := s.started
	s.goBackground(func(bg context.Context) {
		ctx, stop := either(ctx, bg)
		defer stop()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			s.PollBridgeUpdates(since)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	})
}

func (s *Server) bridge(id string) (Bridge, bool) {
	s.bridges.mu.RLock()
	defer s.bridges.mu.RUnlock()
	for _, b := range s.bridges.list {
		if b.ID == id {
			return b, true
		}
	}
	return Bridge{}, false
}

// bridgeForURL names the bridge a base URL points at, for the failed-test hint.
func (s *Server) bridgeForURL(baseURL string) (Bridge, bool) {
	s.bridges.mu.RLock()
	defer s.bridges.mu.RUnlock()
	u := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	for _, b := range s.bridges.list {
		if u == b.BaseURL || u+"/v1" == b.BaseURL {
			return b, true
		}
	}
	return Bridge{}, false
}

func (s *Server) handleBridges(w http.ResponseWriter, r *http.Request) {
	s.bridges.mu.RLock()
	out := make([]Bridge, len(s.bridges.list))
	copy(out, s.bridges.list)
	s.bridges.mu.RUnlock()
	writeJSON(w, http.StatusOK, out)
}

// bridgeToken resolves `bridge` on a key add into the bridge's secret. It
// refuses unless the bridge is running AND the account points at it: the
// token is only ever sent to the loopback surface that minted it.
func (s *Server) bridgeToken(id, kind, baseURL string) (string, int, string) {
	b, ok := s.bridge(id)
	if !ok {
		return "", http.StatusBadRequest, "unknown bridge " + id
	}
	if !b.Enabled || b.token == "" {
		return "", http.StatusConflict, b.Label + " is not running: set " + b.Env + "=on, mount the CLI and login, and restart (" + b.Runbook + ")"
	}
	got, _ := s.bridgeForURL(baseURL)
	if kind != b.Kind || got.ID != b.ID {
		return "", http.StatusBadRequest, "this account does not point at the " + b.Label + " (" + b.Kind + " at " + b.BaseURL + ")"
	}
	return b.token, 0, ""
}

// explainBridgeFailure turns a bare connection error against a bridge that is
// not running into the step that fixes it. A running bridge's failures are its
// own (login expired, plan exhausted) and pass through untouched.
func (s *Server) explainBridgeFailure(baseURL string, test *registration.TestResult) {
	b, ok := s.bridgeForURL(baseURL)
	if !ok || b.Enabled {
		return
	}
	if test.Fail == nil {
		test.Fail = &registration.Failure{Step: "unknown", Message: "key test failed"}
	}
	test.Fail.Hint = b.Label + " is not running: set " + b.Env + "=on, mount the CLI and login, and restart (" + b.Runbook + ")."
}
