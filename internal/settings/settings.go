// Package settings resolves deployment configuration and persisted operator settings.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
)

type Values struct {
	UsageTZ          string `json:"usage_timezone"`
	Translation      bool   `json:"translation_enabled"`
	AutoProbe        bool   `json:"automatic_probing"`
	CatalogInterval  string `json:"catalog_interval"`
	PriceRefresh     bool   `json:"price_refresh"`
	UpdateCheck      bool   `json:"update_check"`
	Retries          int    `json:"retries"`
	AttemptBudget    int    `json:"attempt_budget"`
	DispatchDeadline string `json:"dispatch_deadline"`
	Cooldown429      string `json:"cooldown_429"`
	Cooldown5xx      string `json:"cooldown_5xx"`
	CooldownStrikes  int    `json:"cooldown_strikes"`
	BackupEnabled    bool   `json:"backup_enabled"`
	BackupHour       int    `json:"backup_hour"`
	BackupKeep       int    `json:"backup_keep"`
	NotifyAdded      bool   `json:"notify_free_models_added"`
	NotifyRemoved    bool   `json:"notify_free_models_removed"`
	NotifyBroken     bool   `json:"notify_free_model_broken"`
	NotifyCatalog    bool   `json:"notify_catalog_refresh_failed"`
	NotifyBudget     bool   `json:"notify_client_budget_reached"`
	NotifyPrices     bool   `json:"notify_price_refresh_failed"`
	NotifyBackup     bool   `json:"notify_backup_failed"`
	NotifyBridgeUp   bool   `json:"notify_bridge_updated"`
	NotifyBridgeHeld bool   `json:"notify_bridge_update_blocked"`
	NotifyBridgeBack bool   `json:"notify_bridge_rolled_back"`
	NotifyStreamCut  bool   `json:"notify_stream_truncation"`
	NotifyUpdate     bool   `json:"notify_update_available"`
	// Early warnings, read by the hourly alert check: a quota window at or
	// over AlertQuotaPct, a prepaid balance under AlertBalance, a session
	// cookie expiring within AlertCookieHours.
	NotifyQuota      bool    `json:"notify_quota_warning"`
	AlertQuotaPct    int     `json:"alert_quota_percent"`
	NotifyBalance    bool    `json:"notify_balance_low"`
	AlertBalance     float64 `json:"alert_balance_below"`
	NotifyCookie     bool    `json:"notify_cookie_expiring"`
	AlertCookieHours int     `json:"alert_cookie_hours"`
	// LedgerRetentionDays rolls calls older than this into daily totals and
	// deletes them. 0 keeps every call.
	LedgerRetentionDays int `json:"ledger_retention_days"`
	// GeminiQueryKey accepts an ezllm key in the ?key= query string on the
	// Gemini surface, as Google's own API does. Off: a URL lands in proxy and
	// browser logs, and the key with it.
	GeminiQueryKey bool `json:"gemini_query_key"`
}

