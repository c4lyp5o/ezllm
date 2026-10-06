package router

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// fakeCatalog is an in-memory Catalog so resolution rules are testable without
// a database.
type fakeCatalog struct {
	accounts   map[string]*provider.Account // by namespace
	keys       map[int64]store.KeyPick
	models     map[int64]map[string]bool
	namespaces []string

	pickKeyErr error
	combos     map[string]*store.Combo // by name
}

func newFake() *fakeCatalog {
	a1 := &provider.Account{ID: 1, Name: "opencode #1", Namespace: "opengo",
		Kind: provider.KindOpenCodeGo, BaseURL: "https://opencode.ai/zen/go/v1", Enabled: true}
	a2 := &provider.Account{ID: 2, Name: "SSN GPT", Namespace: "super-ssn",
		Kind: provider.KindOpenAICompatible, BaseURL: "https://space.stationine.com/v1", Enabled: true}
	a3 := &provider.Account{ID: 3, Name: "disabled acct", Namespace: "off",
		Kind: provider.KindOpenAICompatible, BaseURL: "https://x/v1", Enabled: false}
	return &fakeCatalog{
		accounts: map[string]*provider.Account{"opengo": a1, "super-ssn": a2, "off": a3},
		keys: map[int64]store.KeyPick{
			1: {ID: 11, Hint: "sk…cdef", Plaintext: "sk-opencode-secret"},
			2: {ID: 22, Hint: "sk…wxyz", Plaintext: "sk-ssn-secret"},
		},
		models: map[int64]map[string]bool{
			1: {"qwen3.8-flash": true, "gpt-6-luna": true, "omen-alpha": true},
			2: {"gpt-6-luna": true, "gpt-6.1-sol": true},
		},
		namespaces: []string{"opengo/qwen3.8-flash", "opengo/gpt-6-luna", "opengo/omen-alpha",
			"super-ssn/gpt-6-luna", "super-ssn/gpt-6.1-sol"},
	}
}

