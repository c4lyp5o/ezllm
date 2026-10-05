// Package router resolves a client's requested model into a concrete Route.
//
// M2 scope: DIRECT namespace resolution only — "<namespace>/<model>" against a
// registered account. Combos, strategies, key pools, cooldowns and caps land in
// M4; the Resolver interface is already shaped so M4 replaces the implementation
// without touching the handlers.
package router

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// ErrNotFound is returned when a model string cannot be resolved. The message
// carries the hints (which namespaces/combos DO serve it) so the 404 is useful.
type ErrNotFound struct {
	Requested string
	Hints     []string
}

func (e *ErrNotFound) Error() string {
	if len(e.Hints) == 0 {
		return fmt.Sprintf("no provider serves %q", e.Requested)
	}
	return fmt.Sprintf("no provider serves %q — available: %s", e.Requested, strings.Join(e.Hints, ", "))
}

// Catalog is the read-only view of registered accounts/keys/models that the
// resolver needs. Implemented by the store layer.
type Catalog interface {
	// AccountByNamespace returns the enabled account for a namespace.
	AccountByNamespace(ctx context.Context, ns string) (*provider.Account, error)
	// PickKey returns a decrypted key eligible for the account (M2: the first
	// enabled key; M4 adds rotation/cooldown/quota exclusion).
	PickKey(ctx context.Context, accountID int64) (store.KeyPick, error)
	// ModelExists reports whether the account's catalog contains modelID.
	ModelExists(ctx context.Context, accountID int64, modelID string) (bool, error)
	// AllNamespaces lists namespace/model pairs for helpful 404s.
	AllNamespaces(ctx context.Context) ([]string, error)
}

// Resolver turns a requested model string into a Route.
type Resolver struct {
	catalog Catalog
}

// New builds a Resolver.
func New(c Catalog) *Resolver { return &Resolver{catalog: c} }

// Resolve implements proxy.Rewriter.
//
// Resolution order (per plan §1.1):
//  1. combo name (M4) — not implemented in M2
//  2. "<namespace>/<model>" → direct route to that account+model
//  3. otherwise → 404 with hints listing what DOES serve that model
//
// There is deliberately NO default account: a bare model id is ambiguous
// (the same model id can exist on several accounts), and guessing would
// mis-attribute spend.
func (r *Resolver) Resolve(ctx context.Context, requested string, surface provider.Surface, client string) (proxy.Route, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return proxy.Route{}, errors.New("router: model is required")
	}

	ns, model, hasSlash := splitNamespace(requested)
	if !hasSlash {
		hints, _ := r.catalog.AllNamespaces(ctx)
		return proxy.Route{}, &ErrNotFound{
			Requested: requested,
			Hints:     filterHints(hints, requested),
		}
	}

	acct, err := r.catalog.AccountByNamespace(ctx, ns)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Hint on the MODEL, not the whole "ns/model" string: the namespace
			// is what was wrong, so matching against "nope/gpt-6-luna" would
			// find nothing and leave the user with a bare 404.
			hints, _ := r.catalog.AllNamespaces(ctx)
			return proxy.Route{}, &ErrNotFound{Requested: requested, Hints: filterHints(hints, model)}
		}
		return proxy.Route{}, err
	}
	if !acct.Enabled {
		return proxy.Route{}, fmt.Errorf("router: account %q is disabled", ns)
	}

	// Surface capability is enforced in M4 via the probed models table; in M2 we
	// still verify the model exists in the account's catalog so a typo 404s
	// rather than burning an upstream call.
	if ok, err := r.catalog.ModelExists(ctx, acct.ID, model); err == nil && !ok {
		hints, _ := r.catalog.AllNamespaces(ctx)
		return proxy.Route{}, &ErrNotFound{
			Requested: requested,
			Hints:     filterHints(hints, model),
		}
	}

	key, err := r.catalog.PickKey(ctx, acct.ID)
	if err != nil {
		return proxy.Route{}, fmt.Errorf("router: no usable key for %q: %w", ns, err)
	}

	return proxy.Route{
		Account:  *acct,
		KeyID:    key.ID,
		KeyPlain: key.Plaintext,
		KeyHint:  key.Hint,
		Model:    model,
		Alias:    requested,
		Surface:  surface,
	}, nil
}

// splitNamespace splits "ns/model" into its parts. A model id may itself contain
// dots but not slashes, so we split on the FIRST slash only.
func splitNamespace(s string) (ns, model string, ok bool) {
	i := strings.Index(s, "/")
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// filterHints keeps only hints that mention the requested model, so the 404
// suggests real alternatives instead of dumping the whole catalog.
func filterHints(hints []string, model string) []string {
	if model == "" {
		return hints
	}
	var out []string
	for _, h := range hints {
		if strings.Contains(h, model) {
			out = append(out, h)
		}
	}
	if len(out) == 0 {
		return nil
	}
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}
