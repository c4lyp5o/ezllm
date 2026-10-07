package server

// GET /admin/requests — the token dissection explorer.
// The seeded shapes are deliberate: two small calls, two fat ones, and one
// fat call stamped 48h back so the DEFAULT 24h window provably excludes it.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

type requestsResp struct {
	Rows []struct {
		Tin  any `json:"tin"`
		Tout any `json:"tout"`
	} `json:"rows"`
	Summary struct {
		Count     int64 `json:"count"`
		Tin       int64 `json:"tin"`
		Tout      int64 `json:"tout"`
		MaxTin    int64 `json:"max_tin"`
		MaxTout   int64 `json:"max_tout"`
		Reasoning int64 `json:"reasoning"`
	} `json:"summary"`
	Filter struct {
		Sort  string `json:"sort"`
		Limit int    `json:"limit"`
	} `json:"filter"`
}

func doRequests(t *testing.T, h *harness, query string) (int, requestsResp) {
	t.Helper()
	code, body := jsonDo(t, h, "GET", "/admin/requests"+query, "tok-admin", nil)
	var out requestsResp
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("body is not JSON: %s", body)
		}
	}
	return code, out
}

func seedCalls(t *testing.T, h *harness) {
	t.Helper()
	now := time.Now()
	seeds := []store.Call{
		{TS: now, Client: "hermes", Surface: store.SurfaceOpenAI, Model: "m", Status: 200,
			TokensIn: 10, TokensOut: 5},
		{TS: now, Client: "hermes", Surface: store.SurfaceOpenAI, Model: "m", Status: 200,
			TokensIn: 200, TokensOut: 40},
		{TS: now, Client: "claude-code", Surface: store.SurfaceAnthropic, Model: "m", Status: 200,
			TokensIn: 5000, TokensOut: 300},
		{TS: now, Client: "claude-code", Surface: store.SurfaceAnthropic, Model: "m", Status: 200,
			TokensIn: 90000, TokensOut: 9000, ReasoningTokens: 700},
		{TS: now.Add(-48 * time.Hour), Client: "hermes", Surface: store.SurfaceOpenAI,
			Model: "m", Status: 429, TokensIn: 70000, TokensOut: 1},
	}
	for _, c := range seeds {
		h.db.RecordCall(c)
	}
	if err := h.db.Flush(); err != nil {
		t.Fatal(err)
	}
}

func TestRequestsFiltersAndSummary(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	seedCalls(t, h)

	// Default window: the 48h-old row is out, the four fresh rows are in.
	code, got := doRequests(t, h, "")
	if code != http.StatusOK {
		t.Fatalf("default = %d", code)
	}
	if got.Summary.Count != 4 {
		t.Errorf("default count = %d, want 4 (48h-old row outside the 24h window)", got.Summary.Count)
	}
	if got.Summary.Tin != 10+200+5000+90000 {
		t.Errorf("default tin sum = %d, want 95210", got.Summary.Tin)
	}
	if got.Summary.MaxTin != 90000 || got.Summary.MaxTout != 9000 {
		t.Errorf("maxima = %d/%d, want 90000/9000", got.Summary.MaxTin, got.Summary.MaxTout)
	}

	// min_tin: the two fat calls, summary over the FULL match.
	code, got = doRequests(t, h, "?min_tin=5000")
	if code != http.StatusOK {
		t.Fatalf("min_tin = %d", code)
	}
	if got.Summary.Count != 2 || got.Summary.Tin != 95000 {
		t.Errorf("min_tin=5000 → count=%d tin=%d, want 2/95000", got.Summary.Count, got.Summary.Tin)
	}

	// max_tin: the two small calls.
	_, got = doRequests(t, h, "?max_tin=200")
	if got.Summary.Count != 2 || got.Summary.Tout != 45 {
		t.Errorf("max_tin=200 → count=%d tout=%d, want 2/45", got.Summary.Count, got.Summary.Tout)
	}

	// tout range: only the 300-token and 9000-token outputs (both fresh).
	_, got = doRequests(t, h, "?min_tout=300&max_tout=9000")
	if got.Summary.Count != 2 || got.Summary.Reasoning != 700 {
		t.Errorf("tout range → count=%d reasoning=%d, want 2/700", got.Summary.Count, got.Summary.Reasoning)
	}

	// sort=tin_desc puts the fattest input first.
	_, got = doRequests(t, h, "?sort=tin_desc")
	if len(got.Rows) == 0 {
		t.Fatal("no rows")
	}
	if tin, _ := got.Rows[0].Tin.(float64); int64(tin) != 90000 {
		t.Errorf("rows[0].tin = %v, want 90000 first under tin_desc", got.Rows[0].Tin)
	}
	if got.Filter.Sort != "tin_desc" || got.Filter.Limit != 50 {
		t.Errorf("filter echo = %s/%d, want tin_desc/50", got.Filter.Sort, got.Filter.Limit)
	}

	// Widening the window pulls the old error row in. (UTC form: a local
	// RFC3339 offset would carry a raw '+' that query decoding turns into a
	// space — the handler would 400.)
	code, got = doRequests(t, h, "?from="+time.Now().UTC().Add(-72*time.Hour).Format(time.RFC3339))
	if code != http.StatusOK {
		t.Fatalf("72h window = %d (check the from format)", code)
	}
	if got.Summary.Count != 5 {
		t.Errorf("72h window count = %d, want 5", got.Summary.Count)
	}

	// limit caps the page, never the summary.
	code, got = doRequests(t, h, "?limit=1&sort=tin_desc")
	if code != http.StatusOK || len(got.Rows) != 1 || got.Summary.Count != 4 {
		t.Errorf("limit=1 → rows=%d count=%d (want 1 page / 4 summary)", len(got.Rows), got.Summary.Count)
	}
}

func TestRequestsRejectsBadParams(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	seedCalls(t, h)
	for _, q := range []string{
		"?min_tin=abc", "?min_tin=-1", "?max_tout=-5",
		"?sort=hax", "?limit=0", "?limit=501",
		"?from=yesterday", "?to=soon",
	} {
		if code, _ := doRequests(t, h, q); code != http.StatusBadRequest {
			t.Errorf("%s → %d, want 400", q, code)
		}
	}
}