func Defaults() Values {
	return Values{UsageTZ: "UTC", Translation: true, AutoProbe: true, CatalogInterval: "12h", PriceRefresh: true, UpdateCheck: true, Retries: 3, AttemptBudget: 8, Cooldown429: "60s", Cooldown5xx: "60s", CooldownStrikes: 3, BackupHour: 3, BackupKeep: 7, NotifyAdded: true, NotifyRemoved: true, NotifyBroken: true, NotifyCatalog: true, NotifyBudget: true, NotifyPrices: true, NotifyBackup: true, NotifyBridgeUp: true, NotifyBridgeHeld: true, NotifyBridgeBack: true, NotifyStreamCut: true, NotifyUpdate: true, NotifyQuota: true, AlertQuotaPct: 80, NotifyBalance: true, AlertBalance: 2, NotifyCookie: true, AlertCookieHours: 48}
}
func (v Values) Validate() error {
	if _, err := time.LoadLocation(v.UsageTZ); err != nil || v.UsageTZ == "" {
		return errors.New("usage_timezone must be an IANA timezone")
	}
	if v.Retries < 0 || v.Retries > 9 {
		return errors.New("retries must be 0–9")
	}
	if v.AttemptBudget < 0 || v.AttemptBudget > 64 {
		return errors.New("attempt_budget must be 0–64 (0 means unlimited)")
	}
	if v.CooldownStrikes < 1 {
		return errors.New("cooldown_strikes must be a positive integer")
	}
	for name, raw := range map[string]string{"catalog_interval": v.CatalogInterval, "cooldown_429": v.Cooldown429, "cooldown_5xx": v.Cooldown5xx, "dispatch_deadline": v.DispatchDeadline} {
		if name == "dispatch_deadline" && raw == "" {
			continue
		}
		d, err := time.ParseDuration(raw)
		minimum := time.Second
		if name == "cooldown_429" || name == "cooldown_5xx" {
			minimum = time.Nanosecond
		}
		if name == "catalog_interval" {
			minimum = time.Minute
		}
		if err != nil || d < minimum {
			return fmt.Errorf("%s must be a duration of at least %s", name, minimum)
		}
	}
	if v.BackupHour < 0 || v.BackupHour > 23 {
		return errors.New("backup_hour must be 0–23")
	}
	if v.BackupKeep < 1 || v.BackupKeep > 365 {
		return errors.New("backup_keep must be 1–365")
	}
	if v.AlertQuotaPct < 1 || v.AlertQuotaPct > 100 {
		return errors.New("alert_quota_percent must be 1–100")
	}
	if v.AlertBalance <= 0 || v.AlertBalance > 1_000_000 {
		return errors.New("alert_balance_below must be more than 0")
	}
	if v.AlertCookieHours < 1 || v.AlertCookieHours > 720 {
		return errors.New("alert_cookie_hours must be 1–720")
	}
	if v.LedgerRetentionDays != 0 && (v.LedgerRetentionDays < 32 || v.LedgerRetentionDays > 3650) {
		return errors.New("ledger_retention_days must be 0 (keep every call) or 32–3650")
	}
	return nil
}
func (v Values) Location() *time.Location {
	l, _ := time.LoadLocation(v.UsageTZ)
	if l == nil {
		return time.UTC
	}
	return l
}
func Duration(s string) time.Duration { d, _ := time.ParseDuration(s); return d }
func (v Values) Notify(kind string) bool {
	switch kind {
	case "free_models_added":
		return v.NotifyAdded
	case "free_models_removed":
		return v.NotifyRemoved
	case "free_model_broken":
		return v.NotifyBroken
	case "catalog_refresh_failed":
		return v.NotifyCatalog
	case "client_budget_reached":
		return v.NotifyBudget
	case "price_refresh_failed":
		return v.NotifyPrices
	case "backup_failed":
		return v.NotifyBackup
	case "bridge_updated":
		return v.NotifyBridgeUp
	case "bridge_update_blocked":
		return v.NotifyBridgeHeld
	case "bridge_rolled_back":
		return v.NotifyBridgeBack
	case "stream_truncation":
		return v.NotifyStreamCut
	case "update_available":
		return v.NotifyUpdate
	case "quota_warning":
		return v.NotifyQuota
	case "balance_low":
		return v.NotifyBalance
	case "cookie_expiring":
		return v.NotifyCookie
	case "quota_read_refused":
		return v.NotifyQuota || v.NotifyBalance
	}
	return true
}

type contextKey struct{}

func With(ctx context.Context, v Values) context.Context {
	return context.WithValue(ctx, contextKey{}, v)
}
func From(ctx context.Context) (Values, bool) {
	v, ok := ctx.Value(contextKey{}).(Values)
	return v, ok
}

type State struct {
	Revision int64                      `json:"revision"`
	Values   map[string]json.RawMessage `json:"values"`
}
type Store interface {
	LoadSettings(context.Context) (State, error)
	SaveSettings(context.Context, State, int64) error
}

var ErrPersistence = errors.New("settings persistence failed")

var ErrConflict = errors.New("settings changed; reload and try again")

type Field struct {
	Value    any    `json:"value"`
	Source   string `json:"source"`
	Editable bool   `json:"editable"`
	Saved    bool   `json:"saved"`
	Apply    string `json:"apply"`
}
type View struct {
	Revision int64            `json:"revision"`
	Fields   map[string]Field `json:"fields"`
}
type Manager struct {
	mu      sync.Mutex
	store   Store
	base    Values
	sources map[string]string
	state   State
	current atomic.Pointer[Values]
	// Check validates deployment-dependent capabilities before persistence.
	Check func(Values) error
	// OnChange runs after a saved change takes effect (under the manager's
	// lock: keep it cheap and non-blocking).
	OnChange func(Values)
}

