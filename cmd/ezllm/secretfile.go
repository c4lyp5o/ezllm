package main

import (
	"errors"
	"os"
	"runtime"
	"syscall"
)

// fsyncDir is the directory fsync, replaceable in tests.
var fsyncDir = func(d *os.File) error { return d.Sync() }

// syncDir makes a new directory entry durable. Some file systems (FUSE, some
// network and VM shares) refuse fsync on a directory; by then the file itself
// is written and synced, so that refusal is not worth failing the start over.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = fsyncDir(d)
	d.Close()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, errors.ErrUnsupported) {
		return nil
	}
	return err
}
