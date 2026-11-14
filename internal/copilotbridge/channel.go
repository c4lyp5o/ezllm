package copilotbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// The channel: `copilot --headless --stdio`, the JSON-RPC server the CLI's own
// bundled SDK drives. Every turn runs here because it has what `-p` lacks: a real
// system message, caller-defined tools the model calls natively, and no CLI
// scaffolding on the bill (~14k prompt tokens per turn on `-p`).
//
// By default one child serves one HTTP request. The opt-in pool reuses a child
// only after a fresh session completes cleanup. With tools, each turn stops at the first batch of
// calls. The caller runs them and sends the results on its next request, which is
// a fresh session with the history replayed as text. Holding the session open
// between HTTP requests would keep one premium request per loop, but on AI-credit
// billing it measured only ~20% cheaper and it brings parked children, slots and
// failover edge cases (design §10).
//
// --headless is not listed in --help, so a CLI update could change it. Every
// turn checks the protocol version before anything billable happens. A mismatch
// is a typed refusal: plain chat falls back to `-p`, and a request with tools is
// refused so the router fails over at no cost (bridge.go chat).

// channelProtocolVersion is the channel version this file was measured against
// (CLI 1.0.105, 2026-11-05).
const channelProtocolVersion = 3

const (
	maxRPCMessage  = 64 << 20
	maxErrorDetail = 300
	// After the batch is captured the turn is aborted, and the usage checkpoint
	// the ledger needs arrives just before session.idle.
	drainTimeout  = 15 * time.Second
	deleteTimeout = 5 * time.Second
	probeTimeout  = 30 * time.Second
)

// toolNote always opens the system message: it replaces the CLI's own, so it is
// the only thing telling the model how to read the flattened history.
const toolNote = "Call the functions provided to you as tools when they help; they are your only tools. " +
	"The conversation below is a transcript. A \"Called\" line and a \"Tool result for\" block record a call that already happened: use its result and do not repeat the call unless you need fresher data. " +
	"When you need several functions, call them together."

// plainNote opens the system message of a turn without tools. Saying so matters:
// a history full of tool calls otherwise invites the model to attempt one, and an
// attempt with nothing to call ends the turn empty (measured on emulation).
const plainNote = "You have no tools in this conversation; answer from what you know and what is below. " +
	"The conversation below is a transcript. A \"Called\" line and a \"Tool result for\" block record a call that already happened."

// ChannelTurn is one chat turn. Tools may be empty.
type ChannelTurn struct {
	Model       string
	System      string
	Prompt      string // the transcript, without the system text
	Tools       []toolDef
	Choice      toolChoice
	Attachments []ImageAttachment
	// OnTools synchronously delivers a complete batch before accounting cleanup.
	OnTools      func(Result) error
	cacheHistory []json.RawMessage
}

// errChannelClosed means the child's stdout ended before the turn did.
var errChannelClosed = errors.New("copilot-bridge: the CLI closed the channel")

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

// rpcConn speaks LSP-framed JSON-RPC. It is single-threaded on purpose: a call
// reads until its own response arrives and queues the events it passes, so there
// is no reader goroutine to leak or race. Blocking reads end when closeRead runs.
type rpcConn struct {
	w                   io.Writer
	r                   *bufio.Reader
	closeRead           func()
	observe             func(string, time.Duration)
	nextID              int
	queue               []cliEvent
	strict              bool
	session             string
	retired             map[string]bool
	pending             []rpcMessage
	tainted             bool
	reusable            bool
	idleSeen, usageSeen bool
	cacheEnabled        bool
	completed           *completedSession
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
	Params struct {
		SessionID string          `json:"sessionId"`
		Event     json.RawMessage `json:"event"`
	} `json:"params"`
}

func (c *rpcConn) write(v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return errChannelClosed
	}
	if _, err := c.w.Write(body); err != nil {
		return errChannelClosed
	}
	return nil
}

func (c *rpcConn) read() (rpcMessage, error) {
	var msg rpcMessage
	size := -1
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return msg, errChannelClosed
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(k, "Content-Length") {
			if size, err = strconv.Atoi(strings.TrimSpace(v)); err != nil {
				return msg, fmt.Errorf("copilot-bridge: bad frame length %q", v)
			}
		}
	}
	if size < 0 || size > maxRPCMessage {
		return msg, fmt.Errorf("copilot-bridge: bad frame length %d", size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return msg, errChannelClosed
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return msg, fmt.Errorf("copilot-bridge: malformed channel message: %w", err)
	}
	return msg, nil
}

