package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Publish a fully written key without replacing any existing path. Linking a
// synced temporary file avoids both first-boot overwrite races and readers
// observing the empty file left briefly by OpenFile(O_EXCL) before Write.
func loadOrCreateMasterKey(path string) (string, bool, error) {
	read := func() (string, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		secret := strings.TrimSpace(string(b))
		if secret == "" {
			return "", fmt.Errorf("master key file is empty")
		}
		return secret, nil
	}
	secret, err := read()
	if err == nil {
		return secret, false, nil
	}
	if !os.IsNotExist(err) {
		return "", false, fmt.Errorf("read master key: %w", err)
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", false, err
	}
	file, err := os.CreateTemp(dir, ".ezllm-key-*")
	if err != nil {
		return "", false, err
	}
	defer os.Remove(file.Name())
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err == nil {
		secret = base64.StdEncoding.EncodeToString(buf)
		_, err = file.WriteString(secret + "\n")
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", false, err
	}
	if err = os.Link(file.Name(), path); err != nil {
		if os.IsExist(err) {
			secret, err = read()
			return secret, false, err
		}
		return "", false, fmt.Errorf("publish master key: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return "", false, err
	}
	return secret, true, nil
}

// defaultMasterKeyPath is <data_dir>/ezllm-master.key. Before 2.0.0 the default
// was ~/.hermes/secrets/ezllm-master.key (<data_dir>/.hermes/... without a usable
// home). A key left at a legacy path refuses the boot instead of being replaced:
// a freshly generated key would make every stored provider key unreadable.
func defaultMasterKeyPath(dataDir, home string) (string, error) {
	path := filepath.Join(dataDir, "ezllm-master.key")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	legacy := []string{filepath.Join(dataDir, ".hermes", "secrets", "ezllm-master.key")}
	if home != "" && home != "/" {
		legacy = append([]string{filepath.Join(home, ".hermes", "secrets", "ezllm-master.key")}, legacy...)
	}
	for _, old := range legacy {
		if _, err := os.Stat(old); err == nil {
			return "", fmt.Errorf("found the master key at its pre-2.0.0 default %s; move it to %s (or set EZLLM_MASTER_KEY_FILE=%s)", old, path, old)
		}
	}
	return path, nil
}
