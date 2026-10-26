package store

import (
	"context"
	"os"
	"time"

	"modernc.org/sqlite"
)

// BackupTo copies a consistent WAL-inclusive snapshot using the driver's
// online backup API. The caller owns the private destination directory.
func (d *DB) BackupTo(ctx context.Context, path string) error {
	if err := d.Flush(); err != nil {
		return err
	}
	conn, err := d.r.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(raw any) error {
		b, err := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		}).NewBackup(path)
		if err != nil {
			return err
		}
		finished := false
		defer func() {
			if !finished {
				_ = b.Finish()
			}
		}()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := b.Step(256)
			if err != nil {
				return err
			}
			if !more {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Millisecond):
			}
		}
		err = b.Finish()
		finished = true
		if err != nil {
			return err
		}
		return os.Chmod(path, 0600)
	})
}

func CurrentSchemaVersion() int { return schemaVersion }
