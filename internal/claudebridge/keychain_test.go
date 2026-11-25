package claudebridge

import (
	"os"
	"path/filepath"
	"testing"
)

// On a Mac the CLI's login is in the keychain, which macOS finds through HOME.
// The per-request HOME shows that one folder and nothing else of the home.
func TestLinkKeychainsShowsOnlyTheKeychainFolderOnMac(t *testing.T) {
	home, tmp := t.TempDir(), t.TempDir()
	keychains := filepath.Join(home, "Library", "Keychains")
	if err := os.MkdirAll(keychains, 0o700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(keychains, "login.keychain-db")
	if err := os.WriteFile(db, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "Library", "Preferences"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := linkKeychains("darwin", home, tmp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "Library", "Keychains", "login.keychain-db")); err != nil {
		t.Fatalf("the keychain is not visible in the run's HOME: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(tmp, "Library"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("Library holds %v (err %v), want only Keychains", entries, err)
	}

	// Run's cleanup removes the link, never what it points at.
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("cleanup reached the real keychain: %v", err)
	}
}

func TestLinkKeychainsDoesNothingElsewhere(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "Library", "Keychains"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, goos, home string }{
		{"linux", "linux", home},
		{"no HOME", "darwin", ""},
		{"no keychain folder", "darwin", t.TempDir()},
	} {
		tmp := t.TempDir()
		if err := linkKeychains(c.goos, c.home, tmp); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
			t.Errorf("%s: left %v in the run's HOME", c.name, entries)
		}
	}
}
