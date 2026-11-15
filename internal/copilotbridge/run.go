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
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Usage is what the CLI's own accounting reported for one turn.
//
// Measured on CLI 1.0.105: `prompt_tokens` is INCLUSIVE (input + cache_read +
// cache_write), `tokenDetails.input` is the COLD slice, and the split is real —
// a repeat call reported cache_write 13,960 with input 3. That is why this
// bridge books exact numbers instead of a synthesized envelope; the only
// judgement left is which slices map to the OpenAI fields.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	CachedRead       int
	CachedWrite      int
	ReasoningTokens  int
	PremiumRequests  float64 // a model multiplier can make this fractional
	// NanoAIU is the turn's AI-credit charge in billionths of a credit, the unit
	// token-billed plans pay in. Zero when the CLI did not report it.
	NanoAIU int64
	Model   string
}

// Result is one headless turn.
type Result struct {
	Text  string
	Usage Usage
	// SessionID identifies the native turn for diagnostics and cache qualification.
	SessionID string
	// ToolCalls is the batch the model asked the caller to run (RunChannel only).
	ToolCalls []ToolCall
	// AvailableModels is what the CLI says this login can use, read from the
	// session.auto_mode_resolved event of an `auto` turn. It costs nothing: it
	// rides a turn that was happening anyway.
	AvailableModels []string
}

// Executor runs one turn. *Runner implements it; tests inject a fake.
type Executor interface {
	// Run is a plain chat turn through `copilot -p`, the fallback for a CLI whose
	// channel is unavailable.
	Run(ctx context.Context, model, prompt string, delta func(string) error) (Result, error)
	// RunChannel is a chat turn, with or without client tools, over the channel.
	RunChannel(ctx context.Context, turn ChannelTurn, delta func(string) error) (Result, error)
}

// Runner uses the official Copilot CLI, cold by default or in an opt-in process pool.
//
// The prompt goes in on STDIN, never argv: this client's prompts run 250-600 KB
// and a single argv element is capped at MAX_ARG_STRLEN (128 KB), which fails
// with E2BIG long before anything reaches Copilot. Piped input is a documented
// prompt source (`--trust-folder` names "-p or piped standard input"), and it
// was verified against 1.0.105 before this file existed.
type Runner struct {
	Bin           string
	Home          string // COPILOT_HOME: holds the CLI's own login
	Timeout       time.Duration
	MaxConcurrent int
	// MaxQueue caps requests WAITING for a slot (default 8). Past it Run fails
	// at once with queue_full instead of parking the caller until its context
	// ends: a 2-slot bridge with a 300 s timeout would otherwise hold sockets
	// for minutes behind a stall.
	MaxQueue int
	// WarmPool reuses processes. Sessions stay fresh unless SessionCache is enabled.
	WarmPool bool
	// SessionCache retains completed conversations only with exact history matches.
	SessionCache    bool
	PoolIdleTimeout time.Duration
	PoolMaxAge      time.Duration
	PoolMaxTurns    int
	poolMu          sync.Mutex
	workers         map[*channelWorker]bool
	idleWorkers     []*channelWorker
	closed          bool
	Logf            func(format string, args ...any)

	sem  chan struct{}
	once sync.Once

	inflight, queued, served, rejected  atomic.Int64
	firstByteNS, firstByteN, lastByteNS atomic.Int64
	mu                                  sync.Mutex
	outcomes                            map[string]int64
	stages                              map[string]stageTiming
}

const defaultMaxQueue = 8

// Stats is the runtime picture /status serves: pressure (in flight, queued,
// rejected), what turns ended as, and how long the CLI takes to say anything.
func (r *Runner) Stats() map[string]any {
	r.init()
	r.mu.Lock()
	oc := make(map[string]int64, len(r.outcomes))
	for k, v := range r.outcomes {
		oc[k] = v
	}
	stages := make(map[string]any, len(r.stages))
	for name, v := range r.stages {
		stages[name] = map[string]any{"count": v.count, "avg_ms": float64(v.total) / float64(v.count) / float64(time.Millisecond), "last_ms": float64(v.last) / float64(time.Millisecond)}
	}
	r.mu.Unlock()
	r.poolMu.Lock()
	pool := map[string]any{"enabled": r.WarmPool, "workers": len(r.workers), "idle": len(r.idleWorkers), "closed": r.closed}
	r.poolMu.Unlock()
	avg := int64(0)
	if n := r.firstByteN.Load(); n > 0 {
		avg = r.firstByteNS.Load() / n / int64(time.Millisecond)
	}
	return map[string]any{
		"in_flight":           r.inflight.Load(),
		"queued":              r.queued.Load(),
		"max_queue":           r.maxQueue(),
		"max_concurrent":      cap(r.sem),
		"served":              r.served.Load(),
		"rejected_queue_full": r.rejected.Load(),
		"outcomes":            oc,
		"stage_timings":       stages,
		"pool":                pool,
		"first_byte_ms_avg":   avg,
		"first_byte_ms_last":  r.lastByteNS.Load() / int64(time.Millisecond),
	}
}

