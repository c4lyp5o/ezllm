// Package registration runs ezllm's mandatory key test — the 7-step flow from
// docs/API.md and the M3 design doc.
//
// The load-bearing rule (learned the hard way, probe N1): GET /v1/models on
// opencode-go answers 200 + a full catalog for ANY string, so a catalog
// response can never prove a credential. The gate is the adapter's AuthOracle
// (GET /usage on both live providers: 401 bad key / 200 good, zero tokens),
// and even that oracle's 200 is double-checked with a deliberate bad-key
// calibration call — if the endpoint also answers 200 for garbage it is
// public, the oracle proves nothing, and the gate falls through to the
// inference step. A key is persisted only after a positive proof of use.
//
// This package never writes catalog/quota state itself: it returns evidence
// and the server decides whether to persist.
package registration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// Step names, stable — the UI maps them to chips and the 422 body names one.
const (
	StepFormat    = "format"
	StepCatalog   = "catalog"
	StepAuth      = "auth"
	StepQuota     = "quota"
	StepInference = "inference"
	StepProtocol  = "protocol"
)

// Defaults for the probe budget (design §2.3: 3 surfaces × 6 models max, never
// the full 36×3 = 108 inline calls a registration request would otherwise cost).
const (
	DefaultProbeMaxModels = 6
	defaultStepTimeout    = 15 * time.Second
)