// dispatch handles a message that is not the response being waited for. The
// session never asks the host for anything (no permission or input handler is
// registered), but an unanswered server request would hang the turn.
func (c *rpcConn) dispatch(msg rpcMessage) error {
	switch {
	case msg.Method == "session.event":
		if c.strict {
			id := msg.Params.SessionID
			if id == "" {
				c.reusable = false
				return errors.New("copilot-bridge: session event has no session id")
			}
			if c.retired[id] {
				return nil
			}
			if c.session == "" {
				c.pending = append(c.pending, msg)
				if len(c.pending) > 256 {
					return errors.New("copilot-bridge: too many pending session events")
				}
				return nil
			}
			if id != c.session {
				c.reusable = false
				return errors.New("copilot-bridge: unexpected session event")
			}
		}
		// Metadata/user events can carry multimodal content arrays. Decode only
		// events the turn consumes; never coerce an image echo into text.
		var kind struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(msg.Params.Event, &kind); err != nil {
			return errors.New("copilot-bridge: malformed session event")
		}
		switch kind.Type {
		case "assistant.message_delta", "assistant.message", "session.auto_mode_resolved", "session.usage_checkpoint", "session.error", "session.idle":
		default:
			return nil
		}
		var ev cliEvent
		if err := json.Unmarshal(msg.Params.Event, &ev); err != nil {
			if c.strict {
				return errors.New("copilot-bridge: malformed session event")
			}
		} else {
			c.queue = append(c.queue, ev)
			if len(c.queue) > 4096 {
				return errors.New("copilot-bridge: session event queue exceeded limit")
			}
		}
	case msg.Method != "" && len(msg.ID) > 0:
		return c.write(map[string]any{"jsonrpc": "2.0", "id": msg.ID,
			"error": map[string]any{"code": -32601, "message": "not supported by this host"}})
	}
	return nil
}

func (c *rpcConn) call(method string, params, out any) error {
	start := time.Now()
	if c.observe != nil {
		defer func() { c.observe(method, time.Since(start)) }()
	}
	c.nextID++
	id := strconv.Itoa(c.nextID)
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method, "params": params}); err != nil {
		return err
	}
	for {
		msg, err := c.read()
		if err != nil {
			return err
		}
		if msg.Method == "" && string(msg.ID) == id {
			if msg.Error != nil {
				return msg.Error
			}
			if out != nil && len(msg.Result) > 0 {
				return json.Unmarshal(msg.Result, out)
			}
			return nil
		}
		if err := c.dispatch(msg); err != nil {
			return err
		}
	}
}

func (c *rpcConn) nextEvent() (cliEvent, error) {
	for len(c.queue) == 0 {
		msg, err := c.read()
		if err != nil {
			return cliEvent{}, err
		}
		if err := c.dispatch(msg); err != nil {
			return cliEvent{}, err
		}
	}
	ev := c.queue[0]
	c.queue[0] = cliEvent{}
	c.queue = c.queue[1:]
	if ev.Type == "session.usage_checkpoint" {
		c.usageSeen = true
	}
	if ev.Type == "session.idle" {
		c.idleSeen = true
	}
	return ev, nil
}

// bounded runs f, closing the read side if it outlives d. Used for the
// best-effort steps after the answer is already known.
func (c *rpcConn) bounded(d time.Duration, f func()) {
	done := make(chan struct{})
	t := time.AfterFunc(d, func() { defer close(done); c.closeRead() })
	defer func() {
		if !t.Stop() {
			<-done
			c.tainted = true
			c.reusable = false
		}
	}()
	f()
}

// ping is the zero-cost gate: no session, no model call.
func (c *rpcConn) ping() error {
	var pong struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if err := c.call("ping", map[string]any{"message": "ezllm"}, &pong); err != nil {
		return toolsRefused("the CLI's headless channel did not answer", err)
	}
	if pong.ProtocolVersion != channelProtocolVersion {
		return toolsRefused(fmt.Sprintf("the CLI's headless channel speaks protocol %d, this bridge speaks %d", pong.ProtocolVersion, channelProtocolVersion), nil)
	}
	return nil
}

func toolsRefused(why string, cause error) error {
	return &Failure{Code: "chat_tools_unsupported", Msg: "copilot bridge: client tools are unavailable: " + why, Cause: cause, BeforeInference: true}
}