func (r *Runner) maxQueue() int {
	if r.MaxQueue > 0 {
		return r.MaxQueue
	}
	return defaultMaxQueue
}

func (r *Runner) record(err error) {
	code := "ok"
	var f *Failure
	switch {
	case err == nil:
	case errors.As(err, &f):
		code = f.Code
	case errors.Is(err, context.DeadlineExceeded):
		code = "timeout"
	case errors.Is(err, context.Canceled):
		code = "canceled"
	default:
		code = "error"
	}
	r.mu.Lock()
	if r.outcomes == nil {
		r.outcomes = map[string]int64{}
	}
	r.outcomes[code]++
	r.mu.Unlock()
}

// firstByteReader stamps how long the CLI took to emit its first output.
type firstByteReader struct {
	r     io.Reader
	start time.Time
	once  sync.Once
	onHit func(time.Duration)
}

func (f *firstByteReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if n > 0 {
		f.once.Do(func() { f.onHit(time.Since(f.start)) })
	}
	return n, err
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func (r *Runner) init() {
	r.once.Do(func() {
		n := r.MaxConcurrent
		if n <= 0 {
			n = 2
		}
		r.sem = make(chan struct{}, n)
	})
}

// Run executes one prompt and streams assistant text deltas through delta.
// A delta error aborts the turn (the caller has stopped reading).
func (r *Runner) Run(ctx context.Context, model, prompt string, delta func(string) error) (Result, error) {
	r.init()
	return r.slotted(ctx, func() (Result, error) { return r.run(ctx, model, prompt, delta) })
}

// slotted runs one turn under the concurrency slots and the bounded queue.
func (r *Runner) slotted(ctx context.Context, turn func() (Result, error)) (Result, error) {
	if err := r.accepting(ctx); err != nil {
		return Result{}, err
	}
	waitStart := time.Now()
	select {
	case r.sem <- struct{}{}:
	default:
		if int(r.queued.Add(1)) > r.maxQueue() {
			r.queued.Add(-1)
			r.rejected.Add(1)
			err := &Failure{Code: "queue_full", Msg: "copilot bridge: every CLI slot is busy and the queue is full; retry shortly"}
			r.record(err)
			return Result{}, err
		}
		select {
		case r.sem <- struct{}{}:
			r.queued.Add(-1)
		case <-ctx.Done():
			r.queued.Add(-1)
			r.record(ctx.Err())
			return Result{}, ctx.Err()
		}
	}
	if err := r.accepting(ctx); err != nil {
		<-r.sem
		return Result{}, err
	}
	r.observeStage("queue", time.Since(waitStart))
	defer func() { <-r.sem }()
	r.inflight.Add(1)
	defer r.inflight.Add(-1)
	res, err := turn()
	r.served.Add(1)
	r.record(err)
	return res, err
}

func (r *Runner) stampFirstByte(d time.Duration) {
	r.firstByteNS.Add(int64(d))
	r.firstByteN.Add(1)
	r.lastByteNS.Store(int64(d))
}

func (r *Runner) logTurn(res Result) {
	r.logf("copilot-bridge: turn ok session=%s model=%s in=%d cached=%d out=%d premium=%g nano_aiu=%d tool_calls=%d",
		res.SessionID, res.Usage.Model, res.Usage.PromptTokens, res.Usage.CachedRead,
		res.Usage.CompletionTokens, res.Usage.PremiumRequests, res.Usage.NanoAIU, len(res.ToolCalls))
}

func (r *Runner) run(ctx context.Context, model, prompt string, delta func(string) error) (Result, error) {

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// No --allow-tool: without a permission grant the CLI cannot run its own
	// agent tools, so a stray tool attempt is denied rather than executed
	// against this box. That is the safe default for a chat surface.
	args := []string{"-s", "--output-format=json", "--no-ask-user"}
	if model != "" && model != "auto" {
		args = append(args, "--model", model)
	}
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Dir = os.TempDir() // no repository context: this is a completion call
	cmd.Env = childEnv(os.Environ(), r.Home)
	// A grandchild holding the stdout pipe must not pin Wait (and a concurrency
	// slot) after the kill.
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("copilot-bridge: stdout: %w", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &limitedWriter{w: &stderr, max: 8 << 10}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("copilot-bridge: start %s: %w", r.Bin, err)
	}

	began := time.Now()
	res, parseErr := parseStream(&firstByteReader{r: stdout, start: began, onHit: r.stampFirstByte}, delta)
	if parseErr != nil && !errors.Is(parseErr, io.EOF) {
		// Nobody reads stdout any more: kill the child now or it blocks on a
		// full pipe until the timeout while holding a slot.
		cancel()
	}
	werr := cmd.Wait()

	if parseErr != nil && !errors.Is(parseErr, io.EOF) {
		return res, parseErr
	}
	if werr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return res, ctxErr
		}
		return res, ClassifyExit(werr, stderr.String())
	}
	if res.Text == "" {
		// Exit 0 with no assistant message is not a success: the client would
		// receive an empty answer and no error.
		return res, fmt.Errorf("copilot-bridge: the CLI finished without an assistant message (%s)", firstLine(stderr.String()))
	}
	r.logTurn(res)
	return res, nil
}

