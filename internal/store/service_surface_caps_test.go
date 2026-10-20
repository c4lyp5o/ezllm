package store

import (
	"context"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// The proto_* columns are CHAT-wire probe verdicts. A model that failed the
// chat probe (every embedding / image / rerank model does) must still read as
// UNKNOWN — never "unsupported" — on a service surface, or wire selection
// refuses it on the very endpoint it serves.
func TestServiceSurfaceCapabilityIsNeverTheChatProbe(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	id, err := db.UpsertAccount(ctx, provider.Account{Name: "a", Namespace: "a", Kind: provider.Kind("openai-compatible"), BaseURL: "https://x/v1", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModels(ctx, id, []provider.ModelInfo{{ID: "embed-1"}}); err != nil {
		t.Fatal(err)
	}
	no := false
	if err := db.SetModelProto(ctx, id, "embed-1", provider.SurfaceOpenAI, &no); err != nil {
		t.Fatal(err)
	}
	facts, err := db.ModelFacts(ctx, id, "embed-1")
	if err != nil {
		t.Fatal(err)
	}
	if v := facts.Supports(provider.SurfaceOpenAI); v == nil || *v {
		t.Fatalf("chat verdict = %v, want recorded false", v)
	}
	for _, s := range []provider.Surface{provider.SurfaceEmbeddings, provider.SurfaceImagesGen, provider.SurfaceAudioTTS, provider.SurfaceAudioSTT} {
		if v := facts.Supports(s); v != nil {
			t.Errorf("ModelFacts.Supports(%s) = %v, want nil (no probe covers it)", s, *v)
		}
		if v, err := db.ModelSupportsSurface(ctx, id, "embed-1", s); err != nil || v != nil {
			t.Errorf("ModelSupportsSurface(%s) = %v, %v; want nil", s, v, err)
		}
	}
}
