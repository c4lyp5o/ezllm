package backup

import (
	"os"
	"syscall"
	"testing"
)

// A filesystem that cannot flush a directory (EINVAL on NFS, FUSE, VirtioFS)
// does not fail a backup whose file is already synced and renamed.
func TestSyncDirToleratesUnsupported(t *testing.T) {
	old := fsyncDir
	t.Cleanup(func() { fsyncDir = old })
	fsyncDir = func(*os.File) error { return syscall.EINVAL }
	if err := syncDir(t.TempDir()); err != nil {
		t.Fatalf("syncDir = %v, want EINVAL tolerated", err)
	}
	fsyncDir = func(*os.File) error { return syscall.EIO }
	if err := syncDir(t.TempDir()); err == nil {
		t.Fatal("a real I/O error must still fail")
	}
}
