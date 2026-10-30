package server

// External push for bell events (internal/notify): Settings → Notifications
// lists channels — ntfy, Discord, Telegram, a JSON webhook — and every new
// bell event that meets a channel's minimum severity is sent there. Delivery
// is off the request path: notify() queues the event, one background worker
// sends it, and the outcome is the channel's status, never a bell event (a
// failing channel must not notify about itself).

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/c4lyp5o/ezllm/internal/notify"
	"github.com/c4lyp5o/ezllm/internal/store"
)

const (
	pushQueueSize = 64
	// pushRepeat is how long a folded repeat (count + 1 on an unread row)
	// waits before it is pushed again: a recurring failure reaches the phone
	// once a day, not once per occurrence.
	pushRepeat = 24 * time.Hour
)

type pusher struct {
	queue  chan notify.Message
	client *http.Client
	mu     sync.Mutex
	last   map[string]time.Time // dedup key → when it was last queued
}

func newPusher() *pusher {
	return &pusher{queue: make(chan notify.Message, pushQueueSize), client: notify.Client(), last: map[string]time.Time{}}
}

// queuePush hands a recorded bell event to the worker: a new row always, a
// folded repeat once per pushRepeat. It never blocks; a full queue drops the
// event and says so.
func (s *Server) queuePush(in store.NotificationInput, result store.NotifyResult) {
	if s.push == nil || result == store.NotifyMuted {
		return
	}
	now := time.Now()
	if in.DedupKey != "" {
		s.push.mu.Lock()
		last, seen := s.push.last[in.DedupKey]
		if result == store.NotifyFolded && seen && now.Sub(last) < pushRepeat {
			s.push.mu.Unlock()
			return
		}
		s.push.last[in.DedupKey] = now
		if len(s.push.last) > 512 {
			for k, t := range s.push.last {
				if now.Sub(t) >= pushRepeat {
					delete(s.push.last, k)
				}
			}
		}
		s.push.mu.Unlock()
	}
	severity := in.Severity
	if severity == "" {
		severity = "info"
	}
	m := notify.Message{Kind: in.Kind, Severity: severity, Title: in.Title, Body: in.Body, CreatedAt: now.UTC().Format(time.RFC3339)}
	select {
	case s.push.queue <- m:
	default:
		s.log.Warn("notification push failed", "channel", 0, "kind", in.Kind, "reason", "the push queue is full; event dropped")
	}
}

// StartPush runs the delivery worker until shutdown.
func (s *Server) StartPush(ctx context.Context) {
	if s.db == nil {
		return
	}
	s.goBackground(func(bg context.Context) {
		ctx, stop := either(ctx, bg)
		defer stop()
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-s.push.queue:
				s.deliver(ctx, m)
			}
		}
	})
}

func (s *Server) deliver(ctx context.Context, m notify.Message) {
	channels, err := s.db.ListNotificationChannels(ctx)
	if err != nil {
		s.log.Warn("notification push failed", "channel", 0, "kind", m.Kind, "reason", "cannot read the channels: "+err.Error())
		return
	}
	for _, c := range channels {
		if !c.Enabled || !notify.Meets(m.Severity, c.MinSeverity) {
			continue
		}
		s.recordDelivery(c, notify.Send(ctx, s.push.client, c.Kind, c.Config, c.Secrets(), m, notify.Attempts))
	}
}

