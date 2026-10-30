package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/c4lyp5o/ezllm/internal/settings"
)

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		writeErr(w, 503, "server_error", "settings unavailable")
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, s.settings.View())
		return
	}
	var in struct {
		Revision *int64                     `json:"revision"`
		Values   map[string]json.RawMessage `json:"values"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || in.Revision == nil || len(in.Values) == 0 {
		writeErr(w, 400, "invalid_request_error", "send revision and a nonempty values object")
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeErr(w, 400, "invalid_request_error", "send a single JSON object")
		return
	}
	view, err := s.settings.Patch(r.Context(), *in.Revision, in.Values)
	if err != nil {
		if errors.Is(err, settings.ErrPersistence) {
			writeErr(w, 503, "server_error", "settings could not be persisted; previous values remain active")
			return
		}
		code := 400
		if errors.Is(err, settings.ErrConflict) {
			code = 409
		}
		writeErr(w, code, "invalid_request_error", err.Error())
		return
	}
	s.priceRefreshOn.Store(s.settings.Current().PriceRefresh)
	select {
	case s.settingsWake <- struct{}{}:
	default:
	}
	writeJSON(w, 200, view)
}
func (s *Server) automaticProbing() bool {
	if s.settings != nil {
		return s.settings.Current().AutoProbe
	}
	return s.comboAutoProbe
}
func (s *Server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	// Deliberately build an allowlist, never serialize process config or bridge Status.
	bridges := []map[string]any{}
	s.bridges.mu.RLock()
	for _, b := range s.bridges.list {
		row := map[string]any{"id": b.ID, "enabled": b.Enabled}
		if u := b.Update; u != nil {
			// The updater's verdict and versions only: its detail can carry
			// upstream error text, which stays on the admin bridge card.
			row["update"] = map[string]any{"result": u.Result, "checked_at": u.CheckedAt, "current": u.Current, "candidate": u.Candidate}
		}
		bridges = append(bridges, row)
	}
	s.bridges.mu.RUnlock()
	var size int64
	if s.db != nil {
		if info, err := os.Stat(s.db.Path()); err == nil {
			size = info.Size()
		}
	}
	out := map[string]any{"version": Version, "uptime_seconds": int64(time.Since(s.started).Seconds()), "database_bytes": size, "bridges": bridges, "jobs": s.jobStatus(), "deployment": s.deployment}
	if s.settings != nil {
		out["settings"] = s.settings.View()
	}
	if s.backups != nil {
		out["backups"] = s.backups.Status()
	}
	if r.URL.Query().Get("download") == "true" {
		w.Header().Set("Content-Disposition", `attachment; filename="ezllm-diagnostics.json"`)
	}
	writeJSON(w, 200, out)
}