// Step is one completed stage, echoed to the UI as it finishes.
type Step struct {
	Step   string `json:"step"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
	MS     int    `json:"ms"`
}

// Failure is a 422 key_test_failed, with the upstream's own words preserved.
type Failure struct {
	Step           string         `json:"step"`
	Message        string         `json:"message"`
	UpstreamStatus int            `json:"upstream_status,omitempty"`
	UpstreamError  map[string]any `json:"upstream_error,omitempty"`
	Hint           string         `json:"hint,omitempty"`
	// Retryable marks rate-limit / outage / unreachable failures: the key may
	// be perfectly fine, we simply could not prove it right now.
	Retryable bool `json:"retryable,omitempty"`
}

// Error lets a Failure travel as a Go error without losing its shape.
func (f *Failure) Error() string { return f.Step + ": " + f.Message }

// Options configures one key test.
type Options struct {
	Label         string
	APIKey        string
	SkipInference bool
	// ProbeMaxModels caps the protocol phase (0 → DefaultProbeMaxModels).
	ProbeMaxModels int
	// Surfaces scopes the protocol probe (empty → all three).
	Surfaces []provider.Surface
	// ProtocolInline runs the protocol phase synchronously — tests use it; the
	// HTTP handler defers it so registration answers in seconds rather than
	// the ~30s a paced 6-model × 3-surface sweep costs.
	ProtocolInline bool
}

// ProtocolResult reports the capability sweep.
type ProtocolResult struct {
	Deferred bool                        `json:"deferred"`
	Tested   int                         `json:"tested"`
	Support  map[string]int              `json:"support"` // openai/anthropic/responses counts
	Rows     map[string]map[string]*bool `json:"-"`       // model → surface → verdict
}

// TestResult is the evidence for one key test. OK==false always carries Fail.
type TestResult struct {
	OK      bool                 `json:"ok"`
	Steps   []Step               `json:"steps"`
	Fail    *Failure             `json:"fail,omitempty"`
	Catalog []provider.ModelInfo `json:"catalog,omitempty"`
	Quota   *provider.Quota      `json:"quota,omitempty"`
	Proto   *ProtocolResult      `json:"protocol,omitempty"`
	// Detail is the human summary stored in provider_keys.last_test_detail.
	Detail string `json:"detail"`

	authDeferred bool // oracle could not prove auth → inference must gate
	// catErr is set when an AUTHENTICATED catalog rejected this key (401/403).
	// Kept so a later step can quote the provider's own words instead of our
	// generic "could not verify" prose — see the no-model branch in Run.
	catErr error
}

// Tester runs key tests against real providers.
type Tester struct {
	client   *http.Client
	registry *provider.Registry
	db       *store.DB // optional: admin probes are ledgered so spend is visible
	// sleep is injectable so tests assert probe pacing without waiting on it.
	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time
}

// NewTester builds a Tester. db may be nil (tests, dry runs).
func NewTester(client *http.Client, registry *provider.Registry, db *store.DB) *Tester {
	return &Tester{
		client:   client,
		registry: registry,
		db:       db,
		sleep: func(ctx context.Context, d time.Duration) error {
			if d <= 0 {
				return nil
			}
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
		now: time.Now,
	}
}

// pacer enforces probe_delay_ms between network calls. Pacing is courtesy
// toward provider-side limits we have not measured — NOT a Cloudflare
// workaround (probe N9: the 1010 was a python-urllib User-Agent block, and Go
// was never affected).
type pacer struct {
	delay time.Duration
	last  time.Time
}

func (p *pacer) wait(ctx context.Context, sleep func(context.Context, time.Duration) error) error {
	if p.delay > 0 && !p.last.IsZero() {
		if gap := time.Since(p.last); gap < p.delay {
			if err := sleep(ctx, p.delay-gap); err != nil {
				return err
			}
		}
	}
	p.last = time.Now()
	return nil
}

// httpResult is one HTTP probe's (status, body) pair. Collapsing the pair into
// a struct is what lets the generic probe() below type-check: Go cannot infer a
// type parameter from a function returning three values.
type httpResult struct {
	status int
	body   []byte
}

// probe runs fn with its own timeout after honoring the pace. Pacing, timeout
// and cancellation live in one place so every network step behaves identically.
func probe[T any](
	ctx context.Context,
	t *Tester,
	pace *pacer,
	timeout time.Duration,
	fn func(context.Context) (T, error),
) (T, error) {
	var zero T
	if err := pace.wait(ctx, t.sleep); err != nil {
		return zero, err
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return fn(cctx)
}

// Run executes the key test against a real provider. It performs NO store
// writes: catalog/quota evidence is returned for the caller to persist after
// it has decided the test passed.
func (t *Tester) Run(ctx context.Context, acct provider.Account, opts Options) *TestResult {
	res := &TestResult{}
	adapter, err := t.registry.Get(acct.Kind)
	if err != nil {
		res.Fail = &Failure{Step: StepFormat, Message: err.Error()}
		return res
	}
	t.setAdapter(adapter) // threads the adapter into doProbe (provider quirks live in one place)

	key := strings.TrimSpace(opts.APIKey)
	pace := &pacer{delay: acct.ProbeDelay}

	// ── step 2: catalog — INFORMATIONAL. On opencode-go it answers 200 for
	// any key (N1/N2), so a failure here is recorded but never decides. ──
	models, catErr := probe(ctx, t, pace, defaultStepTimeout,
		func(c context.Context) ([]provider.ModelInfo, error) {
			return adapter.ListModels(c, acct, key)
		})
	switch {
	case catErr == nil:
		res.Catalog = models
		res.Steps = append(res.Steps, Step{Step: StepCatalog, OK: true,
			Detail: fmt.Sprintf("%d models", len(models))})
	case isAuthErr(catErr) && !adapter.CatalogIsPublic():
		// The catalog authenticates on this provider (ssn-gpt) and rejected
		// us. Record it, but keep going: the oracle is the designated gate,
		// and a scope mismatch must not reject a key that can actually infer.
		// We keep the error so the NO-MODEL branch can quote it if nothing
		// else ever gets a chance to check this key.
		res.catErr = catErr
		res.Steps = append(res.Steps, Step{Step: StepCatalog, OK: false,
			Detail: "rejected: " + shortErr(catErr)})
	default:
		res.Steps = append(res.Steps, Step{Step: StepCatalog, OK: false,
			Detail: "unavailable: " + shortErr(catErr)})
	}

	// ── step 3: auth — THE GATE. ──
	if !t.authGate(ctx, adapter, acct, key, pace, res) {
		res.Detail = "failed at " + res.Fail.Step
		return res
	}

	// ── step 4: quota — non-fatal by design: a provider with no /usage is
	// still a perfectly good provider. ──
	q, qErr := probe(ctx, t, pace, defaultStepTimeout,
		func(c context.Context) (provider.Quota, error) {
			return adapter.ReadQuota(c, acct, key)
		})
	switch {
	case qErr == nil && q.Kind != "":
		res.Quota = &q
		res.Steps = append(res.Steps, Step{Step: StepQuota, OK: true, Detail: quotaDetail(q)})
	case isAuthErr(qErr):
		// A credential rejection right after the oracle passed means the key
		// stopped working between calls — contradictory, so treat as fatal.
		f := authFailure(StepQuota, qErr, adapter)
		res.Fail = f
		res.Steps = append(res.Steps, Step{Step: StepQuota, OK: false, Detail: f.Message})
		res.Detail = "failed at quota"
		return res
	default:
		res.Steps = append(res.Steps, Step{Step: StepQuota, OK: false, Detail: "no quota endpoint"})
	}

	// ── step 5: inference — one real generation on the cheapest model.
	// Doubles as the credential gate when the oracle could not prove auth. ──
	if opts.SkipInference {
		// An oracle that 404s defers to inference — but the catalog itself can
		// gate, provided it DISCRIMINATES. Calibrate it the same way authGate
		// calibrates an oracle: one probe with a deliberately-wrong key. A
		// public catalog answers 200 to garbage (the N1 class) and proves
		// nothing; one that rejects the fake key has just proven the real key
		// we watched pass. Costs one extra GET, and only when we would
		// otherwise have to refuse the key outright.
		// res.catErr != nil means OUR key was rejected by the catalog — a fake
		// key being rejected too proves nothing (both keys failed).
		if res.authDeferred && res.catErr == nil && len(res.Catalog) > 0 {
			_, cerr := probe(ctx, t, pace, defaultStepTimeout,
				func(c context.Context) ([]provider.ModelInfo, error) {
					return adapter.ListModels(c, acct, fakeKey())
				})
			switch {
			case isAuthErr(cerr):
				res.authDeferred = false
				res.Steps = append(res.Steps, Step{Step: StepAuth, OK: true,
					Detail: "catalog rejected a fake key — the real key is proven"})
			case cerr == nil:
				// The public-catalog case (N1): 200 for garbage proves nothing.
				res.Steps = append(res.Steps, Step{Step: StepAuth, OK: false,
					Detail: "catalog answered 200 for a fake key — public, no proof"})
			default:
				res.Steps = append(res.Steps, Step{Step: StepAuth, OK: false,
					Detail: "catalog " + httpStatusWord(statusOf(cerr)) + " for a fake key — inconclusive"})
			}
		}
		if res.authDeferred {
			if res.catErr != nil {
				res.Fail = catalogFailure(res.catErr)
				res.Detail = "failed at " + res.Fail.Step
				return res
			}
			res.Fail = &Failure{
				Step:    StepAuth,
				Message: "cannot verify this key: the provider's credential endpoints answer 200 for any string, and the inference probe was skipped",
				Hint:    "Re-run with 'Test & add' and leave the inference probe on — a key that is never proven is a key we must not store.",
			}
			res.Steps = append(res.Steps, Step{Step: StepInference, OK: false, Detail: "skipped — auth unproven"})
			res.Detail = "failed: auth unproven"
			return res
		}
		res.Steps = append(res.Steps, Step{Step: StepInference, OK: true, Detail: "skipped"})
	} else if model := pickProbeModel(res.Catalog, acct.Kind); model == "" {
		if res.authDeferred {
			// If the authenticated catalog already said no, THAT is the
			// provider's verdict — lead with its own words, not our prose.
			if res.catErr != nil {
				res.Fail = catalogFailure(res.catErr)
				res.Detail = "failed at " + res.Fail.Step
				return res
			}
			res.Fail = &Failure{
				Step:    StepAuth,
				Message: "cannot verify this key: no credential-checking endpoint and no model to probe with",
				Hint:    "This gateway exposes neither /usage nor a catalog. Check base_url.",
			}
			res.Detail = "failed: auth unproven"
			return res
		}
		res.Steps = append(res.Steps, Step{Step: StepInference, OK: true, Detail: "no model to probe"})
	} else if ok, f := t.inferenceStep(ctx, adapter, acct, key, model, pace, res); !ok {
		res.Fail = f
		res.Detail = "failed at " + f.Step
		return res
	}

	// ── step 6: protocol — capability DATA, never a gate. ──
	if opts.ProtocolInline {
		p := t.ProtocolSweep(ctx, acct, key, res.Catalog, opts)
		res.Proto = p
		res.Steps = append(res.Steps, Step{Step: StepProtocol, OK: true,
			Detail: fmt.Sprintf("tested %d models", p.Tested)})
	} else {
		res.Proto = &ProtocolResult{Deferred: true, Support: map[string]int{}}
		res.Steps = append(res.Steps, Step{Step: StepProtocol, OK: true,
			Detail: "deferred (runs after this response)"})
	}

	res.OK = true
	res.Detail = summarize(res)
	return res
}

// authGate runs the oracle + calibration. Returns true when auth is proven,
// or false after filling res.Fail (hard rejection / unreachable).
// Sets res.authDeferred when no usable oracle exists and inference must gate.
func (t *Tester) authGate(
	ctx context.Context,
	adapter provider.Adapter,
	acct provider.Account,
	key string,
	pace *pacer,
	res *TestResult,
) bool {
	oracle := adapter.AuthOracle()

	var (
		status  int
		body    []byte
		authErr error
	)
	// Attempt up to 3× on 429/5xx/timeout. 401/403 stop immediately — on
	// these providers 403 means bad credentials, not "slow down", so backing
	// off would only delay the user's error message.
	for attempt := 0; attempt < 3; attempt++ {
		var ores httpResult
		ores, authErr = probe(ctx, t, pace, defaultStepTimeout,
			func(c context.Context) (httpResult, error) {
				s, b, err := doProbe(c, t.client, oracle, acct, key)
				return httpResult{status: s, body: b}, err
			})
		status, body = ores.status, ores.body
		if authErr == nil || !retryableStatus(status) || ctx.Err() != nil {
			break
		}
		if err := t.sleep(ctx, backoff(attempt)); err != nil {
			break
		}
	}

	switch {
	case authErr == nil && status >= 200 && status < 300:
		// The oracle says yes — but does it ever say no? Calibration: one
		// deliberately-invalid key. If garbage also passes, the endpoint is
		// public (the N1 class of bug) and this 200 proves nothing.
		calRes, calErr := probe(ctx, t, pace, defaultStepTimeout,
			func(c context.Context) (httpResult, error) {
				s, b, err := doProbe(c, t.client, oracle, acct, fakeKey())
				return httpResult{status: s, body: b}, err
			})
		calStatus := calRes.status
		switch {
		case calErr == nil && calStatus >= 200 && calStatus < 300:
			res.Steps = append(res.Steps, Step{Step: StepAuth, OK: false,
				Detail: "oracle public (200 for any key) — deferring to inference"})
			res.authDeferred = true
			return true // not a failure yet: inference may still gate
		case isAuthRejectFor(adapter, calStatus):
			res.Steps = append(res.Steps, Step{Step: StepAuth, OK: true,
				Detail: "oracle 200, calibration rejected the fake key"})
			return true
		default:
			// Calibration inconclusive (outage/429): we cannot confirm the
			// endpoint discriminates, so we cannot claim proof from it.
			res.Steps = append(res.Steps, Step{Step: StepAuth, OK: false,
				Detail: "oracle inconclusive (calibration: " + httpStatusWord(calStatus) + ") — deferring to inference"})
			res.authDeferred = true
			return true
		}

	case isAuthRejectFor(adapter, status):
		// The provider itself said no. This is THE invalid-key signal.
		f := authFailure(StepAuth, probeStatusError(status, body), adapter)
		res.Fail = f
		res.Steps = append(res.Steps, Step{Step: StepAuth, OK: false, Detail: f.Message})
		return false

	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		res.Steps = append(res.Steps, Step{Step: StepAuth, OK: false,
			Detail: "no oracle endpoint — deferring to inference"})
		res.authDeferred = true
		return true

	case retryableStatus(status):
		// Rate-limited or broken upstream after retries: honest failure with a
		// retryable flag. Never claim a pass we did not prove.
		f := &Failure{
			Step:           StepAuth,
			Message:        "could not verify key right now: " + shortErr(authErr),
			UpstreamStatus: status,
			Hint:           "The provider is rate-limiting or unreachable. Wait a moment and retest — the key may be fine.",
			Retryable:      true,
		}
		res.Fail = f
		res.Steps = append(res.Steps, Step{Step: StepAuth, OK: false, Detail: f.Message})
		return false

	default:
		f := &Failure{
			Step:           StepAuth,
			Message:        "could not reach the credential endpoint: " + shortErr(authErr),
			UpstreamStatus: status,
			Hint:           "Check base_url and that the provider is reachable from this host.",
			Retryable:      true,
		}
		res.Fail = f
		res.Steps = append(res.Steps, Step{Step: StepAuth, OK: false, Detail: f.Message})
		return false
	}
}

// inferenceStep performs one real generation. When auth is still unproven this
// call IS the gate: 401/403 here rejects the key.
func (t *Tester) inferenceStep(
	ctx context.Context,
	adapter provider.Adapter,
	acct provider.Account,
	key, model string,
	pace *pacer,
	res *TestResult,
) (bool, *Failure) {
	surface := provider.SurfaceOpenAI
	if acct.Kind == provider.KindAnthropicCompat {
		surface = provider.SurfaceAnthropic
	}
	probeSpec := provider.AuthProbe{
		Method: http.MethodPost,
		Path:   provider.SurfacePath(surface),
		Body:   fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"ping"}]}`, model),
	}

	var (
		status  int
		resp    []byte
		err     error
		elapsed time.Duration
	)
	for attempt := 0; attempt < 2; attempt++ {
		start := t.now()
		var ires httpResult
		ires, err = probe(ctx, t, pace, defaultStepTimeout,
			func(c context.Context) (httpResult, error) {
				s, b, e := doProbe(c, t.client, probeSpec, acct, key)
				return httpResult{status: s, body: b}, e
			})
		status, resp = ires.status, ires.body
		elapsed = t.now().Sub(start)
		if err == nil || !retryableStatus(status) {
			break
		}
		if serr := t.sleep(ctx, backoff(attempt)); serr != nil {
			break
		}
	}

	// Ledger admin probes: real token spend must never be invisible.
	total := msAtLeastOne(elapsed)
	t.ledgerCall(store.Call{
		TS:      t.now().UTC(),
		Client:  "admin:test",
		Surface: storeSurface(surface),
		Alias:   acct.Namespace + "/" + model,
		Account: acct.Namespace,
		Model:   model,
		Status:  status,
		Totalms: &total,
	})

	switch {
	case err == nil && status >= 200 && status < 300 && isJSON(resp):
		res.Steps = append(res.Steps, Step{Step: StepInference, OK: true,
			Detail: fmt.Sprintf("%s · %dms", model, msAtLeastOne(elapsed))})
		return true, nil

	case err == nil && status >= 200 && status < 300:
		f := &Failure{
			Step:    StepInference,
			Message: "provider returned a non-JSON 200 (captive portal or HTML error page?)",
			Hint:    "The credential was not proven. Check base_url — an interstitial page suggests the URL is wrong.",
		}
		res.Steps = append(res.Steps, Step{Step: StepInference, OK: false, Detail: f.Message})
		return false, f

	case isAuthStatus(status):
		f := authFailure(StepInference, probeStatusError(status, resp), adapter)
		if res.authDeferred {
			f.Hint = "The provider's catalog/usage endpoints are public, so this generation call was the only credential check. It was rejected: the key is invalid."
		}
		res.Steps = append(res.Steps, Step{Step: StepInference, OK: false, Detail: f.Message})
		return false, f

	default:
		f := &Failure{
			Step:           StepInference,
			Message:        "generation probe failed: " + shortErr(err),
			UpstreamStatus: status,
			UpstreamError:  upstreamErr(resp),
			Hint:           "The provider rejected a minimal test request. The response above is the provider's own explanation.",
			Retryable:      retryableStatus(status),
		}
		res.Steps = append(res.Steps, Step{Step: StepInference, OK: false, Detail: f.Message})
		return false, f
	}
}