func (f *fakeCatalog) AccountByNamespace(_ context.Context, ns string) (*provider.Account, error) {
	if a, ok := f.accounts[ns]; ok {
		return a, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeCatalog) AccountByID(_ context.Context, id int64) (*provider.Account, error) {
	for _, a := range f.accounts {
		if a.ID == id {
			return a, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeCatalog) ComboByName(_ context.Context, name string) (*store.Combo, error) {
	if f.combos == nil {
		return nil, store.ErrNotFound
	}
	if c, ok := f.combos[name]; ok {
		cp := *c
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeCatalog) AllComboNames(_ context.Context) ([]string, error) {
	out := []string{}
	for n, c := range f.combos {
		if c.Enabled {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeCatalog) PickKey(_ context.Context, id int64) (store.KeyPick, error) {
	if f.pickKeyErr != nil {
		return store.KeyPick{}, f.pickKeyErr
	}
	if k, ok := f.keys[id]; ok {
		return k, nil
	}
	return store.KeyPick{}, store.ErrNotFound
}

func (f *fakeCatalog) ModelExists(_ context.Context, id int64, model string) (bool, error) {
	if m, ok := f.models[id]; ok {
		return m[model], nil
	}
	return true, nil // empty catalog -> allow (upstream is authoritative)
}

func (f *fakeCatalog) AllNamespaces(_ context.Context) ([]string, error) {
	return f.namespaces, nil
}

// ── namespace/model resolution (Calypso's #1: routing must be explicit) ─────

func TestResolveNamespaceModel(t *testing.T) {
	r := New(newFake())
	rt, err := r.Resolve(context.Background(), "super-ssn/gpt-6.1-sol", provider.SurfaceOpenAI, "hermes")
	if err != nil {
		t.Fatal(err)
	}
	if rt.Account.Namespace != "super-ssn" {
		t.Errorf("account = %q, want super-ssn", rt.Account.Namespace)
	}
	if rt.Model != "gpt-6.1-sol" {
		t.Errorf("model = %q", rt.Model)
	}
	if rt.KeyPlain != "sk-ssn-secret" {
		t.Error("wrong key decrypted for the account")
	}
	if rt.Alias != "super-ssn/gpt-6.1-sol" {
		t.Errorf("alias = %q, want the requested string", rt.Alias)
	}
	if rt.Surface != provider.SurfaceOpenAI {
		t.Errorf("surface = %q", rt.Surface)
	}
}

// The SAME model id on two accounts must resolve to the account named in the
// request — never guessed. This is the attribution guarantee.
func TestResolveSameModelDifferentAccounts(t *testing.T) {
	r := New(newFake())
	a, err := r.Resolve(context.Background(), "opengo/gpt-6-luna", provider.SurfaceOpenAI, "c")
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Resolve(context.Background(), "super-ssn/gpt-6-luna", provider.SurfaceOpenAI, "c")
	if err != nil {
		t.Fatal(err)
	}
	if a.Account.ID == b.Account.ID {
		t.Error("both resolved to the same account — attribution is ambiguous")
	}
	if a.KeyID == b.KeyID {
		t.Error("both resolved to the same key")
	}
	if a.Account.Namespace != "opengo" || b.Account.Namespace != "super-ssn" {
		t.Errorf("namespaces = %q / %q", a.Account.Namespace, b.Account.Namespace)
	}
}

// A bare model id has NO default account: it must 404 with hints listing the
// namespaces that actually serve it.
func TestResolveBareModelIs404WithHints(t *testing.T) {
	r := New(newFake())
	_, err := r.Resolve(context.Background(), "gpt-6-luna", provider.SurfaceOpenAI, "c")
	if err == nil {
		t.Fatal("bare model id must not resolve (no default account)")
	}
	var nf *ErrNotFound
	if !errors.As(err, &nf) {
		t.Fatalf("err = %T %v, want *ErrNotFound", err, err)
	}
	joined := strings.Join(nf.Hints, ",")
	if !strings.Contains(joined, "opengo/gpt-6-luna") || !strings.Contains(joined, "super-ssn/gpt-6-luna") {
		t.Errorf("hints should list both serving namespaces, got %v", nf.Hints)
	}
	if !strings.Contains(err.Error(), "available:") {
		t.Errorf("error should be helpful: %v", err)
	}
}

// Hints must be filtered to the requested model, not a catalog dump.
func TestResolveHintsAreFiltered(t *testing.T) {
	r := New(newFake())
	_, err := r.Resolve(context.Background(), "gpt-6.1-sol", provider.SurfaceOpenAI, "c")
	var nf *ErrNotFound
	if !errors.As(err, &nf) {
		t.Fatal(err)
	}
	for _, h := range nf.Hints {
		if !strings.Contains(h, "gpt-6.1-sol") {
			t.Errorf("hint %q is unrelated to the requested model", h)
		}
	}
	if len(nf.Hints) != 1 {
		t.Errorf("expected exactly 1 hint, got %v", nf.Hints)
	}
}

func TestResolveUnknownNamespace(t *testing.T) {
	r := New(newFake())
	_, err := r.Resolve(context.Background(), "nope/gpt-6-luna", provider.SurfaceOpenAI, "c")
	var nf *ErrNotFound
	if !errors.As(err, &nf) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if len(nf.Hints) == 0 {
		t.Error("unknown namespace with a known model should still hint")
	}
}

func TestResolveUnknownModelInKnownNamespace(t *testing.T) {
	r := New(newFake())
	_, err := r.Resolve(context.Background(), "opengo/does-not-exist", provider.SurfaceOpenAI, "c")
	if err == nil {
		t.Fatal("unknown model must not resolve")
	}
	var nf *ErrNotFound
	if !errors.As(err, &nf) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestResolveDisabledAccount(t *testing.T) {
	f := newFake()
	f.models[3] = map[string]bool{"m": true}
	r := New(f)
	_, err := r.Resolve(context.Background(), "off/m", provider.SurfaceOpenAI, "c")
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled account must not route, got %v", err)
	}
}

func TestResolveNoUsableKey(t *testing.T) {
	f := newFake()
	f.pickKeyErr = store.ErrNotFound
	r := New(f)
	_, err := r.Resolve(context.Background(), "opengo/qwen3.8-flash", provider.SurfaceOpenAI, "c")
	if err == nil || !strings.Contains(err.Error(), "no usable key") {
		t.Fatalf("want 'no usable key', got %v", err)
	}
}

func TestResolveEmptyAndWhitespace(t *testing.T) {
	r := New(newFake())
	for _, m := range []string{"", "   ", "\t"} {
		if _, err := r.Resolve(context.Background(), m, provider.SurfaceOpenAI, "c"); err == nil {
			t.Errorf("model %q must be rejected", m)
		}
	}
	// surrounding whitespace is trimmed, not rejected
	rt, err := r.Resolve(context.Background(), "  opengo/qwen3.8-flash  ", provider.SurfaceOpenAI, "c")
	if err != nil {
		t.Fatalf("trimmed model should resolve: %v", err)
	}
	if rt.Model != "qwen3.8-flash" {
		t.Errorf("model = %q", rt.Model)
	}
}

// A model id containing dots (very common: qwen3.8-flash, gpt-6.1-sol) must
// split on the FIRST slash only.
func TestResolveModelIDsWithDots(t *testing.T) {
	r := New(newFake())
	for _, req := range []string{"opengo/qwen3.8-flash", "super-ssn/gpt-6.1-sol"} {
		rt, err := r.Resolve(context.Background(), req, provider.SurfaceOpenAI, "c")
		if err != nil {
			t.Fatalf("%s: %v", req, err)
		}
		if rt.Alias != req {
			t.Errorf("alias = %q, want %q", rt.Alias, req)
		}
	}
}

func TestResolvePreservesSurface(t *testing.T) {
	r := New(newFake())
	for _, s := range []provider.Surface{provider.SurfaceOpenAI, provider.SurfaceAnthropic, provider.SurfaceResponses} {
		rt, err := r.Resolve(context.Background(), "opengo/qwen3.8-flash", s, "c")
		if err != nil {
			t.Fatal(err)
		}
		if rt.Surface != s {
			t.Errorf("surface = %q, want %q", rt.Surface, s)
		}
	}
}

func TestSplitNamespace(t *testing.T) {
	cases := []struct {
		in        string
		ns, model string
		ok        bool
	}{
		{"opengo/gpt-6-luna", "opengo", "gpt-6-luna", true},
		{"super-ssn/gpt-6.1-sol", "super-ssn", "gpt-6.1-sol", true},
		{"a/b", "a", "b", true},
		{"gpt-6-luna", "", "", false},  // no slash
		{"/gpt-6-luna", "", "", false}, // empty namespace
		{"opengo/", "", "", false},     // empty model
		{"", "", "", false},
		// first slash wins; the rest is the model
		{"ns/a/b", "ns", "a/b", true},
	}
	for _, c := range cases {
		ns, model, ok := splitNamespace(c.in)
		if ok != c.ok || ns != c.ns || model != c.model {
			t.Errorf("splitNamespace(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, ns, model, ok, c.ns, c.model, c.ok)
		}
	}
}

func TestFilterHintsCapsLength(t *testing.T) {
	many := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		many = append(many, "ns/model")
	}
	if got := filterHints(many, "model"); len(got) != 8 {
		t.Errorf("hints should cap at 8, got %d", len(got))
	}
	if got := filterHints(many, "absent"); got != nil {
		t.Errorf("no match should yield nil, got %v", got)
	}
	if got := filterHints(nil, "x"); got != nil {
		t.Errorf("nil in -> nil out, got %v", got)
	}
}

// Compile-time proof that *Resolver satisfies the proxy's Rewriter contract.
var _ proxy.Rewriter = (*Resolver)(nil)

func TestErrNotFoundMessage(t *testing.T) {
	e := &ErrNotFound{Requested: "x/y"}
	if !strings.Contains(e.Error(), `"x/y"`) {
		t.Errorf("message should quote the request: %s", e.Error())
	}
	e2 := &ErrNotFound{Requested: "x/y", Hints: []string{"a/b"}}
	if !strings.Contains(e2.Error(), "available: a/b") {
		t.Errorf("message should list hints: %s", e2.Error())
	}
}