// num reads a CLI number. The CLI writes the same field as 1 on -p and 1.0 on the
// tool channel (measured, CLI 1.0.105), and a model with a fractional multiplier
// would write 0.33. Decoding that into an int fails the WHOLE event and the turn
// books zero usage, so nothing here is an int, and an odd value reads as zero
// rather than losing the event.
type num float64

func (n *num) UnmarshalJSON(b []byte) error {
	var f float64
	if json.Unmarshal(b, &f) == nil {
		*n = num(f)
	}
	return nil
}

// cliEvent is one CLI session event. `-p` emits them as JSONL and the tool
// channel wraps the same shape in a JSON-RPC notification.
type cliEvent struct {
	Type string `json:"type"`
	// `result` carries sessionId and usage at the TOP level, not under
	// data (measured; the fixture pins it).
	SessionID string `json:"sessionId"`
	Usage     struct {
		PremiumRequests num `json:"premiumRequests"`
	} `json:"usage"`
	Data struct {
		AvailableModels []string `json:"availableModels"`
		DeltaContent    string   `json:"deltaContent"`
		Content         string   `json:"content"`
		Model           string   `json:"model"`
		SessionID       string   `json:"sessionId"`
		// assistant.message: what the model asked to run.
		ToolRequests []struct {
			ToolCallID string          `json:"toolCallId"`
			Name       string          `json:"name"`
			Arguments  json.RawMessage `json:"arguments"`
		} `json:"toolRequests"`
		// session.error
		ErrorType string `json:"errorType"`
		ErrorCode string `json:"errorCode"`
		Message   string `json:"message"`
		Usage     struct {
			PremiumRequests num `json:"premiumRequests"`
		} `json:"usage"`
		TotalNanoAIU num `json:"totalNanoAiu"`
		// session.usage_checkpoint (one entry per model in the turn)
		AccountingSnapshot struct {
			TokenDetails struct {
				Input      struct{ TokenCount num } `json:"input"`
				CacheRead  struct{ TokenCount num } `json:"cache_read"`
				CacheWrite struct{ TokenCount num } `json:"cache_write"`
				Output     struct{ TokenCount num } `json:"output"`
			} `json:"tokenDetails"`
			ProviderModelMetrics []struct {
				ModelID string `json:"modelId"`
				Metrics struct {
					Usage struct {
						InputTokens      num `json:"inputTokens"`
						OutputTokens     num `json:"outputTokens"`
						CacheReadTokens  num `json:"cacheReadTokens"`
						CacheWriteTokens num `json:"cacheWriteTokens"`
						ReasoningTokens  num `json:"reasoningTokens"`
					} `json:"usage"`
				} `json:"metrics"`
			} `json:"providerModelMetrics"`
			TotalPremiumRequestCost num `json:"totalPremiumRequestCost"`
		} `json:"accountingSnapshot"`
	} `json:"data"`
}

