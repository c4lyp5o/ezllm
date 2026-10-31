package toolprune

import (
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"
)

func rootOf(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("fixture decode: %v", err)
	}
	return m
}

func TestObserveShippedShapes(t *testing.T) {
	obs := Observe(rootOf(t, `{
	  "tools": [
	    {"type":"function","function":{"name":"Bash","description":"run"}},
	    {"name":"Read","input_schema":{"type":"object"}},
	    {"type":"function","name":"write","parameters":{}},
	    {"type":"mcp","server":"x"}
	  ],
	  "functions": [{"name":"legacy"}],
	  "messages": [{"role":"user","content":"hi"}]
	}`))
	if len(obs.Shipped) != 4 {
		t.Fatalf("shipped = %d entries, want 4 (legacy functions must not count)", len(obs.Shipped))
	}
	want := []string{"Bash", "Read", "write", ""}
	for i, w := range want {
		if obs.Shipped[i].Name != w {
			t.Errorf("shipped[%d].Name = %q, want %q", i, obs.Shipped[i].Name, w)
		}
		if w != "" && obs.Shipped[i].Tokens <= 0 {
			t.Errorf("shipped[%d].Tokens = %d, want > 0", i, obs.Shipped[i].Tokens)
		}
	}
}

func TestObserveUsedAcrossWires(t *testing.T) {
	// chat: tool_calls + role:tool result; anthropic: tool_use + tool_result;
	// responses: function_call + function_call_output; legacy: function_call.
	obs := Observe(rootOf(t, `{
	  "messages": [
	    {"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{}"}}]},
	    {"role":"tool","tool_call_id":"c1","content":"ok"},
	    {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]},
	    {"role":"assistant","function_call":{"name":"Greet","arguments":"{}"}},
	    {"role":"function","name":"Greet","content":"ok"}
	  ]
	}`))
	for _, name := range []string{"Bash", "Read", "Greet"} {
		if !obs.Used[name] {
			t.Errorf("used[%q] = false, want true", name)
		}
	}

	resp := Observe(rootOf(t, `{
	  "input": [
	    {"type":"function_call","name":"Patch","call_id":"k1","arguments":"{}"},
	    {"type":"function_call_output","call_id":"k1","output":"ok"}
	  ]
	}`))
	if !resp.Used["Patch"] {
		t.Error("responses wire: Patch must be marked used")
	}

	// Result-only visibility: a compacted history keeps the result but lost
	// the call site. Without an id mapping nothing resolves — deliberately
	// conservative in the KEEP direction only via EligibleName's thresholds.
	orphan := Observe(rootOf(t, `{
	  "messages": [{"role":"user","content":[{"type":"tool_result","tool_use_id":"gone","content":"ok"}]}]
	}`))
	if len(orphan.Used) != 0 {
		t.Errorf("orphan result must mark nothing used, got %v", orphan.Used)
	}
}

func TestObserveToolChoice(t *testing.T) {
	cases := []struct {
		name    string
		choice  string
		wantN   string
		wantDem bool
	}{
		{"auto", `"auto"`, "", false},
		{"none", `"none"`, "", false},
		{"required", `"required"`, "", true},
		{"any-string", `"any"`, "", true},
		{"any-typed", `{"type":"any"}`, "", true},
		{"chat-forced", `{"type":"function","function":{"name":"X"}}`, "X", false},
		{"anthropic-forced", `{"type":"tool","name":"X"}`, "X", false},
		{"responses-forced", `{"type":"function","name":"X"}`, "X", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obs := Observe(rootOf(t, `{"tool_choice":`+c.choice+`,"tools":[{"name":"X"}],"messages":[]}`))
			if obs.ChoiceName != c.wantN || obs.ChoiceDemands != c.wantDem {
				t.Errorf("choice = (%q,%v), want (%q,%v)", obs.ChoiceName, obs.ChoiceDemands, c.wantN, c.wantDem)
			}
		})
	}
}

func TestEligibleName(t *testing.T) {
	p := Policy{MinUnusedSessions: 3, Keep: map[string]bool{"ExitPlanMode": true}}
	cases := []struct {
		name string
		st   StatsView
		want bool
	}{
		{"Bash", StatsView{ShippedSessions: 3, UsedSessions: 0}, true},
		{"Bash", StatsView{ShippedSessions: 2, UsedSessions: 0}, false}, // under threshold
		{"Bash", StatsView{ShippedSessions: 9, UsedSessions: 1}, false}, // sticky used
		{"ExitPlanMode", StatsView{ShippedSessions: 9, UsedSessions: 0}, false},
		{"", StatsView{ShippedSessions: 9, UsedSessions: 0}, false}, // unnameable
	}
	for _, c := range cases {
		if got := p.EligibleName(c.name, c.st); got != c.want {
			t.Errorf("EligibleName(%q, %+v) = %v, want %v", c.name, c.st, got, c.want)
		}
	}
}