// sessionConfig builds session.create. availableTools is an allowlist, so the
// CLI's own shell and file tools are invisible, and nothing the caller did not
// declare can run on this box. It must be present even when empty: measured on
// CLI 1.0.105, [] shows the model 0 tools but leaving it out shows 18, including
// bash and apply_patch. overridesBuiltInTool keeps a caller's tool that shares a
// built-in's name (bash, view) routed to the caller.
func sessionConfig(turn ChannelTurn) map[string]any {
	tools := make([]map[string]any, 0, len(turn.Tools))
	names := make([]string, 0, len(turn.Tools))
	for _, d := range turn.Tools {
		var params any = json.RawMessage(`{"type":"object","properties":{}}`)
		if p := strings.TrimSpace(string(d.Function.Parameters)); p != "" && p != "null" {
			params = d.Function.Parameters
		}
		tools = append(tools, map[string]any{
			"name": d.Function.Name, "description": d.Function.Description, "parameters": params,
			"overridesBuiltInTool": true, "skipPermission": true,
		})
		names = append(names, d.Function.Name)
	}
	system := plainNote
	if len(tools) > 0 {
		system = toolNote
	}
	// The channel has no tool_choice: a named function narrows what the model can
	// see, and "must call" is only an instruction the model may ignore.
	switch {
	case turn.Choice.Name != "":
		names = []string{turn.Choice.Name}
		system += " You must call the function " + turn.Choice.Name + " in this reply."
	case turn.Choice.Required:
		system += " You must call at least one function in this reply."
	}
	if s := strings.TrimSpace(turn.System); s != "" {
		system = s + "\n\n" + system
	}
	cfg := map[string]any{
		"tools": tools, "availableTools": names,
		"systemMessage":    map[string]any{"mode": "replace", "content": system},
		"streaming":        true,
		"mcpServers":       map[string]any{},
		"infiniteSessions": map[string]any{"enabled": false},
		// Nobody is here to answer: a prompt for either would hang the turn.
		"requestPermission": false, "requestUserInput": false,
	}
	if turn.Model != "" && turn.Model != "auto" {
		cfg["model"] = turn.Model
	}
	return cfg
}

// classifyCreate types a session.create refusal. Both cases cost nothing because
// no model has been called yet, so a tool the CLI cannot register fails over.
func classifyCreate(err error) error {
	var re *rpcError
	if !errors.As(err, &re) {
		return err
	}
	msg := strings.ToLower(re.Message)
	if strings.Contains(msg, "externaltooldefinitions") || strings.Contains(msg, "tool name") {
		return toolsRefused("the CLI rejected a tool definition", errors.New(clipDetail(re.Message)))
	}
	return ClassifyExit(errors.New(clipDetail(re.Message)), re.Message)
}

// classifySession types a session.error event, whose errorType is snake_case
// ("rate_limit") where ClassifyExit reads the CLI's prose.
func classifySession(ev cliEvent) error {
	d := ev.Data
	words := strings.ReplaceAll(d.ErrorType+" "+d.ErrorCode, "_", " ") + " " + d.Message
	return ClassifyExit(errors.New(clipDetail(d.Message)), words)
}

func clipDetail(s string) string {
	if len(s) > maxErrorDetail {
		return s[:maxErrorDetail] + "..."
	}
	return s
}

