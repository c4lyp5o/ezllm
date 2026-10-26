package store

import (
	"context"
	"database/sql"
	"slices"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
)

// CatalogSnapshot is an immutable in-memory copy of the routing catalog
// (accounts, their keys, models, combos), so one resolution costs a single
// generation read instead of several queries per hop.
//
// Freshness: the schema's catalog_gen triggers bump a counter on every write
// to a table held here, and CatalogSnapshot (the DB method) reloads whenever
// the counter moved. Each answer is the one the live DB method gives: the
// loader calls those very methods, so the two can never drift.
type CatalogSnapshot struct {
	gen int64
	db  *DB

	accounts map[int64]accountAnswer
	byNS     map[string]int64
	// keys holds PickKey's answer per account. Nil while a key cooldown is
	// live: PickKey then depends on the clock, which no write announces, so
	// the snapshot asks the DB instead.
	keys       map[int64]keyAnswer
	models     map[int64]map[string]ModelFacts // a missing account = empty catalog
	combos     map[string]*Combo
	comboNames []string
	namespaces []string
}

type accountAnswer struct {
	acct *provider.Account
	err  error
}

type keyAnswer struct {
	key KeyPick
	err error
}

// catalogGen reads the generation counter the schema's triggers maintain.
func (d *DB) catalogGen(ctx context.Context) (int64, error) {
	var gen int64
	err := d.r.QueryRowContext(ctx, `SELECT gen FROM catalog_gen WHERE id = 1`).Scan(&gen)
	return gen, err
}

// CatalogSnapshot returns a snapshot no older than the last committed catalog
// write. The common case is one single-row read; after a write, the first
// caller reloads while concurrent callers wait for it.
func (d *DB) CatalogSnapshot(ctx context.Context) (*CatalogSnapshot, error) {
	gen, err := d.catalogGen(ctx)
	if err != nil {
		return nil, err
	}
	if s := d.catalog.Load(); s != nil && s.gen >= gen {
		return s, nil
	}
	d.catalogMu.Lock()
	defer d.catalogMu.Unlock()
	if s := d.catalog.Load(); s != nil && s.gen >= gen {
		return s, nil
	}
	// gen was read BEFORE loading: a write landing mid-load leaves the
	// counter ahead of this label, so the next caller reloads again.
	s, err := d.loadCatalogSnapshot(ctx, gen)
	if err != nil {
		return nil, err
	}
	d.catalog.Store(s)
	return s, nil
}

func (d *DB) loadCatalogSnapshot(ctx context.Context, gen int64) (*CatalogSnapshot, error) {
	s := &CatalogSnapshot{
		gen: gen, db: d,
		accounts: map[int64]accountAnswer{},
		byNS:     map[string]int64{},
		models:   map[int64]map[string]ModelFacts{},
		combos:   map[string]*Combo{},
	}

	rows, err := d.r.QueryContext(ctx, `SELECT id, namespace FROM accounts`)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var ns string
		if err := rows.Scan(&id, &ns); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		s.byNS[ns] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var liveCooldowns int
	if err := d.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM key_cooldowns WHERE until_ts > ?`,
		time.Now().UTC().Format("2006-01-02T15:04:05.000Z")).Scan(&liveCooldowns); err != nil {
		return nil, err
	}
	if liveCooldowns == 0 {
		s.keys = map[int64]keyAnswer{}
	}
	for _, id := range ids {
		a, err := d.AccountByID(ctx, id)
		s.accounts[id] = accountAnswer{a, err}
		if s.keys != nil {
			k, err := d.PickKey(ctx, id)
			s.keys[id] = keyAnswer{k, err}
		}
	}

	mrows, err := d.r.QueryContext(ctx, `
SELECT account_id, model_id, proto_pin, proto_chat, proto_anthropic, proto_responses FROM models`)
	if err != nil {
		return nil, err
	}
	for mrows.Next() {
		var (
			acct      int64
			model     string
			pin       sql.NullString
			oa, an, r sql.NullInt64
		)
		if err := mrows.Scan(&acct, &model, &pin, &oa, &an, &r); err != nil {
			mrows.Close()
			return nil, err
		}
		if s.models[acct] == nil {
			s.models[acct] = map[string]ModelFacts{}
		}
		s.models[acct][model] = ModelFacts{Pin: pin.String,
			Chat: nullBoolPtr(oa), Anthropic: nullBoolPtr(an), Responses: nullBoolPtr(r)}
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return nil, err
	}

	crows, err := d.r.QueryContext(ctx, `SELECT name FROM combos`)
	if err != nil {
		return nil, err
	}
	var names []string
	for crows.Next() {
		var n string
		if err := crows.Scan(&n); err != nil {
			crows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return nil, err
	}
	for _, n := range names {
		c, err := d.ComboByName(ctx, n)
		if err != nil {
			return nil, err
		}
		s.combos[n] = c
	}
	if s.comboNames, err = d.AllComboNames(ctx); err != nil {
		return nil, err
	}
	if s.namespaces, err = d.AllNamespaces(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// The methods below answer exactly as the DB methods of the same name.
// Returned values are copies: the snapshot is shared across requests.

func (s *CatalogSnapshot) AccountByID(_ context.Context, id int64) (*provider.Account, error) {
	a, ok := s.accounts[id]
	if !ok {
		return nil, ErrNotFound
	}
	if a.err != nil {
		return nil, a.err
	}
	cp := *a.acct
	return &cp, nil
}

func (s *CatalogSnapshot) AccountByNamespace(ctx context.Context, ns string) (*provider.Account, error) {
	id, ok := s.byNS[ns]
	if !ok {
		return nil, ErrNotFound
	}
	return s.AccountByID(ctx, id)
}

func (s *CatalogSnapshot) PickKey(ctx context.Context, accountID int64) (KeyPick, error) {
	if s.keys == nil {
		return s.db.PickKey(ctx, accountID)
	}
	k, ok := s.keys[accountID]
	if !ok {
		return KeyPick{}, ErrNotFound
	}
	return k.key, k.err
}

func (s *CatalogSnapshot) ModelExists(_ context.Context, accountID int64, modelID string) (bool, error) {
	m := s.models[accountID]
	if len(m) == 0 {
		return true, nil // never synced: the upstream is authoritative
	}
	_, ok := m[modelID]
	return ok, nil
}

func (s *CatalogSnapshot) ModelFacts(_ context.Context, accountID int64, modelID string) (ModelFacts, error) {
	return s.models[accountID][modelID], nil
}

func (s *CatalogSnapshot) ModelPin(_ context.Context, accountID int64, modelID string) (string, error) {
	return s.models[accountID][modelID].Pin, nil
}

func (s *CatalogSnapshot) ModelSupportsSurface(_ context.Context, accountID int64, modelID string, surface provider.Surface) (*bool, error) {
	return s.models[accountID][modelID].Supports(surface), nil
}

func (s *CatalogSnapshot) ComboByName(_ context.Context, name string) (*Combo, error) {
	c, ok := s.combos[name]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *c
	cp.Hops = slices.Clone(c.Hops)
	return &cp, nil
}

func (s *CatalogSnapshot) AllComboNames(context.Context) ([]string, error) {
	return slices.Clone(s.comboNames), nil
}

func (s *CatalogSnapshot) AllNamespaces(context.Context) ([]string, error) {
	return slices.Clone(s.namespaces), nil
}
