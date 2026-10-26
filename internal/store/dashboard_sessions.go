package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"unicode"
)

type DashboardSessionInfo struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
	Browser   string `json:"browser"`
	Current   bool   `json:"current"`
}

func sessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func (d *DB) DescribeDashboardSession(ctx context.Context, token, browser string) error {
	browser = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, browser)
	runes := []rune(browser)
	if len(runes) > 160 {
		browser = string(runes[:160])
	}
	_, err := d.w.ExecContext(ctx, `UPDATE dashboard_session_details SET browser=? WHERE token_hash=?`, browser, HashToken(token))
	return err
}
func (d *DB) ListDashboardSessions(ctx context.Context, token string) ([]DashboardSessionInfo, error) {
	rows, err := d.r.QueryContext(ctx, `SELECT d.id,s.created_at,s.expires_at,d.browser,s.token_hash=? FROM dashboard_sessions s JOIN dashboard_session_details d USING(token_hash) WHERE julianday(s.expires_at)>julianday('now') ORDER BY s.created_at DESC`, HashToken(token))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DashboardSessionInfo{}
	for rows.Next() {
		var s DashboardSessionInfo
		if err := rows.Scan(&s.ID, &s.CreatedAt, &s.ExpiresAt, &s.Browser, &s.Current); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
func (d *DB) RevokeDashboardSessionID(ctx context.Context, id string) error {
	_, err := d.w.ExecContext(ctx, `DELETE FROM dashboard_sessions WHERE token_hash=(SELECT token_hash FROM dashboard_session_details WHERE id=?)`, id)
	return err
}
func (d *DB) RevokeAllDashboardSessions(ctx context.Context) error {
	_, err := d.w.ExecContext(ctx, `DELETE FROM dashboard_sessions`)
	return err
}