// driveTurn runs one turn over an open channel: gate, create, send, then
// events until the model calls a declared tool or the session goes idle.
func driveTurn(c *rpcConn, turn ChannelTurn, delta func(string) error, onFirst func()) (res Result, retErr error) {
	c.reusable = false
	c.tainted = false
	c.idleSeen, c.usageSeen = false, false
	began := time.Now()
	if err := c.ping(); err != nil {
		return res, err
	}
	if len(turn.Attachments) > 0 {
		if err := c.validateImages(turn.Model, turn.Attachments); err != nil {
			return res, err
		}
	}
	var created struct {
		SessionID string `json:"sessionId"`
	}
	prompt := turn.Prompt
	var prior Usage
	reused := false
	if c.completed != nil {
		if next, ok := c.completed.continuation(turn); ok && c.cacheEnabled {
			prompt = next
			prior = c.completed.usage
			reused = true
			created.SessionID = c.session
			c.queue = nil
			c.pending = nil
			if c.observe != nil {
				c.observe("session_cache_hit", 0)
			}
		} else if !c.deleteSession(c.session) {
			// Nothing has been sent for this turn yet: the failure is local and
			// pre-inference. No new inference is dispatched while cleanup of the
			// previous session is uncertain; the caller gets a typed refusal.
			return res, &Failure{Code: codeCachedSessionCleanup, Msg: "copilot-bridge: cached session cleanup failed", BeforeInference: true}
		}
	}
	if !reused {
		if err := c.call("session.create", sessionConfig(turn), &created); err != nil {
			return res, classifyCreate(err)
		}
		if created.SessionID == "" || (c.strict && c.retired[created.SessionID]) {
			return res, errors.New("copilot-bridge: empty or reused session id")
		}
		c.session = created.SessionID
	}
	res.SessionID = created.SessionID
	defer func() {
		cumulative := res.Usage
		if reused && c.usageSeen {
			var ok bool
			res.Usage, ok = usageDelta(cumulative, prior)
			if !ok {
				c.tainted = true
				if retErr == nil {
					retErr = errSessionUsageReset
				}
			}
		}
		if c.cacheEnabled && c.strict && cacheEligible(turn) && retErr == nil && len(res.ToolCalls) == 0 && c.idleSeen && c.usageSeen && !c.tainted {
			hold := res
			hold.Usage = cumulative
			if entry := completedFrom(turn, hold); entry != nil {
				c.completed = entry
				c.reusable = true
				return
			}
		}
		clean := c.deleteSession(created.SessionID)
		c.reusable = clean && c.idleSeen && c.usageSeen && !c.tainted
	}()
	pending := c.pending
	c.pending = nil
	for _, msg := range pending {
		if err := c.dispatch(msg); err != nil {
			return res, err
		}
	}
	send := map[string]any{"sessionId": created.SessionID, "prompt": prompt}
	if len(turn.Attachments) > 0 {
		send["attachments"] = turn.Attachments
	}
	if err := c.call("session.send", send, nil); err != nil {
		failure := classifyCreate(err)
		var typed *Failure
		if errors.As(failure, &typed) {
			typed.BeforeInference = false // dispatch may already have started a model call
		}
		return res, failure
	}

	modelStart := time.Now()
	declared := make(map[string]bool, len(turn.Tools))
	for _, d := range turn.Tools {
		declared[d.Function.Name] = true
	}
	var built strings.Builder
	finalText := ""
	dropped := make([]string, 0, 2) // tool names the model asked for but the caller never declared
	first := false
	stamp := func() {
		if !first {
			first = true
			if c.observe != nil {
				c.observe("first_event", time.Since(began))
				c.observe("model_wait", time.Since(modelStart))
			}
			if onFirst != nil {
				onFirst()
			}
		}
	}
	finish := func() {
		res.Text = finalText
		if res.Text == "" {
			res.Text = built.String()
		}
	}
	for {
		ev, err := c.nextEvent()
		if err != nil {
			finish()
			return res, err
		}
		switch ev.Type {
		case "assistant.message_delta":
			stamp()
			if ev.Data.DeltaContent != "" {
				built.WriteString(ev.Data.DeltaContent)
				if delta != nil {
					if err := delta(ev.Data.DeltaContent); err != nil {
						finish()
						return res, err
					}
				}
			}
		case "assistant.message":
			stamp()
			if ev.Data.Content != "" {
				finalText = ev.Data.Content
			}
			if ev.Data.Model != "" {
				res.Usage.Model = ev.Data.Model
			}
			// An undeclared name is a hallucination: the CLI answers it itself
			// and the model carries on, so it is not handed to the caller. It is
			// remembered, though: a turn that ends with nothing but dropped calls
			// must say which names it dropped, not "no assistant message".
			for _, tr := range ev.Data.ToolRequests {
				if declared[tr.Name] {
					res.ToolCalls = append(res.ToolCalls, ToolCall{ID: tr.ToolCallID, Name: tr.Name, Arguments: normaliseArgs(tr.Arguments)})
				} else if tr.Name != "" {
					dropped = append(dropped, tr.Name)
					c.tainted = true
				}
			}
			if len(res.ToolCalls) > 0 {
				finish()
				if c.observe != nil {
					c.observe("tool_batch", time.Since(began))
				}
				var deliveryErr error
				if turn.OnTools != nil {
					deliveryErr = turn.OnTools(res)
				}
				drainStart := time.Now()
				c.bounded(drainTimeout, func() { drainAborted(c, created.SessionID, &res) })
				if c.observe != nil {
					c.observe("usage_drain", time.Since(drainStart))
				}
				return res, deliveryErr
			}
		case "session.auto_mode_resolved":
			res.AvailableModels = ev.Data.AvailableModels
		case "session.usage_checkpoint":
			ev.applyUsage(&res.Usage)
		case "session.error":
			finish()
			return res, classifySession(ev)
		case "session.idle":
			finish()
			if res.Text == "" && len(res.ToolCalls) == 0 && len(dropped) > 0 {
				// The body carried calls, every one of them undeclared, and the
				// model then stopped: the precise cause is the names, which a
				// generic "no assistant message" would have hidden.
				return res, &Failure{Code: "chat_tools_unsupported",
					Msg: "copilot bridge: the model asked for tools the caller never declared: " + strings.Join(unique(dropped), ", ")}
			}
			return res, nil
		}
	}
}

