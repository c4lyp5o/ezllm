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
	PremiumRequests  int
	Model            string
}

// Result is one headless turn.
type Result struct {
	Text  string
	Usage Usage
	// SessionID lets a later milestone resume this conversation; M1 only logs
	// it, because a resumed session cannot accept an external tool result (see
	// docs/copilot-cli-bridge-design.md §10).
	SessionID string
}

// Executor runs one prompt. *Runner implements it; tests inject a fake.
type Executor interface {
	Run(ctx context.Context, model, prompt string, delta func(string) error) (Result, error)
}

// Runner spawns the official Copilot CLI once per request.
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
	Logf     func(format string, args ...any)

	sem  chan struct{}
	once sync.Once

	inflight, queued, served, rejected  atomic.Int64
	firstByteNS, firstByteN, lastByteNS atomic.Int64
	mu                                  sync.Mutex
	outcomes                            map[string]int64
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
	r.mu.Unlock()
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
	defer func() { <-r.sem }()
	r.inflight.Add(1)
	defer r.inflight.Add(-1)
	res, err := r.run(ctx, model, prompt, delta)
	r.served.Add(1)
	r.record(err)
	return res, err
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
	res, parseErr := parseStream(&firstByteReader{r: stdout, start: began, onHit: func(d time.Duration) {
		r.firstByteNS.Add(int64(d))
		r.firstByteN.Add(1)
		r.lastByteNS.Store(int64(d))
	}}, delta)
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
	r.logf("copilot-bridge: turn ok session=%s model=%s in=%d cached=%d out=%d premium=%d",
		res.SessionID, res.Usage.Model, res.Usage.PromptTokens, res.Usage.CachedRead,
		res.Usage.CompletionTokens, res.Usage.PremiumRequests)
	return res, nil
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
		var ev struct {
			Type string `json:"type"`
			// `result` carries sessionId and usage at the TOP level, not under
			// data (measured; the fixture pins it).
			SessionID string `json:"sessionId"`
			Usage     struct {
				PremiumRequests int `json:"premiumRequests"`
			} `json:"usage"`
			Data struct {
				DeltaContent string `json:"deltaContent"`
				Content      string `json:"content"`
				Model        string `json:"model"`
				SessionID    string `json:"sessionId"`
				Usage        struct {
					PremiumRequests int `json:"premiumRequests"`
				} `json:"usage"`
				// session.usage_checkpoint (one entry per model in the turn)
				AccountingSnapshot struct {
					TokenDetails struct {
						Input      struct{ TokenCount int } `json:"input"`
						CacheRead  struct{ TokenCount int } `json:"cache_read"`
						CacheWrite struct{ TokenCount int } `json:"cache_write"`
						Output     struct{ TokenCount int } `json:"output"`
					} `json:"tokenDetails"`
					ProviderModelMetrics []struct {
						ModelID string `json:"modelId"`
						Metrics struct {
							Usage struct {
								InputTokens      int `json:"inputTokens"`
								OutputTokens     int `json:"outputTokens"`
								CacheReadTokens  int `json:"cacheReadTokens"`
								CacheWriteTokens int `json:"cacheWriteTokens"`
								ReasoningTokens  int `json:"reasoningTokens"`
							} `json:"usage"`
						} `json:"metrics"`
					} `json:"providerModelMetrics"`
					TotalPremiumRequestCost int `json:"totalPremiumRequestCost"`
				} `json:"accountingSnapshot"`
			} `json:"data"`
		}
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
		case "session.usage_checkpoint":
			u := &res.Usage
			snap := ev.Data.AccountingSnapshot
			u.CachedRead = snap.TokenDetails.CacheRead.TokenCount
			u.CachedWrite = snap.TokenDetails.CacheWrite.TokenCount
			u.CompletionTokens = snap.TokenDetails.Output.TokenCount
			// INCLUSIVE prompt total: input is the COLD part, cache_read and
			// cache_write ride beside it (measured: 3 + 0 + 13,960 = 13,963).
			u.PromptTokens = snap.TokenDetails.Input.TokenCount + u.CachedRead + u.CachedWrite
			if u.PromptTokens == 0 {
				// Fall back to the per-model usage block when tokenDetails is
				// absent on a future CLI.
				for _, m := range snap.ProviderModelMetrics {
					mu := m.Metrics.Usage
					if mu.InputTokens+mu.OutputTokens == 0 {
						continue
					}
					u.CachedRead, u.CachedWrite = mu.CacheReadTokens, mu.CacheWriteTokens
					u.CompletionTokens = mu.OutputTokens
					u.ReasoningTokens = mu.ReasoningTokens
					u.PromptTokens = mu.InputTokens + mu.CacheReadTokens + mu.CacheWriteTokens
					if u.Model == "" {
						u.Model = m.ModelID
					}
					break
				}
			}
			if snap.TotalPremiumRequestCost > 0 {
				u.PremiumRequests = snap.TotalPremiumRequestCost
			}
		case "result":
			if ev.SessionID != "" {
				res.SessionID = ev.SessionID
			}
			if ev.Usage.PremiumRequests > 0 {
				res.Usage.PremiumRequests = ev.Usage.PremiumRequests
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
		strings.Contains(msg, "unauthorized"):
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
	return env
}
