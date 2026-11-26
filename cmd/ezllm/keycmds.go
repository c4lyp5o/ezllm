package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/config"
	"github.com/c4lyp5o/ezllm/internal/store"
)

// dataDirDefault is where the commands look for the database: the same
// precedence the server uses (EZLLM_DATA_DIR, else the repo-relative default).
func dataDirDefault() string {
	if v := strings.TrimSpace(os.Getenv("EZLLM_DATA_DIR")); v != "" {
		return v
	}
	return config.DefaultDataDir
}

// runRotateMasterKey re-encrypts every stored secret under a fresh master key
// and puts the new key in the key file. ezllm must be stopped first.
//
// The new key is written next to the old one BEFORE the database commits, so
// a crash after the commit never leaves the database without a key that opens
// it. The old key is kept as <key-file>.old-<time> until the operator has
// started ezllm with the new one.
func runRotateMasterKey(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("rotate-master-key", flag.ContinueOnError)
	dataDir := flags.String("data-dir", dataDirDefault(), "folder holding ezllm.sqlite")
	keyFile := flags.String("key-file", strings.TrimSpace(os.Getenv("EZLLM_MASTER_KEY_FILE")), "current master key file (default <data-dir>/ezllm-master.key)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("rotate-master-key: unexpected argument %q (flags only)", flags.Arg(0))
	}
	if strings.TrimSpace(os.Getenv("EZLLM_MASTER_KEY")) != "" {
		return errors.New("rotate-master-key: the key comes from EZLLM_MASTER_KEY; put it in a file, point --key-file at it, and unset the variable")
	}
	if *keyFile == "" {
		*keyFile = filepath.Join(*dataDir, "ezllm-master.key")
	}
	raw, err := os.ReadFile(*keyFile)
	if err != nil {
		return fmt.Errorf("rotate-master-key: read the current key: %w", err)
	}
	oldSecret := strings.TrimSpace(string(raw))
	if oldSecret == "" {
		return fmt.Errorf("rotate-master-key: %s is empty", *keyFile)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	newSecret := base64.StdEncoding.EncodeToString(buf)

	dir := filepath.Dir(*keyFile)
	tmp, err := os.CreateTemp(dir, ".ezllm-key-new-*")
	if err != nil {
		return fmt.Errorf("rotate-master-key: write the new key: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.WriteString(newSecret + "\n")
		if err == nil {
			err = tmp.Sync()
		}
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("rotate-master-key: write the new key: %w", err)
		}
	} else {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := store.RotateMasterKey(ctx, filepath.Join(*dataDir, "ezllm.sqlite"), oldSecret, newSecret)
	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rotate-master-key: nothing changed: %w", err)
	}
	// The database now needs the new key. Keep the old one beside it, then
	// move the new one into place.
	oldCopy := fmt.Sprintf("%s.old-%s", *keyFile, time.Now().UTC().Format("20060102T150405Z"))
	if err := os.Rename(*keyFile, oldCopy); err != nil {
		return fmt.Errorf("rotate-master-key: the database uses the new key, which is in %s; move it to %s by hand: %w", tmpPath, *keyFile, err)
	}
	if err := os.Rename(tmpPath, *keyFile); err != nil {
		return fmt.Errorf("rotate-master-key: the database uses the new key, which is in %s; move it to %s by hand: %w", tmpPath, *keyFile, err)
	}
	_ = syncDir(dir)
	fmt.Fprintf(out, "master key rotated: %d provider keys, %d management keys, %d channel secrets re-encrypted\n",
		res.ProviderKeys, res.ManagementKeys, res.ChannelSecrets)
	fmt.Fprintf(out, "new key: %s\nold key kept as: %s\n", *keyFile, oldCopy)
	fmt.Fprintln(out, "Start ezllm and check your providers work, then delete the old key file.")
	return nil
}

// runTokenCreate mints a client key from the command line, for setups that
// provision ezllm without the dashboard. The key is printed once.
func runTokenCreate(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("usage: ezllm token create --name NAME [--roles infer,metrics,admin]")
	}
	flags := flag.NewFlagSet("token create", flag.ContinueOnError)
	dataDir := flags.String("data-dir", dataDirDefault(), "folder holding ezllm.sqlite")
	name := flags.String("name", "", "the key's name, shown in the ledger and the dashboard")
	roles := flags.String("roles", "infer", "comma-separated roles: infer, metrics, admin")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("token create: unexpected argument %q (flags only)", flags.Arg(0))
	}
	if strings.TrimSpace(*name) == "" {
		return errors.New("token create: --name is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(*dataDir, "ezllm.sqlite")})
	if err != nil {
		return err
	}
	defer db.Close()
	var list []string
	for _, r := range strings.Split(*roles, ",") {
		if r = strings.TrimSpace(r); r != "" {
			list = append(list, r)
		}
	}
	_, plaintext, err := db.UpsertToken(ctx, store.TokenInput{Name: strings.TrimSpace(*name), Roles: list})
	if err != nil {
		return fmt.Errorf("token create: %w", err)
	}
	fmt.Fprintln(out, plaintext)
	return nil
}
