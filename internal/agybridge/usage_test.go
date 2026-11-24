package agybridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Measured on 1.5.13 (`agy -p /usage --output-format json`), descriptions cut.
const measuredUsage = `{"conversation_id":"","status":"SUCCESS","response":"Gemini Models\tWeekly Limit Remaining\t89%\t2026-12-01T02:30:00Z\n","duration_seconds":0,"num_turns":0,"usage":{"input_tokens":0,"output_tokens":0,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":0},"command":{"name":"usage","data":{"description":"d","groups":[{"name":"Gemini Models","description":"d","buckets":[{"id":"gemini-weekly","name":"Weekly Limit Remaining","window":"weekly","remaining_fraction":0.8943840265274048,"reset_time":"2026-12-01T02:30:00Z"}]},{"name":"Claude and GPT models","description":"d","buckets":[{"id":"3p-weekly","name":"Weekly Limit Remaining","window":"weekly","remaining_fraction":1,"reset_time":"2026-12-01T23:07:28Z"}]}]}}}`

func TestPlanUsageRunsTheUsageCommandAndNoTurn(t *testing.T) {
	bin, dir := fakeAgy(t, measuredUsage, 0)
	got, err := (&Runner{Bin: bin, Home: dir}).PlanUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []PlanBucket{{"Gemini Models", "weekly", 10.56, "2026-12-01T02:30:00Z"}, {"Claude and GPT models", "weekly", 0, "2026-12-01T23:07:28Z"}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("buckets = %+v, want %+v", got, want)
	}
	if argv := read(t, dir+"/argv"); argv != "--print=/usage\n--output-format\njson\n" {
		t.Fatalf("argv = %q: the command must not reach a model as a prompt", argv)
	}
	if cwd := strings.TrimSpace(read(t, dir+"/cwd")); cwd == dir || !strings.Contains(cwd, "agy-bridge-") {
		t.Fatalf("cwd = %q, want a private empty folder", cwd)
	}
}

func TestParsePlanUsageRefusesAnythingButTheCommandAnswer(t *testing.T) {
	for name, body := range map[string]string{
		"a model turn":       strings.Replace(measuredUsage, `"num_turns":0`, `"num_turns":1`, 1),
		"another command":    strings.Replace(measuredUsage, `"name":"usage","data"`, `"name":"help","data"`, 1),
		"no command":         `{"status":"SUCCESS","response":"You have plenty left","num_turns":1}`,
		"an error":           `{"status":"ERROR","error":"authentication required"}`,
		"text, not JSON":     "Gemini Models\tWeekly",
		"an empty reply":     "",
		"a truncated object": measuredUsage[:200],
	} {
		if got, err := ParsePlanUsage([]byte(body)); err == nil {
			t.Errorf("%s: accepted as %+v", name, got)
		}
	}
}

func TestParsePlanUsageLeavesOutAnUnknownBucketAndKeepsAnEmptyOne(t *testing.T) {
	got, err := ParsePlanUsage([]byte(strings.Replace(strings.Replace(measuredUsage, `"remaining_fraction":1,`, "", 1), `0.8943840265274048`, "0", 1)))
	if err != nil || len(got) != 1 || got[0].Used != 100 {
		t.Fatalf("got %+v, %v; want only the Gemini group, 100%% used", got, err)
	}
}

func TestPlanUsageFailuresAreTyped(t *testing.T) {
	bin, dir := fakeAgy(t, "", 1)
	if _, err := (&Runner{Bin: bin, Home: dir}).PlanUsage(context.Background()); err == nil || strings.Contains(err.Error(), "http") {
		t.Fatalf("err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (&Runner{Bin: bin, Home: dir}).PlanUsage(ctx)
	var ne *NativeError
	if !errors.As(err, &ne) || ne.Status != 499 {
		t.Fatalf("cancelled err = %v", err)
	}
}

func TestSupportsPlanUsage(t *testing.T) {
	for v, want := range map[string]bool{"1.1.11": true, "1.1.12": true, "1.2.0": true, "1.5.13": true, "2.0.0": true, " 1.5.16\n": true,
		"1.1.10": false, "1.1.9": false, "1.0.99": false, "0.9.12": false, "": false, "1.5": false, "1.5.x": false, "agy 1.5.13": false} {
		if got := SupportsPlanUsage(v); got != want {
			t.Errorf("SupportsPlanUsage(%q) = %v, want %v", v, got, want)
		}
	}
}

type usageExec struct {
	fakeExec
	buckets []PlanBucket
	err     error
	calls   int
}

func (u *usageExec) PlanUsage(context.Context) ([]PlanBucket, error) {
	u.calls++
	return u.buckets, u.err
}

func getUsage(t *testing.T, ex Executor, on bool, token string) (int, string) {
	t.Helper()
	s := &Server{Runner: ex, Token: "tok", Version: "test", PlanUsage: on, Now: func() time.Time { return time.Unix(1700000000, 0) }}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	req, _ := http.NewRequest("GET", ts.URL+"/v1/usage", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestUsageEndpointReportsTheFullestGroupAndEveryGroup(t *testing.T) {
	buckets, err := ParsePlanUsage([]byte(measuredUsage))
	if err != nil {
		t.Fatal(err)
	}
	status, body := getUsage(t, &usageExec{buckets: buckets}, true, "tok")
	if status != 200 {
		t.Fatalf("status %d: %s", status, body)
	}
	var got struct {
		Usage  map[string]struct{ Percent float64 }
		Groups []struct {
			Name    string
			Percent float64
		}
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Usage) != 1 || got.Usage["weekly"].Percent != 10.56 || !strings.Contains(body, `"resetsAt":"2026-12-01T02:30:00Z"`) {
		t.Fatalf("usage = %s, want only the weekly window at the fullest group", body)
	}
	if len(got.Groups) != 2 || got.Groups[0].Name != "Gemini Models" || got.Groups[1].Name != "Claude and GPT models" || got.Groups[1].Percent != 0 {
		t.Fatalf("groups = %+v", got.Groups)
	}
}

func TestUsageEndpointNamesTheWindowWhenAGroupHasTwo(t *testing.T) {
	_, body := getUsage(t, &usageExec{buckets: []PlanBucket{{"G", "5h", 40, ""}, {"G", "weekly", 12, ""}}}, true, "tok")
	for _, want := range []string{`"rolling":{"percent":40}`, `"weekly":{"percent":12}`, `"name":"G (5h)"`, `"name":"G (weekly)"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %s: %s", want, body)
		}
	}
}

func TestUsageEndpointRefusals(t *testing.T) {
	old := &usageExec{buckets: []PlanBucket{{"G", "weekly", 1, ""}}}
	if status, _ := getUsage(t, old, false, "tok"); status != 404 || old.calls != 0 {
		t.Fatalf("an old CLI answered %d after %d calls: /usage would be a paid prompt there", status, old.calls)
	}
	if status, _ := getUsage(t, old, true, "wrong"); status != 401 || old.calls != 0 {
		t.Fatalf("wrong token answered %d", status)
	}
	if status, _ := getUsage(t, &fakeExec{}, true, "tok"); status != 404 {
		t.Fatalf("an executor without usage answered %d", status)
	}
	if status, _ := getUsage(t, &usageExec{}, true, "tok"); status != 404 {
		t.Fatalf("no limits answered %d", status)
	}
	if status, _ := getUsage(t, &usageExec{err: mapError("ERROR", "authentication required")}, true, "tok"); status != 401 {
		t.Fatalf("a logged-out CLI answered %d", status)
	}
}
