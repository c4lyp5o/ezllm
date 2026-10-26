package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/c4lyp5o/ezllm/internal/backup"
)

func runBackupCommand(command string, args []string) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	archive := flags.String("archive", "", "encrypted backup archive")
	secretFile := flags.String("secret-file", os.Getenv("EZLLM_BACKUP_SECRET_FILE"), "backup encryption secret file")
	output := flags.String("output", "", "new destination directory (restore only)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *archive == "" || *secretFile == "" {
		return fmt.Errorf("--archive and --secret-file are required")
	}
	raw, err := os.ReadFile(*secretFile)
	if err != nil {
		return err
	}
	secret := strings.TrimSpace(string(raw))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if command == "backup-verify" {
		m, err := backup.Verify(ctx, *archive, secret)
		if err != nil {
			return err
		}
		fmt.Printf("backup verified: schema %d, created %s\n", m.Schema, m.CreatedAt)
		return nil
	}
	if *output == "" {
		return fmt.Errorf("--output is required for restore")
	}
	if err := backup.Restore(ctx, *archive, secret, *output); err != nil {
		return err
	}
	fmt.Println("backup restored; configure the restored master-key file before starting ezllm")
	return nil
}