// ProtocolSweep probes surfaces × sampled models and classifies each response.
// Exposed because the HTTP layer runs it in the background after responding.
//
// Rules (design §2.3): 200 → supported; protocol-shaped 400/404 → unsupported;
// 401/403 → ABORT (the key died mid-probe — recording 0 would be a lie);
// 429/5xx/timeout → unknown (NULL) and stop, rather than hammering.
func (t *Tester) ProtocolSweep(
	ctx context.Context,
	acct provider.Account,
	key string,
	models []provider.ModelInfo,
	opts Options,
) *ProtocolResult {
	out := &ProtocolResult{
		Support: map[string]int{},
		Rows:    map[string]map[string]*bool{},
	}
	adapter, err := t.registry.Get(acct.Kind)
	if err != nil {
		return out
	}
	t.setAdapter(adapter)

	maxModels := opts.ProbeMaxModels
	if maxModels <= 0 {
		maxModels = DefaultProbeMaxModels
	}
	sample := sampleModels(models, maxModels)
	surfaces := opts.Surfaces
	if len(surfaces) == 0 {
		surfaces = []provider.Surface{
			provider.SurfaceOpenAI, provider.SurfaceAnthropic, provider.SurfaceResponses,
		}
	}
	pace := &pacer{delay: acct.ProbeDelay}
	counts := map[provider.Surface]int{}

	for _, m := range sample {
		if ctx.Err() != nil {
			break
		}
		abort := false
		for _, s := range surfaces {
			if ctx.Err() != nil {
				abort = true
				break
			}
			verdict, authDead := t.probeSurface(ctx, adapter, acct, key, m.ID, s, pace)
			if authDead {
				abort = true // the key went bad; stop, record nothing misleading
				break
			}
			var ptr *bool
			switch verdict {
			case provider.VerdictSupported:
				v := true
				ptr = &v
				counts[s]++
			case provider.VerdictUnsupported:
				v := false
				ptr = &v
			}
			if out.Rows[m.ID] == nil {
				out.Rows[m.ID] = map[string]*bool{}
			}
			out.Rows[m.ID][string(s)] = ptr
			t.setProto(acct.ID, m.ID, s, ptr)
		}
		out.Tested++
		if abort {
			break
		}
	}

	for _, s := range surfaces {
		out.Support[string(s)] = counts[s]
	}
	return out
}

