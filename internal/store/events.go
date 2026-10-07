package store

import "slices"

// CallEvent is one committed ledger row, shaped for the dashboard feed.
type CallEvent struct {
	TS          string `json:"ts"`
	Client      string `json:"client"`
	Surface     string `json:"surface"`
	Alias       string `json:"alias"`
	Account     string `json:"account"`
	Model       string `json:"model"`
	Status      int    `json:"status"`
	Stream      bool   `json:"stream"`
	TTFTMs      *int64 `json:"ttft_ms,omitempty"`
	TotalMs     *int64 `json:"total_ms,omitempty"`
	TokensIn    int64  `json:"tokens_in"`
	TokensOut   int64  `json:"tokens_out"`
	CachedRead  int64  `json:"tokens_cached_read"`
	CachedWrite int64  `json:"tokens_cached_write"`
	Reasoning   int64  `json:"reasoning_tokens"`
	Saved       int64  `json:"tokens_saved"`
	// Compression is the ledger stamp: "" (never mentioned), "off" /
	// "unknown-profile" / "disabled" (asked, no change), or the profile name.
	Compression string `json:"compression"`
	Applied     bool   `json:"applied"`
	Err         string `json:"error,omitempty"`
}

// SubscribeLedger returns a channel of committed batches plus its cancel func.
// The publish pipeline is single-producer, so channel order == commit order.
// Buffers only bound memory: a full channel DROPS (never blocks the ledger),
// honoring invariant #5 — a slow dashboard must not touch inference latency.
func (d *DB) SubscribeLedger(buf int) (<-chan []Call, func()) {
	ch := make(chan []Call, buf)
	d.sinkMu.Lock()
	d.subs = append(d.subs, ch)
	d.sinkMu.Unlock()
	return ch, func() {
		d.sinkMu.Lock()
		defer d.sinkMu.Unlock()
		// Remove AND close under the same lock publish sends with, otherwise a
		// concurrent publish can send on a closed channel (panic).
		d.subs = slices.DeleteFunc(d.subs, func(c chan []Call) bool { return c == ch })
		close(ch)
	}
}

func (d *DB) publish(rows []Call) {
	// Barrier flushes commit nothing; publishing an empty batch would show up
	// downstream as a phantom "no-op" event.
	if len(rows) == 0 {
		return
	}
	// rows is already a private snapshot owned by the ledger loop.
	batch := rows
	d.sinkMu.Lock()
	defer d.sinkMu.Unlock()
	for _, ch := range d.subs {
		select { // non-blocking: a slow consumer DROPS, never stalls the ledger
		case ch <- batch:
		default:
		}
	}
}

// EventFromCall projects a ledger row onto the wire shape.
func EventFromCall(c Call) CallEvent {
	e := CallEvent{
		TS:     c.TS.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		Client: c.Client, Surface: string(c.Surface), Alias: c.Alias,
		Account: c.Account, Model: c.Model, Status: c.Status,
		Stream:   c.Stream,
		TokensIn: c.TokensIn, TokensOut: c.TokensOut,
		CachedRead: c.TokensCachedRead, CachedWrite: c.TokensCachedWrite,
		Reasoning: c.ReasoningTokens, Saved: c.TokensSaved,
		Compression: c.CompressionProfile, Applied: c.CompressionApplied,
		Err: c.Err,
	}
	if c.TTFTms != nil {
		v := *c.TTFTms
		e.TTFTMs = &v
	}
	if c.Totalms != nil {
		v := *c.Totalms
		e.TotalMs = &v
	}
	return e
}