func New(ctx context.Context, store Store, base Values, sources map[string]string) (*Manager, error) {
	m := &Manager{store: store, base: base, sources: map[string]string{}}
	for k, v := range sources {
		m.sources[k] = v
	}
	state, err := store.LoadSettings(ctx)
	if err != nil {
		return nil, err
	}
	m.state = state
	v, err := m.resolve(state.Values)
	if err != nil {
		return nil, err
	}
	m.current.Store(&v)
	return m, nil
}
func (m *Manager) Current() Values { return *m.current.Load() }
func keys(v Values) map[string]json.RawMessage {
	b, _ := json.Marshal(v)
	out := map[string]json.RawMessage{}
	_ = json.Unmarshal(b, &out)
	return out
}
func (m *Manager) resolve(saved map[string]json.RawMessage) (Values, error) {
	raw := keys(m.base)
	for k, v := range saved {
		if _, ok := raw[k]; !ok {
			return Values{}, fmt.Errorf("unknown setting %s", k)
		}
		if m.sources[k] != "environment" && m.sources[k] != "flag" {
			raw[k] = v
		}
	}
	b, _ := json.Marshal(raw)
	var v Values
	if err := json.Unmarshal(b, &v); err != nil {
		return v, errors.New("settings have an invalid type")
	}
	return v, v.Validate()
}
func (m *Manager) View() View { m.mu.Lock(); defer m.mu.Unlock(); return m.view() }
func (m *Manager) view() View {
	result := View{Revision: m.state.Revision, Fields: map[string]Field{}}
	v := reflect.ValueOf(m.Current())
	typ := v.Type()
	for i := 0; i < v.NumField(); i++ {
		k := typ.Field(i).Tag.Get("json")
		source := m.sources[k]
		if source == "" {
			source = "default"
		}
		_, saved := m.state.Values[k]
		locked := source == "environment" || source == "flag"
		if saved && !locked {
			source = "settings"
		}
		result.Fields[k] = Field{Value: v.Field(i).Interface(), Source: source, Editable: !locked, Saved: saved, Apply: "immediate"}
	}
	return result
}

// Validate reports whether changes would be accepted by Patch, without
// saving anything: configuration import checks a file's settings this way
// before it writes the rest.
func (m *Manager) Validate(changes map[string]json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, _, err := m.apply(changes)
	return err
}

// apply works out the state and values changes would produce. The caller
// holds m.mu.
func (m *Manager) apply(changes map[string]json.RawMessage) (State, Values, error) {
	next := State{Values: map[string]json.RawMessage{}}
	for k, v := range m.state.Values {
		next.Values[k] = v
	}
	allowed := keys(m.base)
	for k, v := range changes {
		if _, ok := allowed[k]; !ok {
			return State{}, Values{}, fmt.Errorf("unknown setting %s", k)
		}
		if m.sources[k] == "environment" || m.sources[k] == "flag" {
			return State{}, Values{}, fmt.Errorf("%s is managed by %s", k, m.sources[k])
		}
		if string(v) == "null" {
			delete(next.Values, k)
		} else {
			next.Values[k] = v
		}
	}
	v, err := m.resolve(next.Values)
	if err != nil {
		return State{}, Values{}, err
	}
	if m.Check != nil {
		if err := m.Check(v); err != nil {
			return State{}, Values{}, err
		}
	}
	return next, v, nil
}

func (m *Manager) Patch(ctx context.Context, revision int64, changes map[string]json.RawMessage) (View, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if revision != m.state.Revision {
		return View{}, ErrConflict
	}
	next, v, err := m.apply(changes)
	if err != nil {
		return View{}, err
	}
	next.Revision = revision + 1
	if err := m.store.SaveSettings(ctx, next, revision); err != nil {
		if errors.Is(err, ErrConflict) {
			return View{}, err
		}
		return View{}, fmt.Errorf("%w: %v", ErrPersistence, err)
	}
	m.state = next
	m.current.Store(&v)
	if m.OnChange != nil {
		m.OnChange(v)
	}
	return m.view(), nil
}

type meterKey struct{}
type Meter struct {
	Window string
	Now    time.Time
}

func WithMeter(ctx context.Context, window string, now time.Time) context.Context {
	return context.WithValue(ctx, meterKey{}, Meter{window, now})
}
func RuleMeter(ctx context.Context) (Meter, bool) {
	v, ok := ctx.Value(meterKey{}).(Meter)
	return v, ok
}
