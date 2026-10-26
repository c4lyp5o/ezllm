package server

import (
	"net/http"
	"strings"
	"sync"

	"github.com/c4lyp5o/ezllm/internal/registration"
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
	token  string
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
// secret its account key must carry, and its boot status. Unknown ids are
// ignored (the registry lists only what the dashboard knows how to offer).
func (s *Server) EnableBridge(id, addr, token string, status map[string]any) {
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
		b.Status = status
	}
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
