package store

import (
	"context"
	"fmt"
	"time"
)

// HopStream is one hop's stream-outcome record over a window
// (docs/hop-health-design.md). A hop that commits a 200 and then dies mid-body
// looks healthy everywhere else: it wrote a row, it has a status, its account is
// enabled. This is the number that shows it.
//
// Rate is truncated / (streams - client_gone): a caller that hung up after the
// last byte is a COMPLETE call, and counting those made a real 18-event incident
// look like a 460-event one.
type HopStream struct {
	Namespace  string  `json:"namespace"`
	Model      string  `json:"model"`
	Streams    int64   `json:"streams"`
	Completed  int64   `json:"completed"`
	Truncated  int64   `json:"truncated"`
	Failed     int64   `json:"failed"`
	ClientGone int64   `json:"client_gone"`
	Rate       float64 `json:"truncation_rate"`
}

// HopStreamHealth rolls the ledger's stream outcomes up per (namespace, model)
// over [from, to]. Rows are ordered by truncation count so the worst hop is
// first — the operator question is "who is cutting answers", never "who is
// busiest".
//
// Only rows the v34 recorder classified participate (a non-empty
// stream_outcome): historical rows carry an empty value because the column was
// never backfilled from prose, and inferring a verdict from err's wording is
// exactly the archaeology this column exists to end.
func (d *DB) HopStreamHealth(ctx context.Context, from, to time.Time) ([]HopStream, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT COALESCE(account,''), model,
       COUNT(*),
       COALESCE(SUM(CASE WHEN stream_outcome = 'completed'   THEN 1 ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN stream_outcome = 'truncated'   THEN 1 ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN stream_outcome = 'failed'      THEN 1 ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN stream_outcome = 'client_gone' THEN 1 ELSE 0 END),0)
FROM calls
WHERE ts >= ? AND ts <= ? AND stream = 1 AND stream_outcome <> ''
GROUP BY account, model
ORDER BY 5 DESC, 1 ASC, 2 ASC`,
		from.UTC().Format("2006-01-02T15:04:05.000Z"),
		to.UTC().Format("2006-01-02T15:04:05.000Z"))
	if err != nil {
		return nil, fmt.Errorf("hop stream health: %w", err)
	}
	defer rows.Close()

	out := []HopStream{}
	for rows.Next() {
		var h HopStream
		if err := rows.Scan(&h.Namespace, &h.Model, &h.Streams, &h.Completed, &h.Truncated, &h.Failed, &h.ClientGone); err != nil {
			return nil, fmt.Errorf("hop stream health: scan: %w", err)
		}
		if judged := h.Streams - h.ClientGone; judged > 0 {
			h.Rate = float64(h.Truncated) / float64(judged)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hop stream health: rows: %w", err)
	}
	return out, nil
}
