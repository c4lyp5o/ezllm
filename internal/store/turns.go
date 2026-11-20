package store

import (
	"context"
	"slices"
	"time"
)

// TurnLength summarises how many requests each agent-loop turn took, per
// (account, model), over a window (GET /admin/turns). A turn is the rows
// sharing (client, turn_id); the model that keeps handing control back after a
// few requests shows a low median and a small long-turn share.
type TurnLength struct {
	Account  string  `json:"account"`
	Model    string  `json:"model"`
	Turns    int     `json:"turns"`
	Requests int     `json:"requests"`
	Median   float64 `json:"median"`
	P75      int     `json:"p75"`
	P90      int     `json:"p90"`
	Max      int     `json:"max"`
	// Single is the share of turns answered in one request (no tool loop).
	Single float64 `json:"single_share"`
	// Long is the share of turns that ran LongTurn requests or more.
	Long float64 `json:"long_share"`
}

// LongTurn is the request count from which a turn counts as long work.
const LongTurn = 20

// TurnLengths reads [from, to). Rows without a turn_id (pre-v37, or bodies
// with no conversation) are left out. A turn that failed over between models
// counts once under each model it used.
func (d *DB) TurnLengths(ctx context.Context, from, to time.Time) ([]TurnLength, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT account, model, COUNT(*) FROM calls
WHERE ts >= ? AND ts < ? AND turn_id <> ''
GROUP BY client, turn_id, account, model`,
		from.UTC().Format("2006-01-02T15:04:05.000Z"), to.UTC().Format("2006-01-02T15:04:05.000Z"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type key struct{ account, model string }
	lens := map[key][]int{}
	for rows.Next() {
		var k key
		var n int
		if err := rows.Scan(&k.account, &k.model, &n); err != nil {
			return nil, err
		}
		lens[k] = append(lens[k], n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]TurnLength, 0, len(lens))
	for k, v := range lens {
		out = append(out, summariseTurns(k.account, k.model, v))
	}
	slices.SortFunc(out, func(a, b TurnLength) int {
		if a.Turns != b.Turns {
			return b.Turns - a.Turns
		}
		if a.Account != b.Account {
			return compareStr(a.Account, b.Account)
		}
		return compareStr(a.Model, b.Model)
	})
	return out, nil
}

func compareStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func summariseTurns(account, model string, v []int) TurnLength {
	slices.Sort(v)
	t := TurnLength{Account: account, Model: model, Turns: len(v), Max: v[len(v)-1]}
	single, long := 0, 0
	for _, n := range v {
		t.Requests += n
		if n == 1 {
			single++
		}
		if n >= LongTurn {
			long++
		}
	}
	if len(v)%2 == 1 {
		t.Median = float64(v[len(v)/2])
	} else {
		t.Median = float64(v[len(v)/2-1]+v[len(v)/2]) / 2
	}
	t.P75 = v[(len(v)*75)/100]
	t.P90 = v[(len(v)*90)/100]
	t.Single = float64(single) / float64(len(v))
	t.Long = float64(long) / float64(len(v))
	return t
}