// applyUsage folds a session.usage_checkpoint into u.
func (ev cliEvent) applyUsage(u *Usage) {
	snap := ev.Data.AccountingSnapshot
	u.CachedRead = int(snap.TokenDetails.CacheRead.TokenCount)
	u.CachedWrite = int(snap.TokenDetails.CacheWrite.TokenCount)
	u.CompletionTokens = int(snap.TokenDetails.Output.TokenCount)
	// INCLUSIVE prompt total: input is the COLD part, cache_read and
	// cache_write ride beside it (measured: 3 + 0 + 13,960 = 13,963).
	u.PromptTokens = int(snap.TokenDetails.Input.TokenCount) + u.CachedRead + u.CachedWrite
	// Per-model inputTokens is already inclusive, unlike tokenDetails.input.
	// The CLI fixture reports 13,963 here alongside 13,960 cache-write tokens;
	// adding cache again would turn the same request into 27,923 input tokens.
	var perModel Usage
	for _, m := range snap.ProviderModelMetrics {
		mu := m.Metrics.Usage
		perModel.PromptTokens += int(mu.InputTokens)
		perModel.CachedRead += int(mu.CacheReadTokens)
		perModel.CachedWrite += int(mu.CacheWriteTokens)
		perModel.CompletionTokens += int(mu.OutputTokens)
		perModel.ReasoningTokens += int(mu.ReasoningTokens)
	}
	u.ReasoningTokens = perModel.ReasoningTokens // subset of output, never added to it
	if u.PromptTokens == 0 {
		u.PromptTokens = perModel.PromptTokens
		u.CachedRead, u.CachedWrite = perModel.CachedRead, perModel.CachedWrite
		u.CompletionTokens = perModel.CompletionTokens
		if u.Model == "" && len(snap.ProviderModelMetrics) == 1 {
			u.Model = snap.ProviderModelMetrics[0].ModelID
		}
	}
	if snap.TotalPremiumRequestCost > 0 {
		u.PremiumRequests = float64(snap.TotalPremiumRequestCost)
	}
	if ev.Data.TotalNanoAIU > 0 {
		u.NanoAIU = int64(ev.Data.TotalNanoAIU)
	}
}

// parseStream reads the JSONL event stream. The event set is version-sensitive,
// so every case is defensive: an unknown type is skipped, and a missing usage
// block stays ZERO rather than being invented.
func parseStream(r io.Reader, delta func(string) error) (Result, error) {
	var res Result
	var built strings.Builder // assembled deltas, used only if no final message
	var finalText string
	dec := bufio.NewScanner(r)
	dec.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for dec.Scan() {
		line := dec.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev cliEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // a malformed line must not kill an otherwise good turn
		}
		switch ev.Type {
		case "assistant.message_delta":
			if ev.Data.DeltaContent != "" {
				built.WriteString(ev.Data.DeltaContent)
				if delta != nil {
					if err := delta(ev.Data.DeltaContent); err != nil {
						return res, err
					}
				}
			}
		case "assistant.message":
			if ev.Data.Content != "" {
				finalText = ev.Data.Content
			}
			if ev.Data.Model != "" {
				res.Usage.Model = ev.Data.Model
			}
		case "session.auto_mode_resolved":
			res.AvailableModels = ev.Data.AvailableModels
		case "session.usage_checkpoint":
			ev.applyUsage(&res.Usage)
		case "result":
			if ev.SessionID != "" {
				res.SessionID = ev.SessionID
			}
			if ev.Usage.PremiumRequests > 0 {
				res.Usage.PremiumRequests = float64(ev.Usage.PremiumRequests)
			}
		}
	}
	if err := dec.Err(); err != nil {
		return res, fmt.Errorf("copilot-bridge: read event stream: %w", err)
	}
	res.Text = finalText
	if res.Text == "" {
		res.Text = built.String()
	}
	return res, nil
}

