package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	if runtime.GOOS != "windows" {
		d, err := os.Open(dir)
		if err != nil {
			return "", true, err
		}
		err = d.Sync()
		d.Close()
		if err != nil {
			return "", true, err
		}
	}
	return secret, true, nil
}
