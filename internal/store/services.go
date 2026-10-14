package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ServiceSurfaceNames are the non-chat endpoint surfaces configurable from the
// Services page. Keep this list in one place; the server validates at its edge.
var ServiceSurfaceNames = []string{"embeddings", "images_gen", "audio_tts", "audio_stt"}

func IsServiceSurface(surface string) bool {
	for _, known := range ServiceSurfaceNames {
		if surface == known {
			return true
		}
	}
	return false
}

type ServiceBinding struct {
	Surface     string `json:"surface"`
	Mode        string `json:"mode"`
	ModelTarget string `json:"model_target"`
	UpdatedAt   string `json:"updated_at"`
}

type ServiceProbe struct {
	Surface   string `json:"surface"`
	AccountID int64  `json:"account_id"`
	ModelID   string `json:"model_id"`
	OK        bool   `json:"ok"`
	Detail    string `json:"detail"`
	TestedAt  string `json:"tested_at"`
}

func (d *DB) ListServiceBindings(ctx context.Context) ([]ServiceBinding, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT surface, mode, model_target, updated_at
FROM service_bindings ORDER BY surface`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ServiceBinding{}
	for rows.Next() {
		var b ServiceBinding
		if err := rows.Scan(&b.Surface, &b.Mode, &b.ModelTarget, &b.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (d *DB) ServiceBinding(ctx context.Context, surface string) (ServiceBinding, error) {
	var b ServiceBinding
	err := d.r.QueryRowContext(ctx, `
SELECT surface, mode, model_target, updated_at
FROM service_bindings WHERE surface=?`, surface).
		Scan(&b.Surface, &b.Mode, &b.ModelTarget, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ServiceBinding{}, ErrNotFound
	}
	return b, err
}

func (d *DB) SetServiceBinding(ctx context.Context, surface, mode, target string) error {
	if !IsServiceSurface(surface) {
		return fmt.Errorf("unknown service surface %q", surface)
	}
	if mode != "manual" && mode != "auto" {
		return fmt.Errorf("service binding mode must be manual or auto")
	}
	if mode == "manual" && strings.TrimSpace(target) == "" {
		return fmt.Errorf("manual service binding requires model_target")
	}
	if mode == "auto" {
		target = ""
	}
	_, err := d.w.Exec(`
INSERT INTO service_bindings(surface, mode, model_target, updated_at)
VALUES(?,?,?,strftime('%Y-%m-%dT%H:%M:%fZ','now'))
ON CONFLICT(surface) DO UPDATE SET
  mode=excluded.mode, model_target=excluded.model_target, updated_at=excluded.updated_at`,
		surface, mode, target)
	return err
}

func (d *DB) ClearServiceBinding(ctx context.Context, surface string) error {
	if !IsServiceSurface(surface) {
		return fmt.Errorf("unknown service surface %q", surface)
	}
	_, err := d.w.Exec(`DELETE FROM service_bindings WHERE surface=?`, surface)
	return err
}

// CountEnabledAccounts reports how many providers are actually usable — the
// number the Services cards read as "not available — connect a provider first"
// when it is zero.
func (d *DB) CountEnabledAccounts(ctx context.Context) (int, error) {
	var n int
	if err := d.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE enabled=1`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (d *DB) UpsertServiceProbe(ctx context.Context, surface string, accountID int64, modelID string, ok bool, detail string) error {
	if !IsServiceSurface(surface) {
		return fmt.Errorf("unknown service surface %q", surface)
	}
	if accountID <= 0 || strings.TrimSpace(modelID) == "" {
		return fmt.Errorf("service probe requires account and model")
	}
	value := 0
	if ok {
		value = 1
	}
	_, err := d.w.Exec(`
INSERT INTO service_probes(surface, account_id, model_id, ok, detail, tested_at)
VALUES(?,?,?,?,?,strftime('%Y-%m-%dT%H:%M:%fZ','now'))
ON CONFLICT(surface, account_id, model_id) DO UPDATE SET
  ok=excluded.ok, detail=excluded.detail, tested_at=excluded.tested_at`,
		surface, accountID, modelID, value, detail)
	return err
}

func (d *DB) ServiceProbe(ctx context.Context, surface string, accountID int64, modelID string) (ServiceProbe, error) {
	var p ServiceProbe
	var ok int
	err := d.r.QueryRowContext(ctx, `
SELECT surface, account_id, model_id, ok, detail, tested_at
FROM service_probes WHERE surface=? AND account_id=? AND model_id=?`,
		surface, accountID, modelID).
		Scan(&p.Surface, &p.AccountID, &p.ModelID, &ok, &p.Detail, &p.TestedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ServiceProbe{}, ErrNotFound
	}
	p.OK = ok != 0
	return p, err
}

func (d *DB) ListServiceProbes(ctx context.Context, surface string) ([]ServiceProbe, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT surface, account_id, model_id, ok, detail, tested_at
FROM service_probes WHERE surface=? ORDER BY tested_at DESC, account_id, model_id`, surface)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ServiceProbe{}
	for rows.Next() {
		var p ServiceProbe
		var ok int
		if err := rows.Scan(&p.Surface, &p.AccountID, &p.ModelID, &ok, &p.Detail, &p.TestedAt); err != nil {
			return nil, err
		}
		p.OK = ok != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

// ResolveServiceDefault returns the requested model string for a model-less
// service request. Manual bindings return their target. Auto chooses the most
// recently tested-ok catalog model whose account is enabled.
func (d *DB) ResolveServiceDefault(ctx context.Context, surface string) (string, error) {
	b, err := d.ServiceBinding(ctx, surface)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if b.Mode == "manual" {
		return b.ModelTarget, nil
	}
	if b.Mode != "auto" {
		return "", nil
	}
	var ns, model string
	err = d.r.QueryRowContext(ctx, `
SELECT a.namespace, p.model_id
FROM service_probes p
JOIN accounts a ON a.id=p.account_id AND a.enabled=1
JOIN models m ON m.account_id=p.account_id AND m.model_id=p.model_id
WHERE p.surface=? AND p.ok=1
ORDER BY p.tested_at DESC, a.namespace, p.model_id
LIMIT 1`, surface).Scan(&ns, &model)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return ns + "/" + model, nil
}

// ServiceTargetExists validates a direct namespaced model target without
// allowing an empty catalog to make a UI binding look valid. The split is on
// the FIRST slash only — model ids may contain slashes themselves
// (vercel/anthropic/claude-opus-5 is namespace "vercel", model
// "anthropic/claude-opus-5"), exactly like the router's splitNamespace. The
// account must be ENABLED: a default pointing at a disabled account is a
// request-time failure waiting to happen.
func (d *DB) ServiceTargetExists(ctx context.Context, target string) (bool, error) {
	i := strings.IndexByte(target, '/')
	if i <= 0 || i == len(target)-1 {
		return false, nil
	}
	acct, err := d.AccountByNamespace(ctx, target[:i])
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !acct.Enabled {
		return false, nil
	}
	return d.ModelExists(ctx, acct.ID, target[i+1:])
}
