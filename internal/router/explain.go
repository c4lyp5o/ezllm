package router

import (
	"context"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
)

// ExplainHop is one hop's verdict in a route explanation: usable now, or why
// not, read from the same gates real resolution applies.
type ExplainHop struct {
	AccountID int64  `json:"account_id"`
	Namespace string `json:"namespace"`
	Model     string `json:"model"`
	// Status is "candidate" (it would be tried), "skipped" (dropped, the
	// combo carries on) or "refused" (the whole request fails on it).
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	// Wire is the API this hop would be dialed on; Translated means the body
	// is converted from the client's API to reach it.
	Wire       string `json:"wire,omitempty"`
	Translated bool   `json:"translated,omitempty"`
	// Reasoning is what the model is known to accept for reasoning_effort.
	ReasoningAllowed  []string `json:"reasoning_allowed,omitempty"`
	ReasoningRequired bool     `json:"reasoning_required,omitempty"`
	// MaxContextTokens is the account's declared context window; 0 = none.
	MaxContextTokens int64  `json:"max_context_tokens,omitempty"`
	Kind             string `json:"kind,omitempty"`
	// KeyCount is how many usable keys the account has. Each is tried in
	// turn, and the hop counts as parked only when every key is.
	KeyCount int `json:"key_count,omitempty"`
}

// Explanation is a dry run of one routing decision: nothing is dialed, no
// strategy cursor moves, no rule counts.
type Explanation struct {
	Requested string       `json:"requested"`
	Kind      string       `json:"kind"` // "combo" or "direct"
	Strategy  string       `json:"strategy,omitempty"`
	Hops      []ExplainHop `json:"hops"`
	// Error is what a real request would get when nothing can serve it.
	Error string `json:"error,omitempty"`
}

type explainKey struct{}

// explainer collects hop verdicts during one traced resolution.
type explainer struct{ hops []ExplainHop }

func explainerFrom(ctx context.Context) *explainer {
	e, _ := ctx.Value(explainKey{}).(*explainer)
	return e
}

// note records one hop's verdict when the resolution is traced; a plain
// request carries no explainer and pays one nil check.
func (e *explainer) note(h ExplainHop) {
	if e != nil {
		e.hops = append(e.hops, h)
	}
}

func candidateHop(rt proxy.Route) ExplainHop {
	wire := rt.Surface
	if rt.WireSurface != "" {
		wire = rt.WireSurface
	}
	return ExplainHop{
		AccountID: rt.Account.ID, Namespace: rt.Account.Namespace, Model: rt.Model, Status: "candidate",
		Wire: string(wire), Translated: rt.Translated || rt.WireSurface != "",
		ReasoningAllowed: rt.Reasoning.Allowed, ReasoningRequired: rt.Reasoning.Required,
		MaxContextTokens: rt.Account.MaxContextTokens, Kind: string(rt.Account.Kind),
	}
}

// Explain runs the resolution for requested on surface without dispatching
// anything, and reports every hop's verdict. Combo hops come back in the
// combo's own order: the strategy reorders the candidates per request, and
// running it here would advance round-robin cursors.
func (r *Resolver) Explain(ctx context.Context, requested string, surface provider.Surface) Explanation {
	ex := &explainer{}
	ctx = context.WithValue(ctx, explainKey{}, ex)
	out := Explanation{Requested: strings.TrimSpace(requested), Kind: "combo"}
	if _, _, slash := splitNamespace(out.Requested); slash {
		out.Kind = "direct"
	}
	routes, err := r.ResolveCandidates(ctx, requested, surface, "")
	if out.Kind == "combo" {
		if c, cerr := r.catalog.ComboByName(ctx, out.Requested); cerr == nil {
			out.Strategy = c.Strategy
		}
	}
	if out.Kind == "direct" {
		// A direct route is one hop: its verdict is the resolution's.
		ns, model, _ := splitNamespace(out.Requested)
		if err != nil {
			ex.note(ExplainHop{Namespace: ns, Model: model, Status: "refused", Reason: err.Error()})
		} else {
			for _, rt := range routes {
				h := candidateHop(rt)
				h.KeyCount = r.keyCount(ctx, rt.Account.ID)
				ex.note(h)
			}
		}
	}
	if err != nil {
		out.Error = err.Error()
	}
	out.Hops = ex.hops
	if out.Hops == nil {
		out.Hops = []ExplainHop{}
	}
	return out
}

// explaining reports whether ctx carries a trace (and so must not move any
// strategy state).
func explaining(ctx context.Context) bool { return explainerFrom(ctx) != nil }
