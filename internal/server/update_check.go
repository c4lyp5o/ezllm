package server

// The release check: once a day, ask GitHub which ezllm release is the latest and
// tell the operator when it is newer than this build. It only tells: nothing is
// downloaded or installed. The request carries no instance data (no version, no
// identifier), and update_check (EZLLM_UPDATE_CHECK) turns it off.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/c4lyp5o/ezllm/internal/store"
)

const (
	latestReleaseURL  = "https://api.github.com/repos/c4lyp5o/ezllm/releases/latest"
	releasePageURL    = "https://github.com/c4lyp5o/ezllm/releases/tag/v"
	updateCheckEvery  = 24 * time.Hour
	updateCheckBoot   = time.Minute
	updateCheckBudget = 15 * time.Second
)

// releaseCheck is the last answer. It is memory only: a restart asks again.
type releaseCheck struct {
	mu      sync.Mutex
	url     string // tests point it at a fake; "" is latestReleaseURL
	latest  string // X.Y.Z
	checked time.Time
}

var releaseTag = regexp.MustCompile(`^v?(\d{1,6})\.(\d{1,6})\.(\d{1,6})$`)

// parseRelease reads vX.Y.Z. Prerelease suffixes are not releases here.
func parseRelease(tag string) (parts [3]int, ok bool) {
	m := releaseTag.FindStringSubmatch(tag)
	if m == nil {
		return parts, false
	}
	for i := range parts {
		parts[i], _ = strconv.Atoi(m[i+1])
	}
	return parts, true
}

// newerRelease reports whether candidate is a later release than current.
func newerRelease(candidate, current string) bool {
	a, ok := parseRelease(candidate)
	b, ok2 := parseRelease(current)
	if !ok || !ok2 {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func fetchLatestRelease(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, updateCheckBudget)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ezllm") // GitHub requires one
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("latest release: HTTP %d", resp.StatusCode)
	}
	var rel struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", fmt.Errorf("latest release: %w", err)
	}
	v, ok := parseRelease(rel.Tag)
	if !ok || rel.Draft || rel.Prerelease {
		return "", fmt.Errorf("latest release: no usable release tag")
	}
	// Rebuilt from numbers: nothing the response says reaches the dashboard as is.
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]), nil
}

// checkForUpdate runs one check. A failure keeps the previous answer.
func (s *Server) checkForUpdate(ctx context.Context) bool {
	url := s.release.url
	if url == "" {
		url = latestReleaseURL
	}
	latest, err := fetchLatestRelease(ctx, url)
	if err != nil {
		return false
	}
	s.release.mu.Lock()
	s.release.latest, s.release.checked = latest, time.Now().UTC()
	s.release.mu.Unlock()
	if newerRelease(latest, Version) && s.updateCheckOn() { // switched off meanwhile: stay quiet
		s.notify(store.NotificationInput{Kind: store.NoteUpdateAvailable,
			Title:    "ezllm v" + latest + " is available",
			Body:     "This instance runs v" + Version + ". Back up before upgrading; the release notes say what changed.",
			Data:     map[string]string{"current": Version, "latest": latest, "url": releasePageURL + latest},
			DedupKey: "update_available:" + latest, Once: true})
	}
	return true
}

func (s *Server) updateCheckOn() bool {
	return s.settings != nil && s.settings.Current().UpdateCheck
}

// handleUpdate: GET /admin/update. The last check's answer, never a new one.
func (s *Server) handleUpdate(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{"enabled": s.updateCheckOn(), "current": Version, "latest": "", "available": false, "url": "", "checked_at": ""}
	if s.updateCheckOn() {
		s.release.mu.Lock()
		latest, checked := s.release.latest, s.release.checked
		s.release.mu.Unlock()
		if latest != "" {
			out["latest"], out["checked_at"] = latest, checked.Format(time.RFC3339)
			out["url"], out["available"] = releasePageURL+latest, newerRelease(latest, Version)
		}
	}
	writeJSON(w, http.StatusOK, out)
}
