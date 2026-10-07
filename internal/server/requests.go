package server

// ── GET /admin/requests ─────────────────────────────────────────────────────
// The token dissection explorer. The mimo token plan kept raising one
// question — "where did the tokens actually GO — is the CLIENT sending too
// much, or is the MODEL burning it on output/reasoning?" — and aggregates
// hide exactly the outliers that answer it. Filter the calls ledger by
// tokens-in / tokens-out ranges, bound the window, and read a summary over
// the WHOLE matching set (not just the returned page): count, sums, maxima.
//
// Params: min_tin / max_tin / min_tout / max_tout (non-negative ints),
// from / to (RFC3339, same [from,to) semantics as /admin/usage; default
// last 24h with the same +1min grace), limit (default 50, max 500),
// sort (ts_desc default | tin_desc | tout_desc).

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// tsBoundFormat matches store.UsageReport's binding so both views compare
// timestamps against the same string encoding.
const tsBoundFormat = "2006-01-02T15:04:05.000Z"

var requestSorts = map[string]string{
	"ts_desc":   "ts DESC, id DESC",
	"tin_desc":  "tokens_in DESC, id DESC",
	"tout_desc": "tokens_out DESC, id DESC",
}

func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	if !hasRole(r, "admin") {
		writeErr(w, http.StatusForbidden, "forbidden", "admin role required")
		return
	}
	q := r.URL.Query()

	// Token bounds: empty = unbounded, else a non-negative integer.
	bounds := map[string]*int64{}
	for _, name := range []string{"min_tin", "max_tin", "min_tout", "max_tout"} {
		raw := q.Get(name)
		if raw == "" {
			continue
		}
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			writeErr(w, http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("%s must be a non-negative integer, got %q", name, raw))
			return
		}
		bounds[name] = &v
	}

	// Window: [from, to), defaulting to the last 24h with the same +1min
	// grace /admin/usage uses (ledger rows are stamped at request start and
	// written asynchronously — a strict `ts < now` would drop the freshest).
	now := time.Now().UTC()
	from, to := now.Add(-24*time.Hour), now.Add(time.Minute)
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request_error", "from must be RFC3339")
			return
		}
		from = t.UTC()
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request_error", "to must be RFC3339")
			return
		}
		to = t.UTC()
	}

	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeErr(w, http.StatusBadRequest, "invalid_request_error", "limit must be 1-500")
			return
		}
		limit = n
	}
	sortKey := q.Get("sort")
	if sortKey == "" {
		sortKey = "ts_desc"
	}
	order, ok := requestSorts[sortKey]
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("sort must be ts_desc, tin_desc or tout_desc, got %q", sortKey))
		return
	}

	// WHERE: the window always, token bounds only when set.
	where := " WHERE ts >= ? AND ts < ?"
	args := []any{from.Format(tsBoundFormat), to.Format(tsBoundFormat)}
	add := func(cond, name string, v *int64) {
		if v != nil {
			where += cond
			args = append(args, *v)
		}
	}
	add(" AND tokens_in >= ?", "min_tin", bounds["min_tin"])
	add(" AND tokens_in <= ?", "max_tin", bounds["max_tin"])
	add(" AND tokens_out >= ?", "min_tout", bounds["min_tout"])
	add(" AND tokens_out <= ?", "max_tout", bounds["max_tout"])

	// Summary first: over the WHOLE matching set, so the numbers answer
	// "how much lives behind this filter" regardless of the page size.
	var sum struct {
		Count, Tin, Tout, Cread, Reasoning, MaxTin, MaxTout int64
	}
	var sargs []any
	for _, a := range args {
		sargs = append(sargs, a)
	}
	row := s.db.Reader().QueryRowContext(r.Context(), `
SELECT COUNT(*), COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0),
       COALESCE(SUM(tokens_cached_read),0), COALESCE(SUM(reasoning_tokens),0),
       COALESCE(MAX(tokens_in),0), COALESCE(MAX(tokens_out),0)
FROM calls`+where, sargs...)
	if err := row.Scan(&sum.Count, &sum.Tin, &sum.Tout, &sum.Cread,
		&sum.Reasoning, &sum.MaxTin, &sum.MaxTout); err != nil {
		writeErr(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}

	rows, err := s.queryCalls(r.Context(), where, args, order, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"rows": rows,
		"summary": map[string]any{
			"count": sum.Count, "tin": sum.Tin, "tout": sum.Tout,
			"cread": sum.Cread, "reasoning": sum.Reasoning,
			"max_tin": sum.MaxTin, "max_tout": sum.MaxTout,
		},
		"filter": map[string]any{
			"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339),
			"min_tin": bounds["min_tin"], "max_tin": bounds["max_tin"],
			"min_tout": bounds["min_tout"], "max_tout": bounds["max_tout"],
			"limit": limit, "sort": sortKey,
		},
	})
}
