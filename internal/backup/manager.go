package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

type Record struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	Bytes     int64  `json:"bytes"`
	Verified  bool   `json:"verified"`
}
type Status struct {
	Error        string   `json:"error,omitempty"`
	Ready        bool     `json:"ready"`
	Running      bool     `json:"running"`
	LastOutcome  string   `json:"last_outcome"`
	LastFinished string   `json:"last_finished,omitempty"`
	Backups      []Record `json:"backups"`
}
type Manager struct {
	mu                        sync.Mutex
	dir, secret, master       string
	db                        *store.DB
	running                   bool
	lastOutcome, lastFinished string
}

func New(db *store.DB, dir, secret, master string) *Manager {
	m := &Manager{db: db, dir: dir, secret: strings.TrimSpace(secret), master: master}
	if rows, err := db.JobHealth(context.Background()); err == nil {
		h := rows["backup"]
		success, _ := time.Parse(time.RFC3339Nano, h.LastSuccess)
		failure, _ := time.Parse(time.RFC3339Nano, h.LastFailure)
		if !success.IsZero() || !failure.IsZero() {
			m.lastOutcome = "success"
			m.lastFinished = h.LastSuccess
			if failure.After(success) {
				m.lastOutcome = "failed"
				m.lastFinished = h.LastFailure
			}
		}
	}
	return m
}
func (m *Manager) Check() error {
	if !filepath.IsAbs(m.dir) || len(m.secret) < 24 || strings.TrimSpace(m.master) == "" || m.secret == strings.TrimSpace(m.master) {
		return errors.New("configure an absolute backup directory and a separate backup secret of at least 24 characters")
	}
	if err := os.MkdirAll(m.dir, 0700); err != nil {
		return errors.New("backup destination unavailable")
	}
	f, err := os.CreateTemp(m.dir, ".ezllm-check-")
	if err != nil {
		return errors.New("backup destination is not writable")
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

var recordID = regexp.MustCompile(`^ezllm-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{16}$`)

func (m *Manager) Records() ([]Record, error) {
	out := []Record{}
	if m.dir == "" {
		return out, nil
	}
	entries, err := os.ReadDir(m.dir)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return out, errors.New("backup destination unavailable")
	}
	for _, e := range entries {
		id := strings.TrimSuffix(e.Name(), ".json")
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || !recordID.MatchString(id) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(m.dir, e.Name()))
		if err != nil {
			return out, errors.New("backup metadata unavailable")
		}
		var r Record
		if json.Unmarshal(raw, &r) != nil || r.ID != id || !r.Verified {
			continue
		}
		info, err := os.Lstat(filepath.Join(m.dir, id+".tar.gz.age"))
		if err != nil || !info.Mode().IsRegular() || info.Size() != r.Bytes {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows, err := m.Records()
	inventoryError := ""
	if err != nil {
		inventoryError = "backup inventory unavailable"
	}
	return Status{Error: inventoryError, Ready: filepath.IsAbs(m.dir) && len(m.secret) >= 24 && m.master != "" && m.secret != strings.TrimSpace(m.master) && err == nil, Running: m.running, LastOutcome: m.lastOutcome, LastFinished: m.lastFinished, Backups: rows}
}

// Reserve is synchronous so two simultaneous manual/scheduled starts cannot
// both be accepted. The server runs RunReserved in its tracked background group.
func (m *Manager) Reserve() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return ErrBusy
	}
	if err := m.Check(); err != nil {
		m.finished(err)
		return err
	}
	m.running = true
	return nil
}
func (m *Manager) RunReserved(ctx context.Context, keep int) (err error) {
	defer func() { m.mu.Lock(); defer m.mu.Unlock(); m.running = false; m.finished(err) }()

	if keep < 1 || keep > 365 {
		return errors.New("retention must be 1–365")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	stage, err := os.MkdirTemp(m.dir, ".ezllm-backup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	database := filepath.Join(stage, "ezllm.sqlite")
	if err = m.db.BackupTo(ctx, database); err != nil {
		return err
	}
	archive := filepath.Join(stage, "archive.age")
	if err = writeArchive(ctx, archive, database, m.master, m.secret, m.db.SchemaVersion()); err != nil {
		return err
	}
	// Verification stays on the mounted destination: temporary plaintext never
	// escapes to a different volume whose permissions the operator did not choose.
	verifyDir, err := os.MkdirTemp(stage, "verify-")
	if err != nil {
		return err
	}
	manifest, err := extract(ctx, archive, m.secret, verifyDir)
	if err != nil {
		return err
	}
	if err = verifyDatabase(ctx, verifyDir, manifest); err != nil {
		return err
	}
	var nonce [8]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	id := "ezllm-" + time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(nonce[:])
	info, err := os.Stat(archive)
	if err != nil {
		return err
	}
	record := Record{ID: id, CreatedAt: manifest.CreatedAt, Bytes: info.Size(), Verified: true}
	raw, _ := json.Marshal(record)
	meta := filepath.Join(stage, "record.json")
	if err = os.WriteFile(meta, raw, 0600); err != nil {
		return err
	}
	if err = syncFile(meta); err != nil {
		return err
	}
	if err = os.Rename(archive, filepath.Join(m.dir, id+".tar.gz.age")); err != nil {
		return err
	}
	if err = os.Rename(meta, filepath.Join(m.dir, id+".json")); err != nil {
		return err
	}
	if err = syncDir(m.dir); err != nil {
		return err
	}
	if keep < 1 {
		return errors.New("retention must be positive")
	}
	records, err := m.Records()
	if err != nil {
		return err
	}
	for _, old := range records {
		if old.ID == id {
			continue
		}
		if keep > 1 {
			keep--
			continue
		}
		if err = os.Remove(filepath.Join(m.dir, old.ID+".tar.gz.age")); err != nil {
			return err
		}
		if err = os.Remove(filepath.Join(m.dir, old.ID+".json")); err != nil {
			return err
		}
	}
	return syncDir(m.dir)
}
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (m *Manager) CancelReserved() { m.mu.Lock(); defer m.mu.Unlock(); m.running = false }

var ErrBusy = errors.New("a backup is already running")

// finished is called with m.mu held. Persist only safe timestamps/outcomes.
func (m *Manager) finished(err error) {
	m.lastFinished = time.Now().UTC().Format(time.RFC3339Nano)
	m.lastOutcome = "success"
	if err != nil {
		m.lastOutcome = "failed"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, readErr := m.db.JobHealth(ctx)
	if readErr != nil {
		return
	}
	h := rows["backup"]
	if err == nil {
		h.LastSuccess = m.lastFinished
	} else {
		h.LastFailure = m.lastFinished
	}
	_ = m.db.SetJobHealth(ctx, "backup", h)
}