// unique keeps the first occurrence of each name, for a stable message.
func unique(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// drainAborted stops the turn and collects its usage. A failure here costs only
// the ledger's numbers for this turn, never the calls already captured.
func drainAborted(c *rpcConn, sessionID string, res *Result) {
	if c.call("session.abort", map[string]any{"sessionId": sessionID}, nil) != nil {
		return
	}
	for {
		ev, err := c.nextEvent()
		if err != nil {
			return
		}
		switch ev.Type {
		case "session.usage_checkpoint":
			ev.applyUsage(&res.Usage)
		case "session.idle":
			return
		}
	}
}

// RunChannel executes one turn over the channel, under the same slots and queue
// as Run. Result.ToolCalls holds the batch if the model made one.
// codeCachedSessionCleanup marks a reused worker that could not delete its
// previous cached session before this turn started.
const codeCachedSessionCleanup = "cached_session_cleanup"

func (r *Runner) RunChannel(ctx context.Context, turn ChannelTurn, delta func(string) error) (Result, error) {
	r.init()
	return r.slotted(ctx, func() (Result, error) {
		timeout := r.Timeout
		if timeout <= 0 {
			timeout = 180 * time.Second
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		began := time.Now()
		var res Result
		with := r.withChannel
		if r.WarmPool {
			with = func(ctx context.Context, f func(*rpcConn) error) error { return r.withPooledTurn(ctx, &turn, f) }
		}
		run := func(c *rpcConn) error {
			var e error
			c.cacheEnabled = r.SessionCacheEnabled()
			res, e = driveTurn(c, turn, delta, func() { r.stampFirstByte(time.Since(began)) })
			if e == nil && res.Text == "" && len(res.ToolCalls) == 0 {
				e = errors.New("copilot-bridge: the CLI finished without an assistant message or a tool call")
			}
			return e
		}
		err := with(ctx, run)
		if err != nil {
			return res, err
		}
		r.logTurn(res)
		return res, nil
	})
}

// ProbeChannel reports whether the channel answers at the expected version. It
// runs no session, so it costs nothing and is safe at boot.
func (r *Runner) ProbeChannel(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return r.withChannel(ctx, func(c *rpcConn) error { return c.ping() })
}

// withChannel spawns one headless child for f and always reaps it. A channel
// that dies mid-turn is classified from the child's own words, as `-p` is.
func (r *Runner) withChannel(ctx context.Context, f func(*rpcConn) error) error {
	ctx, kill := context.WithCancel(ctx)
	defer kill()
	cmd := exec.CommandContext(ctx, r.Bin, "--headless", "--stdio", "--no-auto-update", "--log-level", "error", "--disable-builtin-mcps")
	cmd.Dir = os.TempDir() // no repository context: this is a completion call
	cmd.Env = childEnv(os.Environ(), r.Home)
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("copilot-bridge: stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("copilot-bridge: stdout: %w", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, max: 8 << 10}
	spawnStart := time.Now()
	if err := cmd.Start(); err != nil {
		return toolsRefused("the CLI could not start", err)
	}
	r.observeStage("spawn", time.Since(spawnStart))
	// A blocked read has no context of its own: closing it is what ends it.
	closeRead := func() { _ = stdout.Close() }
	stop := context.AfterFunc(ctx, closeRead)
	defer stop()

	ferr := f(&rpcConn{w: stdin, r: bufio.NewReader(stdout), closeRead: closeRead, observe: r.observeStage})
	ctxErr := ctx.Err() // before kill() makes it non-nil
	_ = stdin.Close()
	kill()
	_ = cmd.Wait()

	var typed *Failure
	switch {
	case ctxErr != nil && ferr != nil:
		return ctxErr
	case errors.As(ferr, &typed):
		return ferr // already says what happened, e.g. the gate's refusal
	case errors.Is(ferr, errChannelClosed):
		return ClassifyExit(errors.New("the channel closed mid-turn"), stderr.String())
	}
	return ferr
}
