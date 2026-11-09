package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Idriz-hax/model-stead/internal/database"
	sqlite "modernc.org/sqlite"
)

type backupDriver interface {
	NewBackup(string) (*sqlite.Backup, error)
}
type restoreDriver interface {
	NewRestore(string) (*sqlite.Backup, error)
}

var renameFile = os.Rename

// OnlineBackup writes a consistent SQLite snapshot into dir and returns its path.
func OnlineBackup(db *sql.DB, dir string) (string, error) {
	return OnlineBackupContext(context.Background(), db, dir)
}

// OnlineBackupContext removes its temporary file if cancellation interrupts copying.
func OnlineBackupContext(ctx context.Context, db *sql.DB, dir string) (string, error) {
	tmp, err := os.CreateTemp(dir, ".router-backup-*.tmp")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	defer os.Remove(tmpPath)
	if err := copyDatabase(ctx, db, tmpPath); err != nil {
		return "", err
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		return "", err
	}
	if err := validate(tmpPath); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, "router-"+time.Now().UTC().Format("20060102T150405.000000000Z")+".db")
	if err := renameFile(tmpPath, dest); err != nil {
		return "", err
	}
	return dest, nil
}

func copyDatabase(ctx context.Context, db *sql.DB, path string) error {
	c, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	err = c.Raw(func(raw any) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		d, ok := raw.(backupDriver)
		if !ok {
			return fmt.Errorf("sqlite driver does not support backup")
		}
		b, err := d.NewBackup(path)
		if err != nil {
			return err
		}
		var stepErr error
		for {
			if err := ctx.Err(); err != nil {
				stepErr = err
				break
			}
			more, err := b.Step(128)
			if err != nil {
				stepErr = err
				break
			}
			if !more {
				break
			}
		}
		finishErr := b.Finish()
		if stepErr != nil {
			return stepErr
		}
		return finishErr
	})
	return err
}

// Restore acquires the database process lock, then stages and validates a
// replacement before preserving the database and sidecars. CLI callers must not
// acquire this lock separately; every restore caller is serialized here.
func Restore(source, target string) error {
	lock, err := database.AcquireProcessLock(target)
	if err != nil {
		return err
	}
	defer lock.Close()
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".router-restore-*.tmp")
	if err != nil {
		return err
	}
	stage := tmp.Name()
	tmp.Close()
	defer os.Remove(stage)
	db, err := sql.Open("sqlite", stage)
	if err != nil {
		return err
	}
	err = copyDatabaseFrom(source, db)
	closeErr := db.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Chmod(stage, 0600); err != nil {
		return err
	}
	if err := validate(stage); err != nil {
		return err
	}
	if err := validateSchema(stage); err != nil {
		return fmt.Errorf("restore source schema is incompatible: %w", err)
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	var moved []struct{ from, to string }
	for _, suffix := range []string{"", "-wal", "-shm"} {
		from := target + suffix
		if _, err := os.Lstat(from); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		to := target + ".pre-restore-" + stamp + suffix
		if err := renameFile(from, to); err != nil {
			return errors.Join(err, rollback(moved))
		}
		moved = append(moved, struct{ from, to string }{from, to})
	}
	if err := renameFile(stage, target); err != nil {
		return errors.Join(err, rollback(moved))
	}
	return nil
}

func copyDatabaseFrom(source string, dst *sql.DB) error {
	// SQLite restore runs on the destination connection with the backup filename.
	c, err := dst.Conn(context.Background())
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Raw(func(raw any) error {
		d, ok := raw.(restoreDriver)
		if !ok {
			return fmt.Errorf("sqlite driver does not support restore")
		}
		b, err := d.NewRestore(source)
		if err != nil {
			return err
		}
		var stepErr error
		for {
			more, err := b.Step(-1)
			if err != nil {
				stepErr = err
				break
			}
			if !more {
				break
			}
		}
		finishErr := b.Finish()
		if stepErr != nil {
			return stepErr
		}
		return finishErr
	})
}

func validateSchema(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	var version int64
	if err := db.QueryRow(`SELECT version_id FROM goose_db_version WHERE is_applied = 1 ORDER BY id DESC LIMIT 1`).Scan(&version); err != nil {
		return err
	}
	if version < database.CurrentSchemaVersion {
		return fmt.Errorf("database migration version %d is older than required version %d", version, database.CurrentSchemaVersion)
	}
	return nil
}

func validate(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("sqlite integrity check failed")
	}
	return nil
}

func rollback(moved []struct{ from, to string }) error {
	var errs []error
	for i := len(moved) - 1; i >= 0; i-- {
		if err := renameFile(moved[i].to, moved[i].from); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", filepath.Base(moved[i].from), err))
		}
	}
	return errors.Join(errs...)
}
