package router

import (
	"context"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// Explain reports every hop with its verdict, in the combo's order.
func TestExplainComboHops(t *testing.T) {
	c := &store.Combo{ID: 7, Name: "daily", Strategy: "true_round_robin", Enabled: true, Hops: []store.ComboHop{
		{AccountID: 1, ModelID: "gpt-6-luna"},
		{AccountID: 2, ModelID: "gpt-6-luna", Enabled: boolPtr(false)},
		{AccountID: 3, ModelID: "gpt-6-luna"},
		{AccountID: 2, ModelID: "missing-model"},
		{AccountID: 2, ModelID: "gpt-6.1-sol"},
	}}
	r := New(withCombo(newFake(), c))
	ex := r.Explain(context.Background(), "daily", provider.SurfaceChat)
	if ex.Kind != "combo" || ex.Strategy != "true_round_robin" || ex.Error != "" {
		t.Fatalf("explanation = %+v", ex)
	}
	want := []struct{ ns, status, reason string }{
		{"demo", "candidate", ""},
		{"initech", "skipped", "turned off"},
		{"off", "skipped", "disabled"},
		{"initech", "skipped", "model list"},
		{"initech", "candidate", ""},
	}
	if len(ex.Hops) != len(want) {
		t.Fatalf("hops = %+v", ex.Hops)
	}
	for i, w := range want {
		h := ex.Hops[i]
		if h.Namespace != w.ns || h.Status != w.status || !strings.Contains(h.Reason, w.reason) {
			t.Errorf("hop %d = %+v, want %s/%s/%q", i, h, w.ns, w.status, w.reason)
		}
	}
	if ex.Hops[0].Wire != "chat" {
		t.Errorf("candidate wire = %q, want chat", ex.Hops[0].Wire)
	}
}

// A dry run moves no strategy state: a round-robin cursor stays put.
func TestExplainDoesNotAdvanceBlockCursor(t *testing.T) {
	c := &store.Combo{ID: 8, Name: "blocky", Strategy: "block_round_robin", BlockSize: 2, Enabled: true, Hops: []store.ComboHop{
		{AccountID: 1, ModelID: "gpt-6-luna"}, {AccountID: 2, ModelID: "gpt-6-luna"},
	}}
	r := New(withCombo(newFake(), c))
	for i := 0; i < 3; i++ {
		r.Explain(context.Background(), "blocky", provider.SurfaceChat)
	}
	for _, bc := range r.BlockCursors() {
		if bc.Combo == "blocky" && bc.Hop != "" {
			t.Fatalf("explain started a block: %+v", bc)
		}
	}
}

// A direct route is one hop; an unknown one says why.
func TestExplainDirect(t *testing.T) {
	r := New(newFake())
	ok := r.Explain(context.Background(), "initech/gpt-6-luna", provider.SurfaceChat)
	if ok.Kind != "direct" || len(ok.Hops) != 1 || ok.Hops[0].Status != "candidate" {
		t.Fatalf("direct = %+v", ok)
	}
	bad := r.Explain(context.Background(), "initech/nope", provider.SurfaceChat)
	if len(bad.Hops) != 1 || bad.Hops[0].Status != "refused" || bad.Error == "" {
		t.Fatalf("unknown direct = %+v", bad)
	}
}
