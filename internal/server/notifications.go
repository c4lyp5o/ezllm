package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/c4lyp5o/ezllm/internal/provider"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// notify records a bell notification. Best-effort: a failed write is logged
// and never fails the operation that raised it.
func (s *Server) notify(in store.NotificationInput) {
	if s.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.db.Notify(ctx, in); err != nil {
		s.log.Warn("notification write failed", "kind", in.Kind, "err", err)
	}
}

// handleNotifications: GET /admin/notifications?limit=N → newest first plus
// the unread count (the bell's badge).
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, unread, err := s.db.ListNotifications(r.Context(), limit)
	if err != nil {
		s.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "unread": unread})
}

// handleNotificationsRead: POST /admin/notifications/read {"ids":[…]} or
// {"all":true}.
func (s *Server) handleNotificationsRead(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []int64 `json:"ids"`
		All bool    `json:"all"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if !in.All && len(in.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "send ids or all:true")
		return
	}
	if err := s.db.MarkNotificationsRead(r.Context(), in.IDs, in.All); err != nil {
		s.writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// catalogChange is what one catalog save changed.
type catalogChange struct {
	added, removed         []string
	freeAdded, freeRemoved []string
	first                  bool // the account had no catalog before
}

func diffIDs(before, after []string) (added, removed []string) {
	b := make(map[string]bool, len(before))
	for _, id := range before {
		b[id] = true
	}
	a := make(map[string]bool, len(after))
	for _, id := range after {
		a[id] = true
		if !b[id] {
			added = append(added, id)
		}
	}
	for _, id := range before {
		if !a[id] {
			removed = append(removed, id)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

// saveCatalog is the one way a listed catalog is stored: it replaces the
// account's catalog (which also rebuilds the auto-free combos), logs exactly
// which models came and went, and raises bell notifications for free models
// that joined or left. origin names the trigger ("catalog refresh",
// "key added", "manual sync").
func (s *Server) saveCatalog(ctx context.Context, acctID int64, namespace string, models []provider.ModelInfo, origin string) error {
	var before []string
	if rows, err := s.db.ListModels(ctx, acctID); err == nil {
		for _, m := range rows {
			before = append(before, m.ID)
		}
	}
	freeBefore, _ := s.db.FreeModelIDs(ctx, acctID)
	if err := s.db.UpsertModels(ctx, acctID, models); err != nil {
		return err
	}
	after := make([]string, len(models))
	for i, m := range models {
		after[i] = m.ID
	}
	freeAfter, _ := s.db.FreeModelIDs(ctx, acctID)
	var ch catalogChange
	ch.first = len(before) == 0
	ch.added, ch.removed = diffIDs(before, after)
	ch.freeAdded, ch.freeRemoved = diffIDs(freeBefore, freeAfter)
	if len(ch.added) > 0 || len(ch.removed) > 0 {
		s.log.Info("catalog changed", "account", namespace, "origin", origin,
			"models", len(after), "added", len(ch.added), "removed", len(ch.removed),
			"added_ids", logList(ch.added), "removed_ids", logList(ch.removed))
	}
	s.noteCatalogChange(acctID, namespace, ch)
	if len(ch.added) > 0 {
		s.pricesWanted.Store(true)
	}
	return nil
}

// logList bounds a model-id list for one log line.
func logList(ids []string) string { return boundedList(ids, ",") }

// noteList is the same bounded list for text a person reads.
func noteList(ids []string) string { return boundedList(ids, ", ") }

func boundedList(ids []string, sep string) string {
	const max = 20
	if len(ids) <= max {
		return strings.Join(ids, sep)
	}
	return strings.Join(ids[:max], sep) + fmt.Sprintf("%s… and %d more", sep, len(ids)-max)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func (s *Server) noteCatalogChange(acctID int64, namespace string, ch catalogChange) {
	if n := len(ch.freeAdded); n > 0 {
		title := fmt.Sprintf("%d new free %s on %s", n, plural(n, "model", "models"), namespace)
		if ch.first {
			title = fmt.Sprintf("%d free %s available on %s", n, plural(n, "model", "models"), namespace)
		}
		s.notify(store.NotificationInput{
			Kind: store.NoteFreeModelsAdded, Title: title,
			Body: "Joined the auto-free combos; they are probed before they take traffic: " + noteList(ch.freeAdded),
			Data: map[string]any{"account_id": acctID, "account": namespace, "models": ch.freeAdded},
		})
	}
	if n := len(ch.freeRemoved); n > 0 && !ch.first {
		s.notify(store.NotificationInput{
			Kind:  store.NoteFreeModelsRemoved,
			Title: fmt.Sprintf("%d free %s gone from %s", n, plural(n, "model", "models"), namespace),
			Body:  "No longer in the provider's catalog, so out of the auto-free combos: " + noteList(ch.freeRemoved),
			Data:  map[string]any{"account_id": acctID, "account": namespace, "models": ch.freeRemoved},
		})
	}
}

// budgetNoted remembers which client+bucket already raised its "budget
// reached" notification in this process, so a key hammering past its cap
// costs one write, not one per refused request.
var budgetNoted sync.Map

func (s *Server) noteBudgetReached(client string, b store.ClientBudget) {
	key := client + "\x00" + b.Bucket
	if _, seen := budgetNoted.LoadOrStore(key, true); seen {
		return
	}
	s.notify(store.NotificationInput{
		Kind: store.NoteClientBudgetReached, Severity: "warn",
		Title:    fmt.Sprintf("Token %q reached its %s budget", client, b.Window),
		Body:     fmt.Sprintf("%d of %d tokens used; its requests are refused with 429 until the window resets.", b.Used, b.Limit),
		Data:     map[string]any{"client": client, "used": b.Used, "limit": b.Limit, "window": b.Window, "bucket": b.Bucket},
		DedupKey: "client_budget:" + key,
	})
}
