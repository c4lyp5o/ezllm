package copilotbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// The bridge-check surface: what `ezllm bridge-check copilot` runs before a CLI
// release is allowed near production.
//
// Two tiers, like the other three bridges. The static tier costs nothing: it
// resolves the binary, reads its version, handshakes the headless channel and
// creates (then deletes) a session, which proves the CLI accepts work without
// running a model.
//
// It does NOT prove the login, and must not claim to: measured 2026-11-06, an
// EMPTY COPILOT_HOME passes `session.create` — the CLI only reads the credential
// when a prompt asks GitHub for something. The login is proven by the live tier,
// which is therefore what promotion requires. The live tier also proves the two
// paths a release actually breaks: a plain answer, and a client-tool batch
// followed by a turn that replays the result.
//
// The channel is the fragile part and the reason this exists at all:
// `--headless --stdio` is NOT in `copilot --help`, so a release can change it
// without any version number moving. `ping` reports the protocol version, which
// this package pins.

// QualifiedVersion is the CLI release this package's fixtures and live proof
// were taken on. A different version still serves; it is reported unqualified
// until a live proof records it.
const QualifiedVersion = "1.0.105"

// Preflight is what the static tier established.
type Preflight struct {
	Version         string
	ProtocolVersion int
}

// CLIVersion reads `copilot --version` ("GitHub Copilot CLI 1.0.105.") and
// returns the version token, or "unknown" when the binary cannot be asked.
func CLIVersion(bin string) string {
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return "unknown"
	}
	for _, f := range strings.Fields(string(out)) {
		if f[0] >= '0' && f[0] <= '9' {
			return strings.TrimSuffix(f, ".")
		}
	}
	return strings.TrimSpace(string(out))
}

// RunPreflight is the static tier. It returns the first thing that stops the
// release, wrapped so the caller can print it verbatim.
func RunPreflight(ctx context.Context, bin, home string) (Preflight, error) {
	var p Preflight
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return p, fmt.Errorf("binary does not resolve: %w", err)
	}
	p.Version = CLIVersion(resolved)
	if p.Version == "" || p.Version == "unknown" {
		return p, errors.New("the CLI did not report a version (`copilot --version`)")
	}
	r := &Runner{Bin: resolved, Home: home, Timeout: probeTimeout}
	// One child covers both: the handshake proves the undocumented channel is
	// still there and at our protocol, and a created session proves the login.
	if err := r.probeChannel(ctx); err != nil {
		return p, fmt.Errorf("the headless channel: %w", err)
	}
	p.ProtocolVersion = channelProtocolVersion
	return p, nil
}

// probeChannel handshakes the channel and creates (then deletes) a session
// without sending a prompt. Nothing billable happens, and it proves the CLI will
// accept a session — NOT that the login works (see the package comment above).
func (r *Runner) probeChannel(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*probeTimeout)
	defer cancel()
	return r.withChannel(ctx, func(c *rpcConn) error {
		if err := c.ping(); err != nil {
			return err
		}
		var created struct {
			SessionID string `json:"sessionId"`
		}
		if err := c.call("session.create", sessionConfig(ChannelTurn{Model: "auto"}), &created); err != nil {
			// A create refusal is still worth typing: a channel at the right
			// protocol that refuses every session is not a usable release.
			return classifyCreate(err)
		}
		if created.SessionID != "" {
			_ = c.call("session.delete", map[string]any{"sessionId": created.SessionID}, nil)
		}
		return nil
	})
}

// Live prompts. Small on purpose: the check judges shape, not prose.
const (
	livePlainPrompt = "Reply with exactly: pong"
	// Two tools, so a batch (not just a single call) is exercised — the shape a
	// tool-using client actually sends.
	liveToolPrompt = "Call the weather tool for Kuala Lumpur and the clock tool, both in this reply. Call nothing else."
)

func liveToolDefs() []toolDef {
	weather := toolDef{Type: "function"}
	weather.Function.Name = "weather"
	weather.Function.Description = "Report the current weather for a city."
	weather.Function.Parameters = []byte(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)

	clock := toolDef{Type: "function"}
	clock.Function.Name = "clock"
	clock.Function.Description = "Report the current local time."
	clock.Function.Parameters = []byte(`{"type":"object","properties":{"tz":{"type":"string"}}}`)

	return []toolDef{weather, clock}
}

