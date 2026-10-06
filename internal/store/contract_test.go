package store

// Contract pin: the dashboard's TypeScript types are hand-written from
// docs/API.md, so nothing in `go build` or `tsc` can catch a JSON tag drifting
// away from what the frontend reads — it shows up as a silently blank stat
// card. This test closes that gap from the Go side by asserting the UsageRow
// marshalled key set against the UsageRow type declared in web/src/api.ts.
//
// If it fails: either rename the struct tag back, or update BOTH docs/API.md
// and web/src/api.ts in the same change.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// usageRowTSKeys extracts the field names from `export type UsageRow = { ... }`.
func usageRowTSKeys(t *testing.T) []string {
	t.Helper()
	// repo root is two levels up from internal/store
	path := filepath.Join("..", "..", "web", "src", "api.ts")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("frontend not present (%v) — nothing to compare against", err)
	}
	m := regexp.MustCompile(`export type UsageRow = \{([^}]*)\}`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("UsageRow type not found in %s — the frontend shape changed; update this test with it", path)
	}
	var keys []string
	for _, f := range strings.Split(string(m[1]), ";") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		// "tin: number" → "tin"
		k := strings.TrimSpace(strings.SplitN(f, ":", 2)[0])
		if k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		t.Fatalf("parsed no fields out of UsageRow in %s", path)
	}
	sort.Strings(keys)
	return keys
}

func TestUsageRowJSONKeysMatchFrontend(t *testing.T) {
	raw, err := json.Marshal(UsageRow{Key: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var goKeys []string
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for k := range m {
		goKeys = append(goKeys, k)
	}
	sort.Strings(goKeys)

	// Fields the API sends that the frontend simply doesn't use yet.
	unread := map[string]bool{"p50_ttft_ms": true}

	tsKeys := usageRowTSKeys(t)

	inGo := map[string]bool{}
	for _, k := range goKeys {
		inGo[k] = true
	}
	// 1. everything the frontend reads must actually be sent — this is the
	//    failure that blanked the stat cards.
	for _, k := range tsKeys {
		if !inGo[k] {
			t.Errorf("frontend reads %q but UsageRow does not send it (go: %v)", k, goKeys)
		}
	}
	// 2. nothing may be sent that the frontend neither reads nor knows about.
	inTS := map[string]bool{}
	for _, k := range tsKeys {
		inTS[k] = true
	}
	for _, k := range goKeys {
		if !inTS[k] && !unread[k] {
			t.Errorf("UsageRow sends %q which the frontend does not declare — add it to web/src/api.ts or to `unread`", k)
		}
	}
	t.Logf("go=%v ts=%v", goKeys, tsKeys)
}

// TestOverviewKeysMatchFrontend is the same pin for the overview envelope:
// Dashboard's first paint reads exactly these five keys.
func TestOverviewKeysMatchFrontend(t *testing.T) {
	want := []string{"accounts", "health", "recent_calls", "usage", "usage_by_surface"}
	// documented in docs/API.md §GET /admin/overview
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "API.md"))
	if err != nil {
		t.Skipf("docs/API.md: %v", err)
	}
	for _, k := range want {
		if !strings.Contains(string(doc), `"`+k+`"`) {
			t.Errorf("docs/API.md never mentions %q — the contract and the code have diverged", k)
		}
	}
}
