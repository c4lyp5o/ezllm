package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

func TestReportCacheSharesAndExpires(t *testing.T) {
	now := time.Unix(1700000000, 0)
	c := newReportCache(10*time.Second, func() time.Time { return now })
	var computes atomic.Int32
	fn := func() (any, error) { computes.Add(1); return "rows", nil }

	if v, _, err := c.do("k", fn); err != nil || v != "rows" {
		t.Fatalf("first = %v %v", v, err)
	}
	now = now.Add(9 * time.Second)
	if _, age, _ := c.do("k", fn); computes.Load() != 1 || age != 9*time.Second {
		t.Fatalf("within ttl: computes=%d age=%s, want 1 and 9s", computes.Load(), age)
	}
	now = now.Add(2 * time.Second)
	if _, age, _ := c.do("k", fn); computes.Load() != 2 || age != 0 {
		t.Fatalf("past ttl: computes=%d age=%s, want a fresh compute", computes.Load(), age)
	}
}

// Simultaneous identical requests (several dashboard tabs) run one query.
func TestReportCacheCoalescesConcurrentCallers(t *testing.T) {
	c := newReportCache(10*time.Second, time.Now)
	release := make(chan struct{})
	var computes atomic.Int32
	fn := func() (any, error) { computes.Add(1); <-release; return 1, nil }
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.do("k", fn) }()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := computes.Load(); n != 1 {
		t.Fatalf("computes = %d, want 1 for 5 concurrent callers", n)
	}
}

// A failed report is returned to its callers but never served from cache.
func TestReportCacheDoesNotKeepErrors(t *testing.T) {
	c := newReportCache(10*time.Second, time.Now)
	boom := errors.New("db busy")
	if _, _, err := c.do("k", func() (any, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the compute error", err)
	}
	if v, _, err := c.do("k", func() (any, error) { return "ok", nil }); err != nil || v != "ok" {
		t.Fatalf("after an error = %v %v, want a fresh compute", v, err)
	}
}

// Through the endpoint: polls inside reportTTL share one report (rows written
// meanwhile appear once it lapses), and the Age header says how old it is.
func TestUsageEndpointSharesReportWithinTTL(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	now := time.Now()
	h.server.reports = newReportCache(reportTTL, func() time.Time { return now })
	record := func() {
		h.db.RecordCall(store.Call{TS: time.Now().UTC(), Client: "hermes", Account: "up", Model: "m", Status: 200, TokensIn: 10})
		if err := h.db.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	calls := func() (int64, string) {
		code, hdr, body := h.do("GET", "/admin/usage?group_by=account", "tok-admin", "")
		if code != 200 {
			t.Fatalf("usage = %d %s", code, body)
		}
		var out struct {
			Totals struct {
				Calls int64 `json:"calls"`
			} `json:"totals"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		return out.Totals.Calls, hdr.Get("Age")
	}
	record()
	if n, _ := calls(); n != 1 {
		t.Fatalf("first report calls = %d, want 1", n)
	}
	record()
	now = now.Add(4 * time.Second)
	if n, age := calls(); n != 1 || age != "4" {
		t.Fatalf("inside the ttl: calls=%d Age=%q, want the shared report (1, \"4\")", n, age)
	}
	now = now.Add(reportTTL)
	if n, _ := calls(); n != 2 {
		t.Fatalf("after the ttl: calls = %d, want 2", n)
	}
}