// probeSurface classifies one (model, surface) pair. authDead reports a
// credential failure so the caller can abort the whole sweep.
func (t *Tester) probeSurface(
	ctx context.Context,
	adapter provider.Adapter,
	acct provider.Account,
	key, model string,
	surface provider.Surface,
	pace *pacer,
) (provider.ProbeVerdict, bool) {
	var body string
	if surface == provider.SurfaceResponses {
		body = fmt.Sprintf(`{"model":%q,"max_output_tokens":8,"input":"ping"}`, model)
	} else {
		body = fmt.Sprintf(`{"model":%q,"max_tokens":8,"messages":[{"role":"user","content":"ping"}]}`, model)
	}
	spec := provider.AuthProbe{Method: http.MethodPost, Path: provider.SurfacePath(surface)}

	pres, err := probe(ctx, t, pace, defaultStepTimeout,
		func(c context.Context) (httpResult, error) {
			s, b, e := doProbe(c, t.client, spec, acct, key, withBody(body))
			return httpResult{status: s, body: b}, e
		})
	status, resp := pres.status, pres.body
	if err != nil {
		// Transport failure → unknown, never "unsupported".
		return provider.VerdictUnknown, false
	}
	return provider.ClassifyProbeResult(status, string(resp))
}

// ── probe transport ─────────────────────────────────────────────────────────