func TestEligibleGuards(t *testing.T) {
	p := Policy{MinUnusedSessions: 1}
	obs := Observation{
		Shipped: []Schema{{Name: "A", Tokens: 10}, {Name: "B", Tokens: 20}, {Name: "C", Tokens: 4}},
		Used:    map[string]bool{},
	}
	stats := map[string]StatsView{"A": {ShippedSessions: 5}, "B": {ShippedSessions: 5}, "C": {ShippedSessions: 5, UsedSessions: 1}}

	got := p.Eligible(obs, stats)
	if len(got) != 2 || got[0].Name != "A" || got[1].Name != "B" {
		t.Fatalf("eligible = %+v, want [A B] (C is used)", got)
	}
	if n := ShadowTokens(got); n != 30 {
		t.Errorf("ShadowTokens = %d, want 30", n)
	}

	// A forced target is protected…
	obs.ChoiceName = "A"
	got = p.Eligible(obs, stats)
	if len(got) != 1 || got[0].Name != "B" {
		t.Errorf("forced A must be protected, got %+v", got)
	}
	// …and a demand for SOME call disables eligibility entirely.
	obs.ChoiceName, obs.ChoiceDemands = "", true
	if got := p.Eligible(obs, stats); got != nil {
		t.Errorf("tool_choice demand must yield no eligible set, got %+v", got)
	}
}

func TestTrackerDeltas(t *testing.T) {
	trk := NewTracker()
	now := time.Now()
	obs := Observation{
		Shipped: []Schema{{Name: "A", Tokens: 10}, {Name: "B", Tokens: 20}},
		Used:    map[string]bool{"A": true},
	}

	first := trk.Deltas("cli", "s1", obs, now)
	if len(first) != 2 {
		t.Fatalf("first deltas = %+v, want 2", first)
	}
	for _, d := range first {
		if d.ShippedRequests != 1 || d.ShippedSessions != 1 {
			t.Errorf("first delta %+v: want shipped 1/1", d)
		}
		if d.Tool == "A" {
			if d.UsedSessions != 1 || d.LastUsedAt == "" {
				t.Errorf("A must carry its used flip, got %+v", d)
			}
		} else if d.UsedSessions != 0 || d.LastUsedAt != "" {
			t.Errorf("B must not carry usage, got %+v", d)
		}
	}

	// Same session again: requests count, pairs do not, the used flip is
	// spent (sticky within the session).
	second := trk.Deltas("cli", "s1", obs, now.Add(time.Minute))
	for _, d := range second {
		if d.ShippedRequests != 1 || d.ShippedSessions != 0 || d.UsedSessions != 0 {
			t.Errorf("second delta %+v: want request-only increments", d)
		}
	}

	// A NEW session counts its pairs again.
	third := trk.Deltas("cli", "s2", obs, now.Add(2*time.Minute))
	for _, d := range third {
		if d.ShippedSessions != 1 {
			t.Errorf("new-session delta %+v: want shipped_sessions 1", d)
		}
	}

	// Used but no longer shipped still owes its flip (sticky survives a
	// shrinking catalog).
	fourth := trk.Deltas("cli", "s2", Observation{
		Shipped: []Schema{{Name: "A", Tokens: 10}},
		Used:    map[string]bool{"Ghost": true},
	}, now.Add(3*time.Minute))
	var ghost *Delta
	for i := range fourth {
		if fourth[i].Tool == "Ghost" {
			ghost = &fourth[i]
		}
	}
	if ghost == nil || ghost.UsedSessions != 1 {
		t.Fatalf("Ghost must carry a used flip, got %+v", fourth)
	}

	// No derivable session: request counts only — a stateless client can
	// never make a tool eligible (conservative by construction).
	stateless := trk.Deltas("cli", "", obs, now.Add(4*time.Minute))
	for _, d := range stateless {
		if d.ShippedSessions != 0 || d.UsedSessions != 0 {
			t.Errorf("stateless delta %+v: want no session counters", d)
		}
	}
}