// recordDelivery stores a delivery's outcome on the channel; a failure is
// also logged. The reason has already been cleared of secrets (notify.Send).
func (s *Server) recordDelivery(c store.NotificationChannel, err error) {
	reason := ""
	if err != nil {
		reason = err.Error()
		s.log.Warn("notification push failed", "channel", c.ID, "kind", c.Kind, "reason", reason)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.db.RecordChannelDelivery(ctx, c.ID, reason)
}

// ── API ──────────────────────────────────────────────────────────────────────

// channelView is a channel as the dashboard sees it: no secret values, the
// names of the set ones, and where it delivers in words.
type channelView struct {
	store.NotificationChannel
	Target string `json:"target"`
}

func viewChannel(c store.NotificationChannel) channelView {
	return channelView{NotificationChannel: c, Target: notify.Target(c.Kind, c.Config, c.Secrets())}
}

// channelBody is a create, edit or test. On an edit an absent field keeps its
// value; in secrets, an absent name keeps the stored secret and "" clears it.
type channelBody struct {
	ID          *int64            `json:"id"`
	Kind        string            `json:"kind"`
	Name        *string           `json:"name"`
	Enabled     *bool             `json:"enabled"`
	MinSeverity *string           `json:"min_severity"`
	Config      map[string]string `json:"config"`
	Secrets     map[string]string `json:"secrets"`
}

// buildChannel applies body to base (the stored channel, or a new one) and
// validates the result for its kind. The error is for the operator: it names
// a field, never a value.
func buildChannel(base store.NotificationChannel, body channelBody) (store.NotificationChannel, map[string]string, string) {
	c := base
	if base.ID == 0 {
		c.Kind, c.Enabled, c.MinSeverity = strings.TrimSpace(body.Kind), true, "warn"
	} else if body.Kind != "" && body.Kind != base.Kind {
		return c, nil, "kind cannot change; delete the channel and add a new one"
	}
	if body.Name != nil {
		c.Name = strings.TrimSpace(*body.Name)
	}
	if n := len([]rune(c.Name)); n == 0 || n > 60 {
		return c, nil, "name must be 1 to 60 characters"
	}
	if body.Enabled != nil {
		c.Enabled = *body.Enabled
	}
	if body.MinSeverity != nil {
		c.MinSeverity = *body.MinSeverity
	}
	if !notify.ValidSeverity(c.MinSeverity) {
		return c, nil, "min_severity must be info, warn or error"
	}
	if body.Config != nil || base.ID == 0 {
		c.Config = map[string]string{}
		for k, v := range body.Config {
			if v = strings.TrimSpace(v); v != "" {
				c.Config[k] = v
			}
		}
	}
	secrets := base.Secrets()
	for k, v := range body.Secrets {
		if v = strings.TrimSpace(v); v == "" {
			delete(secrets, k)
		} else {
			secrets[k] = v
		}
	}
	if err := notify.Validate(c.Kind, c.Config, secrets); err != nil {
		return c, nil, err.Error()
	}
	return c, secrets, ""
}

// handleNotificationChannels: GET lists the channels with the kinds and
// their fields; POST adds one.
func (s *Server) handleNotificationChannels(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		list, err := s.db.ListNotificationChannels(r.Context())
		if err != nil {
			s.writeStoreErr(w, err)
			return
		}
		views := make([]channelView, 0, len(list))
		for _, c := range list {
			views = append(views, viewChannel(c))
		}
		writeJSON(w, http.StatusOK, map[string]any{"channels": views, "kinds": notify.Kinds, "max": store.MaxNotificationChannels})
		return
	}
	var body channelBody
	if !decodeBody(w, r, &body) {
		return
	}
	c, secrets, msg := buildChannel(store.NotificationChannel{}, body)
	if msg != "" {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", msg)
		return
	}
	id, err := s.db.CreateNotificationChannel(r.Context(), c, secrets)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	s.writeChannel(w, r, id, http.StatusCreated)
}

// handleNotificationChannel: PATCH edits a channel, DELETE removes it.
func (s *Server) handleNotificationChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.db.DeleteNotificationChannel(r.Context(), id); err != nil {
			s.writeStoreErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	base, err := s.db.NotificationChannel(r.Context(), id)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	var body channelBody
	if !decodeBody(w, r, &body) {
		return
	}
	c, secrets, msg := buildChannel(base, body)
	if msg != "" {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", msg)
		return
	}
	if err := s.db.UpdateNotificationChannel(r.Context(), c, secrets); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	s.writeChannel(w, r, id, http.StatusOK)
}

func (s *Server) writeChannel(w http.ResponseWriter, r *http.Request, id int64, status int) {
	c, err := s.db.NotificationChannel(r.Context(), id)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	writeJSON(w, status, viewChannel(c))
}

// handleNotificationChannelTest sends one test message, once, to a saved
// channel (`id`, optionally with unsaved edits) or to a draft that is not
// saved yet. Only a saved channel tested as stored records the outcome as its
// status: with edits, the outcome describes a destination that is not saved.
func (s *Server) handleNotificationChannelTest(w http.ResponseWriter, r *http.Request) {
	var body channelBody
	if !decodeBody(w, r, &body) {
		return
	}
	base := store.NotificationChannel{}
	if body.ID != nil {
		var err error
		if base, err = s.db.NotificationChannel(r.Context(), *body.ID); err != nil {
			s.writeStoreErr(w, err)
			return
		}
	}
	c, secrets, msg := buildChannel(base, body)
	if msg != "" {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", msg)
		return
	}
	m := notify.Message{Kind: "test", Severity: "info", Title: "ezllm test notification",
		Body: "This channel reaches you. Sent from Settings → Notifications.", CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	err := notify.Send(r.Context(), s.push.client, c.Kind, c.Config, secrets, m, 1)
	asStored := body.Name == nil && body.Enabled == nil && body.MinSeverity == nil && body.Config == nil && len(body.Secrets) == 0
	if body.ID != nil && asStored {
		s.recordDelivery(c.WithSecrets(secrets), err)
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