// ClassifyExit turns the CLI's own words into a typed failure the router can
// act on: an auth problem must never look like a transport blip (fail fast, no
// backoff) and a plan/premium refusal must not park the hop as unhealthy.
func ClassifyExit(err error, stderr string) error {
	msg := strings.ToLower(stderr)
	switch {
	case strings.Contains(msg, "not logged in"), strings.Contains(msg, "please log in"),
		strings.Contains(msg, "authentication failed"), strings.Contains(msg, "authentication required"),
		strings.Contains(msg, "unauthorized"), strings.Contains(msg, "no github oauth token"):
		return &Failure{Code: "not_logged_in", Msg: "copilot bridge: the CLI is not logged in for this COPILOT_HOME", Cause: err}
	case strings.Contains(msg, "premium request"), strings.Contains(msg, "quota"), strings.Contains(msg, "rate limit"):
		return &Failure{Code: "quota_exhausted", Msg: "copilot bridge: the plan refused this request (premium requests or quota)", Cause: err}
	case strings.Contains(msg, "not included in your plan"), strings.Contains(msg, "not in your plan"),
		strings.Contains(msg, "model") && strings.Contains(msg, "not available"):
		return &Failure{Code: "model_not_in_plan", Msg: "copilot bridge: that model is not included in this Copilot plan", Cause: err}
	}
	return &Failure{Code: "cli_failed", Msg: "copilot bridge: the CLI exited with an error", Cause: err}
}

// Failure is a typed bridge failure: Code rides the ledger's [cause] marker and
// the shared error envelope.
type Failure struct {
	Code  string
	Msg   string
	Cause error
	// BeforeInference is set only for local startup/handshake/registration
	// refusals. A missing usage counter does not establish that work was free.
	BeforeInference bool
}

func (f *Failure) Error() string {
	if f.Cause == nil {
		return f.Msg
	}
	return f.Msg + ": " + f.Cause.Error()
}
func (f *Failure) Unwrap() error { return f.Cause }

type limitedWriter struct {
	w   io.Writer
	max int
	n   int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n >= l.max {
		return len(p), nil // swallow, never grow a CLI's stderr without bound
	}
	room := l.max - l.n
	if len(p) > room {
		p = p[:room]
	}
	l.n += len(p)
	return l.w.Write(p)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// childEnv is the CLI child's environment: an allowlist, not os.Environ(). The
// child runs an agent loop, and ezllm's own process carries admin tokens and
// provider keys that it has no business seeing.
func childEnv(parent []string, home string) []string {
	keep := map[string]bool{
		"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "LC_ALL": true, "TZ": true,
		// Identity and XDG dirs: the deploy sets USER/LOGNAME for the bridge
		// CLIs on purpose (a container uid has no passwd entry), and a CLI that
		// cannot resolve them or its cache dir can fail before it reaches auth.
		"USER": true, "LOGNAME": true, "XDG_CONFIG_HOME": true, "XDG_CACHE_HOME": true,
		"XDG_DATA_HOME": true, "XDG_STATE_HOME": true, "XDG_RUNTIME_DIR": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
		"http_proxy": true, "https_proxy": true, "no_proxy": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true,
	}
	env := make([]string, 0, len(keep)+1)
	for _, kv := range parent {
		if k, _, ok := strings.Cut(kv, "="); ok && keep[k] {
			env = append(env, kv)
		}
	}
	if home != "" {
		env = append(env, "COPILOT_HOME="+home)
	}
	// The CLI extracts a ~236 MB bundled package into its cache dir on first run.
	// A container uid has no home (HOME=/), so without a writable cache it dies
	// with "EACCES: mkdir '/.cache'" (measured 2026-11-05). Default to a temp
	// location; a deploy that wants it to survive restarts sets XDG_CACHE_HOME.
	if !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "XDG_CACHE_HOME=") }) {
		env = append(env, "XDG_CACHE_HOME="+filepath.Join(os.TempDir(), "ezllm-copilot-cache"))
	}
	return env
}

// stageTiming holds aggregate diagnostics only; no request content.
type stageTiming struct {
	count       int64
	total, last time.Duration
}

func (r *Runner) observeStage(name string, elapsed time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stages == nil {
		r.stages = make(map[string]stageTiming)
	}
	v := r.stages[name]
	v.count++
	v.total += elapsed
	v.last = elapsed
	r.stages[name] = v
}
