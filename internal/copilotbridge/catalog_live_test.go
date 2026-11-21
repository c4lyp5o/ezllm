package copilotbridge

import (
	"context"
	"os"
	"testing"
)

// Explicit opt-in: discovery only, no inference and no service restart.
func TestLiveModelCatalog(t *testing.T) {
	bin := os.Getenv("EZLLM_CATALOG_PROOF_BIN")
	if bin == "" {
		t.Skip("live proof opt-in")
	}
	r := &Runner{Bin: bin, Home: os.Getenv("EZLLM_CATALOG_PROOF_HOME")}
	models, err := r.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, m := range models {
		found[m.ID] = true
	}
	for _, id := range []string{"auto", "gpt-6.2-sol", "mai-code-2", "mai-code-2-flash"} {
		if !found[id] {
			t.Fatalf("missing %s", id)
		}
	}
	t.Logf("live RPC catalog: %d models", len(models))
}
