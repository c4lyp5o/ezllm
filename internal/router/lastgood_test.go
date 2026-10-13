package router

// F8 regression: lastGoodEntry used to store a whole proxy.Route, whose
// KeyPlain field is the DECRYPTED upstream credential. Both readers
// (stickyOrder) use exactly two fields — the account id and
// the model — so everything else the Route carries, including the plaintext
// key, was being pinned in a process-global map with no eviction, duplicating
// it outside the request scope that needed it.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/c4lyp5o/ezllm/internal/proxy"
	"github.com/c4lyp5o/ezllm/internal/store"
)

func TestLastGoodEntryCarriesNoKeyMaterial(t *testing.T) {
	r := New(withCombo(newFake(), dayNightCombo()))
	const secretKey = "sk-super-secret-plaintext-key"
	route := hop(9, "gpt-6-luna")
	route.KeyPlain = secretKey
	route.KeyHint = "****abcd"
	route.Alias = "daily"
	r.MarkHopGood("daily", route)

	v, ok := lastGood.Load("daily")
	if !ok {
		t.Fatal("MarkHopGood stored nothing")
	}
	e, ok := v.(lastGoodEntry)
	if !ok {
		t.Fatalf("stored value is %T, want lastGoodEntry", v)
	}
	// Behaviour must survive: both identifying fields still land.
	if e.accountID != 9 {
		t.Errorf("accountID = %d, want 9", e.accountID)
	}
	if e.model != "gpt-6-luna" {
		t.Errorf("model = %q, want gpt-6-luna", e.model)
	}
	if e.at.IsZero() {
		t.Error("timestamp missing: sticky_idle_s would treat every entry as expired")
	}
	// The point of the fix: no key material anywhere in the stored value.
	dumped := fmt.Sprintf("%+v", e)
	if strings.Contains(dumped, secretKey) {
		t.Fatal("lastGood still stores the decrypted provider key")
	}
	if strings.Contains(strings.ToLower(dumped), "key") {
		t.Fatalf("lastGood still carries any key field: %s", dumped)
	}

	// And the strategies built on it still resolve the remembered hop.
	routes := []proxy.Route{hop(1, "m"), hop(9, "gpt-6-luna")}
	sticky := &store.Combo{Name: "daily", Strategy: "sticky_last_good"}
	ordered := stickyOrder(sticky, routes)
	if len(ordered) != 2 || ordered[0].Account.ID != 9 {
		t.Fatalf("sticky_last_good no longer sticks to hop 9: %+v", ordered)
	}
}
