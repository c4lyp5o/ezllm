package bridgeutil

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
)

// TailBuffer keeps the last lines of a CLI child's stderr for error reports:
// diagnosis, not noise. A failure whose output went to /dev/null cannot be
// diagnosed later. The zero value keeps 40 lines and is safe for concurrent use.
type TailBuffer struct {
	mu    sync.Mutex
	lines []string
}

const tailLines = 40

// Add appends a line, dropping the oldest past the limit.
func (t *TailBuffer) Add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > tailLines {
		t.lines = t.lines[len(t.lines)-tailLines:]
	}
}

// Tail joins the kept lines with " | " ("" when nothing was written).
func (t *TailBuffer) Tail() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, " | ")
}

// WriteJSONBytes answers with an already-encoded JSON body.
func WriteJSONBytes(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// RandomHex returns n random bytes as hex, for response and turn ids.
// crypto/rand.Read does not fail on supported platforms.
func RandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
