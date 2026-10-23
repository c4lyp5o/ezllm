package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestMasterKeyExistingAndEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(" original-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key, created, err := loadOrCreateMasterKey(path)
	if err != nil || created || key != "original-secret" {
		t.Fatalf("existing: created=%t err=%v", created, err)
	}
	if err = os.WriteFile(path, []byte(" \n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = loadOrCreateMasterKey(path); err == nil {
		t.Fatal("empty key accepted")
	}
	b, _ := os.ReadFile(path)
	if string(b) != " \n" {
		t.Fatal("empty file overwritten")
	}
}

func TestMasterKeyReadFailurePreservesFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires normal permission checks")
	}
	path := filepath.Join(t.TempDir(), "key")
	original := "must-survive"
	if err := os.WriteFile(path, []byte(original), 0200); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadOrCreateMasterKey(path); err == nil {
		t.Fatal("read error ignored")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != original {
		t.Fatalf("key changed: %v", err)
	}
}

func TestMasterKeyConcurrentFirstBoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "key")
	const n = 12
	var wg sync.WaitGroup
	keys := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) { defer wg.Done(); keys[i], _, errs[i] = loadOrCreateMasterKey(path) }(i)
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil || keys[i] == "" || keys[i] != keys[0] {
			t.Fatalf("first boot mismatch at %d: %v", i, errs[i])
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("permissions %v %v", info, err)
	}
}