// bodyOverride lets a caller supply a POST body without changing AuthProbe.
type bodyOverride struct{ body string }

func withBody(b string) *bodyOverride { return &bodyOverride{body: b} }

// doProbe performs one HTTP probe with the adapter's auth semantics: Bearer vs
// x-api-key, per-request session ids, custom headers — all in one place.
func doProbe(
	ctx context.Context,
	client *http.Client,
	spec provider.AuthProbe,
	acct provider.Account,
	key string,
	body ...*bodyOverride,
) (int, []byte, error) {
	method := spec.Method
	if method == "" {
		method = http.MethodGet
	}
	url := provider.UpstreamURL(acct.BaseURL, acct.Kind, spec.Path)

	payload := spec.Body
	if len(body) > 0 && body[0] != nil && body[0].body != "" {
		payload = body[0].body
	}
	var rd io.Reader
	if payload != "" {
		rd = strings.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return 0, nil, err
	}
	if payload != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := currentAdapter.PrepareRequest(ctx, req, acct, key, surfaceFor(spec.Path)); err != nil {
		return 0, nil, err
	}
	for k, v := range acct.CustomHeaders {
		if k != "" && v != "" {
			req.Header.Set(k, v)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, &probeError{status: 0, raw: err.Error()}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, b, nil
}

// currentAdapter is set per Run/Sweep. Tests substitute fakes by calling
// SetAdapterForTest. Single-flight by construction: one Tester per request.
var currentAdapter provider.Adapter

func (t *Tester) setAdapter(a provider.Adapter) { currentAdapter = a }

// SetAdapterForTest exposes the indirection so unit tests can inject fakes
// without an http.Client.
func SetAdapterForTest(a provider.Adapter) { currentAdapter = a }

// surfaceFor picks the wire protocol implied by the probe path.
func surfaceFor(path string) provider.Surface {
	switch {
	case strings.HasSuffix(path, "/messages"):
		return provider.SurfaceAnthropic
	case strings.HasSuffix(path, "/responses"):
		return provider.SurfaceResponses
	default:
		return provider.SurfaceOpenAI
	}
}

// ── step 1: format ──────────────────────────────────────────────────────────

// ── error shaping ───────────────────────────────────────────────────────────

// authFailure builds the canonical 422 for a credential rejection, appending
// the N1 lesson as a hint when the provider's catalog is public.
func authFailure(step string, err error, adapter provider.Adapter) *Failure {
	f := &Failure{
		Step:    step,
		Message: "provider rejected this key",
		Hint:    "The credential endpoint returned 401/403.",
	}
	var ps *probeError
	if errors.As(err, &ps) {
		f.UpstreamStatus = ps.status
		f.UpstreamError = ps.body
		if msg := upstreamMessage(ps.body); msg != "" {
			f.Message = msg
		}
	}
	if adapter != nil && adapter.CatalogIsPublic() {
		f.Hint = fmt.Sprintf(
			"%s's GET /v1/models succeeds for ANY key, so a working catalog does not prove the key is valid. This key failed the usage-oracle check.",
			adapter.Kind())
	}
	return f
}

// probeError carries an HTTP status + parsed upstream body out of doProbe.
type probeError struct {
	status int
	body   map[string]any
	raw    string
}

func (e *probeError) Error() string {
	if msg := upstreamMessage(e.body); msg != "" {
		return fmt.Sprintf("HTTP %d: %s", e.status, msg)
	}
	if e.status == 0 {
		return e.raw
	}
	return fmt.Sprintf("HTTP %d: %s", e.status, truncate(e.raw, 200))
}

func probeStatusError(status int, raw []byte) error {
	return &probeError{status: status, body: upstreamErr(raw), raw: string(raw)}
}

func isAuthErr(err error) bool {
	return isAuthStatus(statusOf(err))
}

// catalogFailure turns an authenticated catalog's 401/403 into the failure we
// report — the provider's own words, its status, and an honest hint about why
// nothing else could check the key.
func catalogFailure(err error) *Failure {
	f := &Failure{
		Step:    StepCatalog,
		Message: "the catalog rejected this key: " + shortErr(err),
		Hint:    "Nothing else could check this key (no oracle endpoint, and the inference probe had no model or was skipped). Verify the key and the base_url.",
	}
	var ps *probeError
	if errors.As(err, &ps) {
		f.UpstreamStatus = ps.status
	}
	return f
}

// statusOf extracts the HTTP status from an error: 0 when the request never
// got a response (connection refused, bad URL). Understands both probeError
// (the oracle path) and provider.HTTPError (the adapter path) — adapter
// errors used to arrive as bare fmt.Errorf, which made every 401 from a
// catalog look like a transport failure.
func statusOf(err error) int {
	var ps *probeError
	if errors.As(err, &ps) {
		return ps.status
	}
	var sc interface{ StatusCode() int }
	if errors.As(err, &sc) {
		return sc.StatusCode()
	}
	return 0
}

func isAuthStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// isAuthRejectFor is isAuthStatus widened by the adapter's own reject codes
// (AuthRejectStatuser). Adapters without the interface keep 401/403 only.
func isAuthRejectFor(adapter provider.Adapter, status int) bool {
	if isAuthStatus(status) {
		return true
	}
	if r, ok := adapter.(provider.AuthRejectStatuser); ok {
		for _, s := range r.AuthRejectStatuses() {
			if s == status {
				return true
			}
		}
	}
	return false
}

// retryableStatus: 401/403 are NOT retryable — they mean bad credentials, and
// retrying only delays the user's error (probe N9/N10 lesson). status 0 is a
// transport failure (DNS/conn refused/timeout), which is worth one retry.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, 0:
		return true
	default:
		return status >= 500
	}
}