// LivePlain drives one plain turn and judges that an answer came back with usage
// attached. The text is judged, never printed: a check must not paste a model's
// words into a build log.
func LivePlain(ctx context.Context, r *Runner, model string) error {
	if model == "" {
		model = "auto"
	}
	res, err := r.RunChannel(ctx, ChannelTurn{Model: model, Prompt: livePlainPrompt}, nil)
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToLower(res.Text), "pong") {
		return fmt.Errorf("the answer did not follow the prompt (%d chars back)", len(res.Text))
	}
	if res.Usage.CompletionTokens <= 0 && res.Usage.NanoAIU == 0 {
		return errors.New("the turn reported no usage at all")
	}
	return nil
}

// LiveTools drives the client-tool path, which is where this bridge broke twice
// before the channel: the batch must come back with the caller's names and the
// CLI's own ids, and a follow-up turn that replays the result must answer.
func LiveTools(ctx context.Context, r *Runner, model string) error {
	if model == "" {
		model = "auto"
	}
	first, err := r.RunChannel(ctx, ChannelTurn{Model: model, Prompt: liveToolPrompt, Tools: liveToolDefs()}, nil)
	if err != nil {
		// "Finished without an assistant message or a tool call" IS the empty
		// case, and naming it as a channel fault would send the reader hunting
		// the wrong thing.
		if strings.Contains(err.Error(), "without an assistant message") {
			return errors.New("the model asked for no tool and said nothing, so the batch path is unproven")
		}
		return fmt.Errorf("tool turn: %w", err)
	}
	if len(first.ToolCalls) == 0 {
		return errors.New("the model asked for no tool, so the batch path is unproven")
	}
	declared := map[string]bool{"weather": true, "clock": true}
	for _, c := range first.ToolCalls {
		if !declared[c.Name] {
			return fmt.Errorf("a tool the caller never declared came back: %q", c.Name)
		}
		if c.ID == "" {
			return fmt.Errorf("the %s call came back without an id", c.Name)
		}
		if strings.TrimSpace(c.Arguments) == "" {
			return fmt.Errorf("the %s call came back with no arguments", c.Name)
		}
	}
	// The caller's next request is a fresh session with the results replayed as
	// text; if that cannot answer, a tool loop would stall after one step.
	replay := ChannelTurn{
		Model:  model,
		Prompt: liveToolPrompt + "\n\nTool result for weather: {\"city\":\"Kuala Lumpur\",\"temp_c\":31}\nTool result for clock: {\"local_time\":\"09:41\"}\n\nAnswer in one short sentence using those results.",
	}
	second, err := r.RunChannel(ctx, replay, nil)
	if err != nil {
		return fmt.Errorf("the replay turn: %w", err)
	}
	if strings.TrimSpace(second.Text) == "" {
		return errors.New("the replay turn produced no answer")
	}
	return nil
}

// LiveSessionCache proves exact-history continuation and request usage deltas.
// Provider cache hits are not guaranteed for a tiny gate prompt, so the gate
// checks native session reuse rather than requiring a particular token discount.
func LiveSessionCache(ctx context.Context, r *Runner, model string) error {
	if !r.SessionCacheEnabled() {
		return errors.New("completed-session cache is not enabled")
	}
	if model == "" || model == "auto" {
		model = "gpt-6-luna"
	}
	messages := []map[string]any{{"role": "user", "content": livePlainPrompt}}
	send := func() (Result, error) {
		body, _ := json.Marshal(map[string]any{"model": model, "messages": messages})
		req, err := ParseRequest(body)
		if err != nil {
			return Result{}, err
		}
		return r.RunChannel(ctx, ChannelTurn{Model: req.Model, System: req.System, Prompt: req.Transcript, cacheHistory: canonicalCacheHistory(body)}, nil)
	}
	r.mu.Lock()
	before := r.stages["session_cache_hit"].count
	r.mu.Unlock()
	first, err := send()
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToLower(first.Text), "pong") || first.Usage.CompletionTokens <= 0 {
		return errors.New("initial cache turn failed its answer/usage proof")
	}
	messages = append(messages, map[string]any{"role": "assistant", "content": first.Text}, map[string]any{"role": "user", "content": livePlainPrompt})
	next, err := send()
	if err != nil {
		return err
	}
	r.mu.Lock()
	after := r.stages["session_cache_hit"].count
	r.mu.Unlock()
	if first.SessionID == "" || next.SessionID != first.SessionID || after != before+1 || !strings.Contains(strings.ToLower(next.Text), "pong") || next.Usage.CompletionTokens <= 0 {
		return errors.New("completed-session continuation failed its identity/answer/usage proof")
	}
	return nil
}
