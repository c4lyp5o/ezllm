package server

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

// Prometheus metrics (GET /metrics). Design: docs/metrics.md.
//
// Counters and histograms are fed by store.ObserveLedger: the ledger goroutine
// hands every committed batch to observe, so the request path does no metrics
// work at all and a scrape counts exactly what the ledger committed. Gauges
// (in-flight requests, bridge CLIs, runtime) are read at scrape time. The text
// exposition format is written by hand: no client library, no new dependency.

// metricsSeriesCap bounds each labelled family. The model label can carry a
// caller-supplied name on a refused request; past the cap new models fold into
// model="_other" so a client cannot grow the scrape without bound.
const metricsSeriesCap = 2000

// Latency buckets in seconds: interactive answers through long agent turns.
var metricsBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

type reqKey struct {
	surface, account, model string
	status                  int
}
type modelKey struct{ account, model string }
type latKey struct{ surface, account string }

type tokenCounts struct{ in, out, cachedRead, cachedWrite, reasoning, saved int64 }

type histogram struct {
	counts []uint64 // per bucket, non-cumulative; the last slot is +Inf
	sum    float64
	n      uint64
}

func (h *histogram) add(seconds float64) {
	if h.counts == nil {
		h.counts = make([]uint64, len(metricsBuckets)+1)
	}
	i, _ := slices.BinarySearch(metricsBuckets, seconds)
	h.counts[i]++
	h.sum += seconds
	h.n++
}

type metricsCollector struct {
	started time.Time

	mu       sync.Mutex
	requests map[reqKey]uint64
	tokens   map[modelKey]*tokenCounts
	duration map[latKey]*histogram
	ttft     map[latKey]*histogram
}

func newMetricsCollector(started time.Time) *metricsCollector {
	return &metricsCollector{started: started,
		requests: map[reqKey]uint64{}, tokens: map[modelKey]*tokenCounts{},
		duration: map[latKey]*histogram{}, ttft: map[latKey]*histogram{}}
}

// observe runs on the ledger goroutine for every committed batch.
func (m *metricsCollector) observe(rows []store.Call) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range rows {
		c := &rows[i]
		surface, model := string(c.Surface), c.Model
		rk := reqKey{surface, c.Account, model, c.Status}
		if _, ok := m.requests[rk]; !ok && len(m.requests) >= metricsSeriesCap {
			rk.model = "_other"
		}
		m.requests[rk]++

		mk := modelKey{c.Account, model}
		t := m.tokens[mk]
		if t == nil {
			if len(m.tokens) >= metricsSeriesCap {
				mk.model = "_other"
				t = m.tokens[mk]
			}
			if t == nil {
				t = &tokenCounts{}
				m.tokens[mk] = t
			}
		}
		t.in += c.TokensIn
		t.out += c.TokensOut
		t.cachedRead += c.TokensCachedRead
		t.cachedWrite += c.TokensCachedWrite
		t.reasoning += c.ReasoningTokens
		t.saved += c.TokensSaved

		lk := latKey{surface, c.Account}
		if c.Totalms != nil {
			histOf(m.duration, lk).add(float64(*c.Totalms) / 1000)
		}
		if c.TTFTms != nil {
			histOf(m.ttft, lk).add(float64(*c.TTFTms) / 1000)
		}
	}
}

// histOf: latency families are keyed by surface and account only, both
// operator-defined, so they need no cap.
func histOf(fam map[latKey]*histogram, k latKey) *histogram {
	h := fam[k]
	if h == nil {
		h = &histogram{}
		fam[k] = h
	}
	return h
}

// handleMetrics serves the scrape to a key with the metrics or admin role, or
// to the dashboard session. A metrics key reads only this route: it cannot
// call models (infer) or reach /admin.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.metrics == nil {
		http.NotFound(w, r)
		return
	}
	serve := func(w http.ResponseWriter, _ *http.Request) { s.writeMetrics(w) }
	if tok := bearerToken(r); tok != "" && (s.db == nil || !s.db.DashboardSession(r.Context(), tok)) {
		s.authed(func(w http.ResponseWriter, r *http.Request) {
			if hasRole(r, "admin") {
				serve(w, r)
				return
			}
			s.needRole("metrics", serve)(w, r)
		})(w, r)
		return
	}
	s.admin(serve)(w, r) // the dashboard session (bearer or cookie)
}

func (s *Server) writeMetrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	var b strings.Builder
	s.metrics.write(&b)
	s.writeGauges(&b)
	_, _ = io.WriteString(w, b.String())
}

func (m *metricsCollector) write(b *strings.Builder) {
	m.mu.Lock()
	defer m.mu.Unlock()

	metricFamily(b, "ezllm_requests_total", "counter", "Completed requests recorded in the ledger, by surface, provider account, upstream model and HTTP status.")
	for _, k := range sortedKeys(m.requests, func(k reqKey) string {
		return fmt.Sprintf("%s\x00%s\x00%s\x00%03d", k.surface, k.account, k.model, k.status)
	}) {
		metricSample(b, "ezllm_requests_total", metricLabels("surface", k.surface, "account", k.account, "model", k.model, "status", strconv.Itoa(k.status)), float64(m.requests[k]))
	}

	metricFamily(b, "ezllm_tokens_total", "counter", "Tokens recorded in the ledger, by provider account, upstream model and kind.")
	tokenKeys := sortedKeys(m.tokens, func(k modelKey) string { return k.account + "\x00" + k.model })
	for _, k := range tokenKeys {
		t := m.tokens[k]
		for _, kv := range [...]struct {
			kind string
			v    int64
		}{{"input", t.in}, {"output", t.out}, {"cached_read", t.cachedRead}, {"cached_write", t.cachedWrite}, {"reasoning", t.reasoning}} {
			metricSample(b, "ezllm_tokens_total", metricLabels("account", k.account, "model", k.model, "kind", kv.kind), float64(kv.v))
		}
	}

	metricFamily(b, "ezllm_compression_saved_tokens_total", "counter", "Prompt tokens removed by compression before the request went upstream.")
	for _, k := range tokenKeys {
		metricSample(b, "ezllm_compression_saved_tokens_total", metricLabels("account", k.account, "model", k.model), float64(m.tokens[k].saved))
	}

	writeHistograms(b, "ezllm_request_duration_seconds", "Total request time, from admission to the last byte.", m.duration)
	writeHistograms(b, "ezllm_time_to_first_token_seconds", "Time to the first streamed token.", m.ttft)
}

