//go:build !linux

package codexbridge

import (
	"errors"
	"fmt"
	"os"
)

// Landlock is a Linux kernel feature; other builds report it unavailable, so
// the bridge runs with client tools off (it refuses to start off Linux anyway).

func LandlockABI() (int, error) { return 0, errors.New("Landlock is Linux-only") }

func LandlockMain([]string) {
	fmt.Fprintln(os.Stderr, "codex-bridge landlock: Linux-only")
	os.Exit(126)
}

const landlockSupported = false