func TestObserveServerToolUseMarksUsed(t *testing.T) {
	// Anthropic server tools are shipped by name but called as
	// server_tool_use / mcp_tool_use, never tool_use.
	obs := Observe(rootOf(t, `{
	  "tools": [{"type":"web_search_20250305","name":"web_search"}],
	  "messages": [
	    {"role":"assistant","content":[
	      {"type":"server_tool_use","id":"s1","name":"web_search","input":{}},
	      {"type":"web_search_tool_result","tool_use_id":"s1","content":[]},
	      {"type":"mcp_tool_use","id":"m1","name":"lookup","server_name":"x","input":{}}
	    ]}
	  ]
	}`))
	for _, name := range []string{"web_search", "lookup"} {
		if !obs.Used[name] {
			t.Errorf("used[%q] = false, want true", name)
		}
	}
	p := Policy{MinUnusedSessions: 1}
	if got := p.Eligible(obs, map[string]StatsView{"web_search": {ShippedSessions: 5}}); len(got) != 0 {
		t.Errorf("a server tool in use must not be eligible, got %+v", got)
	}
}

func TestObserveUnresolvedResult(t *testing.T) {
	// previous_response_id: the output arrives without its function_call.
	obs := Observe(rootOf(t, `{
	  "previous_response_id": "resp_1",
	  "tools": [{"type":"function","name":"Patch","parameters":{}}],
	  "input": [{"type":"function_call_output","id":"fco_1","call_id":"k1","output":"ok"}]
	}`))
	if !obs.Unresolved {
		t.Fatal("an output with no call site must report Unresolved")
	}
	p := Policy{MinUnusedSessions: 1}
	if got := p.Eligible(obs, map[string]StatsView{"Patch": {ShippedSessions: 5}}); got != nil {
		t.Errorf("unresolved request must yield no eligible set, got %+v", got)
	}

	// A result resolved by any of its ids is not unresolved.
	ok := Observe(rootOf(t, `{
	  "input": [
	    {"type":"function_call","name":"Patch","call_id":"k1","arguments":"{}"},
	    {"type":"function_call_output","id":"fco_1","call_id":"k1","output":"ok"}
	  ]
	}`))
	if ok.Unresolved {
		t.Error("a resolved output must not report Unresolved")
	}
}

func TestEligibleSkipsToolUsedThisRequest(t *testing.T) {
	// Stats predate this request; the trace already calls A.
	p := Policy{MinUnusedSessions: 1}
	obs := Observation{
		Shipped: []Schema{{Name: "A", Tokens: 10}, {Name: "B", Tokens: 20}},
		Used:    map[string]bool{"A": true},
	}
	got := p.Eligible(obs, map[string]StatsView{"A": {ShippedSessions: 5}, "B": {ShippedSessions: 5}})
	if len(got) != 1 || got[0].Name != "B" {
		t.Fatalf("eligible = %+v, want [B] (A is in this request's trace)", got)
	}
}

func TestTrackerUnresolvedWithdrawsSession(t *testing.T) {
	trk := NewTracker()
	now := time.Now()
	shipped := []Schema{{Name: "A", Tokens: 10}, {Name: "B", Tokens: 20}}
	trk.Deltas("cli", "s1", Observation{Shipped: shipped, Used: map[string]bool{"A": true}}, now)

	// The next request shows an orphan result: B's shipped pair is withdrawn
	// (A was used, its pair stands) and nothing counts afterwards.
	got := trk.Deltas("cli", "s1", Observation{Shipped: shipped, Used: map[string]bool{}, Unresolved: true}, now.Add(time.Minute))
	sess := map[string]int64{}
	for _, d := range got {
		sess[d.Tool] += d.ShippedSessions
	}
	if sess["B"] != -1 || sess["A"] != 0 {
		t.Fatalf("withdrawal = %v, want B:-1 A:0 (deltas %+v)", sess, got)
	}
	again := trk.Deltas("cli", "s1", Observation{Shipped: append(shipped, Schema{Name: "C", Tokens: 5}), Used: map[string]bool{}}, now.Add(2*time.Minute))
	for _, d := range again {
		if d.ShippedSessions != 0 {
			t.Errorf("tainted session must not count pairs, got %+v", d)
		}
	}
}

func TestTrackerConcurrentSameSession(t *testing.T) {
	// Parallel requests of one session (subagents, retries) share its maps.
	// Without the lock spanning Deltas this is a fatal concurrent map write
	// (and a -race failure).
	trk := NewTracker()
	obs := Observation{Used: map[string]bool{"t0": true}}
	for i := 0; i < 64; i++ {
		obs.Shipped = append(obs.Shipped, Schema{Name: "t" + strconv.Itoa(i), Tokens: 1})
	}
	var total int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				for _, d := range trk.Deltas("cli", "s1", obs, time.Now()) {
					mu.Lock()
					total += d.ShippedSessions
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if total != int64(len(obs.Shipped)) {
		t.Errorf("shipped_sessions total = %d, want %d (each pair once)", total, len(obs.Shipped))
	}
}
