package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Notification kinds: what the dashboard bell can say. Kept as constants so
// producers and the UI agree on the vocabulary.
const (
	NoteFreeModelsAdded      = "free_models_added"
	NoteFreeModelsRemoved    = "free_models_removed"
	NoteFreeModelBroken      = "free_model_broken"
	NoteCatalogRefreshFailed = "catalog_refresh_failed"
	NoteClientBudgetReached  = "client_budget_reached"
	NotePriceRefreshFailed   = "price_refresh_failed"
	NoteBridgeUpdated        = "bridge_updated"
	NoteBridgeUpdateBlocked  = "bridge_update_blocked"
	NoteBridgeRolledBack     = "bridge_rolled_back"
)

// Notification is one bell entry.
type Notification struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	Severity  string          `json:"severity"`
	Title     string          `json:"title"`
	Body      string          `json:"body,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Count     int             `json:"count"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
	ReadAt    *string         `json:"read_at"`
}

// NotificationInput is what a producer hands Notify. DedupKey, when set,
// folds repeats of the same event into its one unread row.
type NotificationInput struct {
	Kind     string
	Severity string // info (default) | warn | error
	Title    string
	Body     string
	Data     any
	DedupKey string
}

// notificationKeep bounds the table: read rows older than this go, and the
// table never holds more than notificationCap rows.
const (
	notificationKeep = "-30 days"
	notificationCap  = 500
)

// Notify records a notification. A repeat of an unread event with the same
// DedupKey updates that row (count + 1, newest title/body/data) rather than
// adding one, so a recurring failure is one line with a counter, not a flood.
func (d *DB) Notify(ctx context.Context, in NotificationInput) error {
	if m := d.operatorSettings.Load(); m != nil && !m.Current().Notify(in.Kind) {
		return nil
	}
	if strings.TrimSpace(in.Kind) == "" || strings.TrimSpace(in.Title) == "" {
		return errors.New("store: notification needs a kind and a title")
	}
	switch in.Severity {
	case "":
		in.Severity = "info"
	case "info", "warn", "error":
	default:
		return fmt.Errorf("store: notification severity %q", in.Severity)
	}
	data := ""
	if in.Data != nil {
		b, err := json.Marshal(in.Data)
		if err != nil {
			return fmt.Errorf("store: notification data: %w", err)
		}
		data = string(b)
	}
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if in.DedupKey != "" {
			res, err := tx.ExecContext(ctx, `
UPDATE notifications SET title=?, body=?, data=?, severity=?, count=count+1,
       updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now')
WHERE dedup_key=? AND read_at IS NULL`, in.Title, in.Body, data, in.Severity, in.DedupKey)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				return nil
			}
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO notifications (kind, severity, title, body, data, dedup_key) VALUES (?,?,?,?,?,?)`,
			in.Kind, in.Severity, in.Title, in.Body, data, in.DedupKey); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
DELETE FROM notifications WHERE read_at IS NOT NULL AND updated_at < strftime('%Y-%m-%dT%H:%M:%fZ','now', ?)`,
			notificationKeep); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
DELETE FROM notifications WHERE id NOT IN (SELECT id FROM notifications ORDER BY updated_at DESC, id DESC LIMIT ?)`,
			notificationCap)
		return err
	})
}

// ListNotifications returns the newest notifications first (by last update)
// and the unread count across the whole table.
func (d *DB) ListNotifications(ctx context.Context, limit int) ([]Notification, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := d.r.QueryContext(ctx, `
SELECT id, kind, severity, title, body, data, count, created_at, updated_at, read_at
FROM notifications ORDER BY updated_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		var data string
		var readAt sql.NullString
		if err := rows.Scan(&n.ID, &n.Kind, &n.Severity, &n.Title, &n.Body, &data, &n.Count,
			&n.CreatedAt, &n.UpdatedAt, &readAt); err != nil {
			return nil, 0, err
		}
		if data != "" {
			n.Data = json.RawMessage(data)
		}
		if readAt.Valid {
			n.ReadAt = &readAt.String
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var unread int
	if err := d.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications WHERE read_at IS NULL`).Scan(&unread); err != nil {
		return nil, 0, err
	}
	return out, unread, nil
}

// MarkNotificationsRead marks the given ids (or every unread one, when all)
// as read. Unknown ids are ignored.
func (d *DB) MarkNotificationsRead(ctx context.Context, ids []int64, all bool) error {
	return d.WithWriteTx(ctx, func(tx *sql.Tx) error {
		if all {
			_, err := tx.ExecContext(ctx,
				`UPDATE notifications SET read_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE read_at IS NULL`)
			return err
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx,
				`UPDATE notifications SET read_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=? AND read_at IS NULL`, id); err != nil {
				return err
			}
		}
		return nil
	})
}