func httpStatusWord(status int) string {
	if status == 0 {
		return "transport error"
	}
	return fmt.Sprintf("HTTP %d", status)
}

func backoff(attempt int) time.Duration {
	d := time.Second << attempt
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

// upstreamErr normalizes every error body shape we have seen into one map:
//
//	opencode/anthropic  {"type":"error","error":{"type":…,"message":…}}
//	openai-style        {"error":{"message":…,"type":…}}
//	ssn-gpt             {"code":…,"message":…}
func upstreamErr(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{"message": truncate(string(raw), 300)}
	}
	if e, ok := m["error"].(map[string]any); ok {
		return e
	}
	if _, hasMsg := m["message"]; hasMsg {
		if _, hasCode := m["code"]; hasCode {
			m["type"] = m["code"]
		}
		return m
	}
	return map[string]any{"message": truncate(string(raw), 300)}
}

func upstreamMessage(m map[string]any) string {
	if m == nil {
		return ""
	}
	if s, ok := m["message"].(string); ok && s != "" {
		return s
	}
	return ""
}

// ── model selection ─────────────────────────────────────────────────────────

// pickProbeModel chooses the cheapest catalog model for the inference probe:
// known-cheap first (deepseek-v4.1-flash measured 1689ms / 31 prompt tokens on
// opencode-go), then name patterns, then the head of the catalog. Reasoning
// models ignore max_tokens:1 anyway (qwen returned 25 completion tokens), so
// cheap-by-prompt is what actually matters.
func pickProbeModel(models []provider.ModelInfo, kind provider.Kind) string {
	if len(models) == 0 {
		if kind == provider.KindOpenCodeGo {
			return "deepseek-v4.1-flash" // verified cheap + present on this kind
		}
		return ""
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	for _, k := range []string{"deepseek-v4.1-flash", "qwen3.8-flash", "minimax-m3"} {
		for _, id := range ids {
			if id == k {
				return id
			}
		}
	}
	for _, p := range []string{"flash", "mini", "haiku", "nano", "lite", "free", "instant", "small"} {
		for _, id := range ids {
			if strings.Contains(strings.ToLower(id), p) {
				return id
			}
		}
	}
	sort.Strings(ids) // deterministic fallback
	return ids[0]
}

// sampleModels picks the protocol-probe sample: cheap-looking models first,
// then the rest of the catalog, deduped and capped.
func sampleModels(models []provider.ModelInfo, max int) []provider.ModelInfo {
	if max <= 0 || len(models) == 0 {
		return nil
	}
	if len(models) <= max {
		return models
	}
	cheap := make([]provider.ModelInfo, 0, max)
	rest := make([]provider.ModelInfo, 0, len(models))
	for _, m := range models {
		low := strings.ToLower(m.ID)
		hit := false
		for _, p := range []string{"flash", "mini", "haiku", "nano", "lite", "free", "instant"} {
			if strings.Contains(low, p) {
				hit = true
				break
			}
		}
		if hit && len(cheap) < max {
			cheap = append(cheap, m)
		} else {
			rest = append(rest, m)
		}
	}
	out := cheap
	for _, m := range rest {
		if len(out) >= max {
			break
		}
		out = append(out, m)
	}
	return out
}

// ── formatting ──────────────────────────────────────────────────────────────

func quotaDetail(q provider.Quota) string {
	switch q.Kind {
	case provider.QuotaPercent:
		var parts []string
		if q.PercentWeekly != nil {
			parts = append(parts, fmt.Sprintf("weekly %.0f%%", *q.PercentWeekly))
		}
		if q.PercentMonthly != nil {
			parts = append(parts, fmt.Sprintf("monthly %.0f%%", *q.PercentMonthly))
		}
		return "percent " + strings.Join(parts, " ")
	case provider.QuotaMoney:
		unit := q.BalanceUnit
		if unit == "" {
			unit = "USD"
		}
		bal := 0.0
		if q.Balance != nil {
			bal = *q.Balance
		}
		return fmt.Sprintf("money balance=%.4f %s", bal, unit)
	case provider.QuotaNone:
		return "none"
	default:
		return string(q.Kind)
	}
}

// summarize composes provider_keys.last_test_detail — the one-line verdict a
// human reads on the providers page.
func summarize(res *TestResult) string {
	var b strings.Builder
	b.WriteString("ok")
	for _, s := range res.Steps {
		if s.Detail == "" {
			continue
		}
		fmt.Fprintf(&b, " · %s %s", s.Step, s.Detail)
	}
	if res.Proto != nil && !res.Proto.Deferred && res.Proto.Tested > 0 {
		var counts []string
		for _, k := range []string{"openai", "anthropic", "responses"} {
			if n, ok := res.Proto.Support[k]; ok {
				counts = append(counts, fmt.Sprintf("%s %d", k, n))
			}
		}
		if len(counts) > 0 {
			fmt.Fprintf(&b, " · protocol %s", strings.Join(counts, "/"))
		}
	}
	return b.String()
}

// ── small utilities ─────────────────────────────────────────────────────────

func fakeKey() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return "sk-ezllm-invalid-" + hex.EncodeToString(buf)
}

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	return truncate(err.Error(), 160)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func isJSON(b []byte) bool {
	var v any
	return json.Unmarshal(b, &v) == nil
}

func msAtLeastOne(d time.Duration) int64 {
	ms := d.Milliseconds()
	if ms < 1 {
		return 1 // sub-ms first bytes must not read as "unmeasured"
	}
	return ms
}

func storeSurface(s provider.Surface) store.Surface {
	switch s {
	case provider.SurfaceAnthropic:
		return store.SurfaceAnthropic
	case provider.SurfaceResponses:
		return store.SurfaceResponses
	default:
		return store.SurfaceOpenAI
	}
}

// ledgerCall records an admin probe so its token spend appears in dashboards.
func (t *Tester) ledgerCall(c store.Call) {
	if t.db != nil {
		t.db.RecordCall(c)
	}
}

// setProto persists one (model, surface) verdict: true → 1, false → 0,
// nil → NULL (untested — a first-class state the UI renders as "?").
func (t *Tester) setProto(accountID int64, modelID string, s provider.Surface, supported *bool) {
	if t.db == nil {
		return
	}
	_ = t.db.SetModelProto(context.Background(), accountID, modelID, s, supported)
}
