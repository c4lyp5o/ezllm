package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The CLI's own answers: `auth status` prints JSON and exits 1 when logged out.
func TestClaudeLoginCheckPerSystem(t *testing.T) {
	t.Setenv("USER", "operator")
	t.Setenv("LOGNAME", "operator")
	cli := func(body string) string {
		bin := filepath.Join(t.TempDir(), "claude")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return bin
	}
	in := cli(`echo '{'; echo '  "loggedIn": true,'; echo '  "subscriptionType": "pro"'; echo '}'`)
	out := cli(`echo '{'; echo '  "loggedIn": false,'; echo '}'; exit 1`)
	broken := cli(`echo 'boom' >&2; exit 1`)
	withFile := t.TempDir()
	if err := os.WriteFile(filepath.Join(withFile, ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ name, goos, bin, dir, want string }{
		{"mac, login in the keychain", "darwin", in, t.TempDir(), "logged-in"},
		{"mac, no login", "darwin", out, t.TempDir(), "logged-out"},
		{"mac, the CLI fails", "darwin", broken, t.TempDir(), "unverified"},
		{"linux, no file", "linux", in, t.TempDir(), "logged-out"},
		{"linux, file", "linux", in, withFile, "logged-in"},
		{"linux, the CLI refuses", "linux", out, withFile, "unverified"},
	} {
		if got, _ := claudeLoginCheckOn(c.goos, c.bin, c.dir); got != c.want {
			t.Errorf("%s: state = %q, want %q", c.name, got, c.want)
		}
	}
}
