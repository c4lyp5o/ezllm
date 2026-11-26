package codexbridge

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A template that never reached the server keeps no credit occupied, even when
// its local folder cannot be removed.
func TestDeleteLocalTemplateReturnsItsCredit(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can remove a read-only folder")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "tpl")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "sub"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "sub"), 0o700) })
	r := &Runner{}
	r.cache.count = 1
	r.deleteTemplate(&templateLease{r: r, root: root})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.cache.mu.Lock()
		count, failures := r.cache.count, r.cache.cleanupFailures
		r.cache.mu.Unlock()
		if count == 0 {
			return
		}
		if failures > 0 {
			t.Fatalf("credit withheld: count=%d failures=%d", count, failures)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("deleteTemplate never returned the credit")
}