func writeHistograms(b *strings.Builder, name, help string, fam map[latKey]*histogram) {
	metricFamily(b, name, "histogram", help)
	for _, k := range sortedKeys(fam, func(k latKey) string { return k.surface + "\x00" + k.account }) {
		h := fam[k]
		base := metricLabels("surface", k.surface, "account", k.account)
		var cum uint64
		for i, le := range metricsBuckets {
			cum += h.counts[i]
			metricSample(b, name+"_bucket", base+`,le="`+formatFloat(le)+`"`, float64(cum))
		}
		metricSample(b, name+"_bucket", base+`,le="+Inf"`, float64(h.n))
		metricSample(b, name+"_sum", base, h.sum)
		metricSample(b, name+"_count", base, float64(h.n))
	}
}

// writeGauges: point-in-time values read at scrape time.
func (s *Server) writeGauges(b *strings.Builder) {
	metricFamily(b, "ezllm_build_info", "gauge", "The running ezllm version.")
	metricSample(b, "ezllm_build_info", metricLabels("version", Version), 1)
	metricFamily(b, "ezllm_start_time_seconds", "gauge", "Process start time, Unix seconds.")
	metricSample(b, "ezllm_start_time_seconds", "", float64(s.metrics.started.Unix()))

	if g := s.admission; g != nil {
		g.mu.Lock()
		active, limit := g.active, g.limits.GlobalInflight
		g.mu.Unlock()
		metricFamily(b, "ezllm_inflight_requests", "gauge", "Inference requests currently admitted.")
		metricSample(b, "ezllm_inflight_requests", "", float64(active))
		metricFamily(b, "ezllm_inflight_limit", "gauge", "The configured global in-flight limit.")
		metricSample(b, "ezllm_inflight_limit", "", float64(limit))
	}

	metricFamily(b, "ezllm_ledger_lost_rows_total", "counter", "Ledger rows discarded after a failed flush and retry.")
	metricSample(b, "ezllm_ledger_lost_rows_total", "", float64(store.LostRows()))
	metricFamily(b, "ezllm_ledger_dropped_rows_total", "counter", "Ledger rows dropped because the write queue was full.")
	metricSample(b, "ezllm_ledger_dropped_rows_total", "", float64(store.DroppedCalls()))
	metricFamily(b, "ezllm_ledger_unmetered_rows_total", "counter", "Committed ledger rows the usage meter did not count.")
	metricSample(b, "ezllm_ledger_unmetered_rows_total", "", float64(store.UnmeteredRows()))

	if s.bridges != nil {
		metricFamily(b, "ezllm_bridge_cli_info", "gauge", "Running subscription bridges, their pinned CLI version and the updater's last result.")
		s.bridges.mu.RLock()
		for _, br := range s.bridges.list {
			if !br.Enabled {
				continue
			}
			version, result := "", ""
			if br.Update != nil {
				version, result = br.Update.Current, br.Update.Result
			}
			metricSample(b, "ezllm_bridge_cli_info", metricLabels("bridge", br.ID, "version", version, "update_result", result), 1)
		}
		s.bridges.mu.RUnlock()
	}

	// runtime/metrics reads without stopping the world (runtime.ReadMemStats
	// would), so a scrape cannot pause request handling.
	rs := []metrics.Sample{{Name: "/sched/goroutines:goroutines"}, {Name: "/memory/classes/heap/objects:bytes"}, {Name: "/memory/classes/total:bytes"}}
	metrics.Read(rs)
	for i, f := range [...]struct{ name, help string }{
		{"go_goroutines", "Goroutines that currently exist."},
		{"go_memory_heap_objects_bytes", "Memory occupied by live and not-yet-swept heap objects."},
		{"go_memory_total_bytes", "All memory mapped by the Go runtime."},
	} {
		if rs[i].Value.Kind() == metrics.KindUint64 {
			metricFamily(b, f.name, "gauge", f.help)
			metricSample(b, f.name, "", float64(rs[i].Value.Uint64()))
		}
	}
}

func metricFamily(b *strings.Builder, name, kind, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

func metricSample(b *strings.Builder, name, lbls string, v float64) {
	b.WriteString(name)
	if lbls != "" {
		b.WriteString("{" + lbls + "}")
	}
	b.WriteString(" " + formatFloat(v) + "\n")
}

// metricLabels renders k="v" pairs with the exposition format's escaping.
func metricLabels(kv ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(kv[i] + `="` + labelEscaper.Replace(kv[i+1]) + `"`)
	}
	return b.String()
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func formatFloat(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
		return strconv.FormatInt(int64(v), 10) // counters, timestamps: no exponent
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// sortedKeys gives a stable scrape order (diffable, testable).
func sortedKeys[K comparable, V any](m map[K]V, sortKey func(K) string) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b K) int { return strings.Compare(sortKey(a), sortKey(b)) })
	return keys
}
