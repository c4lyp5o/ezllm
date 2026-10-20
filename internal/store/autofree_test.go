package store

import (
	"context"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func TestAutoFreeComboTracksCatalog(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	mk := func(name, ns string) int64 {
		id, err := db.UpsertAccount(ctx, provider.Account{Name: name, Namespace: ns, Kind: provider.Kind("openai-compatible"), BaseURL: "https://x/v1", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	or, ol := mk("or", "or"), mk("ol", "ol")
	cat := func(ids ...string) []provider.ModelInfo {
		var out []provider.ModelInfo
		for _, id := range ids {
			out = append(out, provider.ModelInfo{ID: id})
		}
		return out
	}
	if err := db.UpsertModels(ctx, or, cat("a/x:free", "a/paid")); err != nil {
		t.Fatal(err)
	}
	cid, err := db.UpsertCombo(ctx, Combo{Name: "free", Strategy: "failover", Enabled: true, AutoFree: true,
		AutoFreeInclude: []FreeInclude{{AccountID: ol, ModelID: "beam"}}})
	if err != nil {
		t.Fatal(err)
	}
	hops := func() []string {
		cs, _ := db.ListCombos(ctx)
		var out []string
		for _, c := range cs {
			if c.ID == cid {
				for _, h := range c.Hops {
					out = append(out, h.ModelID)
				}
			}
		}
		return out
	}
	if got := hops(); len(got) != 1 || got[0] != "a/x:free" {
		t.Fatalf("hops after save = %v, want [a/x:free] (beam not in catalog yet)", got)
	}
	if err := db.UpsertModels(ctx, ol, cat("beam", "kimi-k3")); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModels(ctx, or, cat("a/x:free", "b/y-free", "a/paid")); err != nil {
		t.Fatal(err)
	}
	if got := hops(); len(got) != 3 {
		t.Fatalf("hops after catalog sync = %v, want x:free, y-free, beam", got)
	}
	if err := db.UpsertModels(ctx, or, cat("b/y-free")); err != nil {
		t.Fatal(err)
	}
	if got := hops(); len(got) != 2 {
		t.Fatalf("a model that left the catalog must leave the pool, got %v", got)
	}
}

func TestFreshDBSeedsFreeComboOnce(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	cs, err := db.ListCombos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Name != DefaultFreeComboName || !cs[0].AutoFree || len(cs[0].Hops) != 0 {
		t.Fatalf("fresh db combos = %+v, want one empty auto_free %q", cs, DefaultFreeComboName)
	}
	// First provider arrives: the seeded pool fills without any combo call.
	id, err := db.UpsertAccount(ctx, provider.Account{Name: "or", Namespace: "or", Kind: provider.Kind("openai-compatible"), BaseURL: "https://x/v1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModels(ctx, id, []provider.ModelInfo{{ID: "a/x:free"}, {ID: "a/paid"}}); err != nil {
		t.Fatal(err)
	}
	cs, _ = db.ListCombos(ctx)
	if len(cs[0].Hops) != 1 || cs[0].Hops[0].ModelID != "a/x:free" {
		t.Fatalf("seeded combo hops = %+v", cs[0].Hops)
	}
	// A deleted seed is not resurrected by a later Open.
	if err := db.DeleteCombo(ctx, cs[0].ID); err != nil {
		t.Fatal(err)
	}
	cs, _ = db.ListCombos(ctx)
	if len(cs) != 0 {
		t.Fatalf("deleted seed came back: %+v", cs)
	}
}

// Cloudflare Workers AI free-tier models carry no id marker; the built-in list
// names them. Only chat-capable ones on a host-verified Workers AI account
// join the pool — an embedding model, a same-id model on another host, and an
// unlisted model all stay out.
func TestAutoFreeCloudflareBuiltinList(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	mk := func(name, ns, base string) int64 {
		id, err := db.UpsertAccount(ctx, provider.Account{Name: name, Namespace: ns, Kind: provider.Kind("openai-compatible"), BaseURL: base, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	cf := mk("cf", "cloudflare", "https://api.cloudflare.com/client/v4/accounts/abc123/ai/v1")
	spoof := mk("cf2", "cloudflare2", "https://gateway.example/v1")
	cat := func(ids ...string) []provider.ModelInfo {
		var out []provider.ModelInfo
		for _, id := range ids {
			out = append(out, provider.ModelInfo{ID: id})
		}
		return out
	}
	if err := db.UpsertModels(ctx, cf, cat("@cf/openai/gpt-oss-120b", "@cf/meta/llama-3.3-70b-instruct-fp8-fast", "@cf/baai/bge-m3", "@cf/meta/not-on-the-list")); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModels(ctx, spoof, cat("@cf/openai/gpt-oss-20b")); err != nil {
		t.Fatal(err)
	}
	cid, err := db.UpsertCombo(ctx, Combo{Name: "free", Strategy: "failover", Enabled: true, AutoFree: true})
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := db.ListCombos(ctx)
	var got []string
	for _, c := range cs {
		if c.ID == cid {
			for _, h := range c.Hops {
				got = append(got, h.ModelID)
			}
		}
	}
	want := map[string]bool{"@cf/openai/gpt-oss-120b": true, "@cf/meta/llama-3.3-70b-instruct-fp8-fast": true}
	if len(got) != len(want) {
		t.Fatalf("hops = %v, want exactly %v", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected hop %q (embedding / unlisted / spoof host must stay out)", id)
		}
	}
}
