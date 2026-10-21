package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

func catalogIDs(t *testing.T, h *harness, acct int64) []string {
	t.Helper()
	var out []string
	for _, m := range listModels(t, h, acct) {
		out = append(out, m.ID)
	}
	sort.Strings(out)
	return out
}

// "Test all" (probe_all, only-untested) on a catalog where some models are
// already proven: the untested filter used to run IN PLACE over the very slice
// the catalog was then saved from, overwriting proven entries with untested
// ones — a sync that pruned models it never meant to touch.
func TestTestAllSyncKeepsTheFullCatalog(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"},{"id":"c"},{"id":"d"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	acct := seededAccountID(t, h)
	ctx := context.Background()
	if err := h.db.UpsertModels(ctx, acct, []provider.ModelInfo{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}); err != nil {
		t.Fatal(err)
	}
	yes := true
	for _, m := range []string{"a", "b"} {
		for _, sf := range comboProbeSurfaces {
			if err := h.db.SetModelProto(ctx, acct, m, sf, &yes); err != nil {
				t.Fatal(err)
			}
		}
	}
	code, _, body := h.do("POST", fmt.Sprintf("/admin/accounts/%d/sync", acct), "tok-admin", `{"probe_all":true}`)
	if code != 200 {
		t.Fatalf("sync = %d %s", code, body)
	}
	deadline := time.Now().Add(20 * time.Second)
	for !sweepIdle(h, acct) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := catalogIDs(t, h, acct); fmt.Sprint(got) != "[a b c d]" {
		t.Fatalf("catalog after test-all = %v, want [a b c d]", got)
	}
}

// A test-all with nothing left to probe still saves the catalog it just
// listed: it answered "synced: N" while silently keeping the old catalog.
func TestTestAllSyncSavesCatalogWhenNothingToProbe(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"a"},{"id":"gone-new"}]}`))
	})
	acct := seededAccountID(t, h)
	ctx := context.Background()
	if err := h.db.UpsertModels(ctx, acct, []provider.ModelInfo{{ID: "a"}, {ID: "old"}}); err != nil {
		t.Fatal(err)
	}
	yes := true
	for _, m := range []string{"a", "gone-new"} {
		for _, sf := range comboProbeSurfaces {
			if err := h.db.SetModelProto(ctx, acct, m, sf, &yes); err != nil {
				t.Fatal(err)
			}
		}
	}
	code, _, body := h.do("POST", fmt.Sprintf("/admin/accounts/%d/sync", acct), "tok-admin", `{"probe_all":true}`)
	if code != 200 {
		t.Fatalf("sync = %d %s", code, body)
	}
	if got := catalogIDs(t, h, acct); fmt.Sprint(got) != "[a gone-new]" {
		t.Fatalf("catalog after a nothing-to-probe sync = %v, want the listed [a gone-new]", got)
	}
}
