package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Idriz-hax/model-stead/internal/database"
)

func TestOnlineBackup(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE sample (value TEXT); INSERT INTO sample VALUES ('saved')`); err != nil {
		t.Fatal(err)
	}
	path, err := OnlineBackup(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	copy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	var value string
	if err := copy.QueryRow(`SELECT value FROM sample`).Scan(&value); err != nil || value != "saved" {
		t.Fatalf("value=%q err=%v", value, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
}

func TestOnlineBackupContextCancellationCleansTemporaryFile(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := OnlineBackupContext(ctx, db, dir); err == nil {
		t.Fatal("expected cancellation")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("backup artifacts remain: %v", entries)
	}
}

func TestOnlineBackupContextCancelsBlockedConnectionAcquisition(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	held, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := OnlineBackupContext(ctx, db, dir); done <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("backup did not cancel while waiting for a connection")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("backup artifacts remain: %v", entries)
	}
}

func TestRestorePreservesTargetOnInvalidSource(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.db")
	if err := os.WriteFile(target, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.db")
	if err := os.WriteFile(bad, []byte("not sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Restore(bad, target); err == nil {
		t.Fatal("expected invalid source error")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "original" {
		t.Fatalf("target=%q err=%v", got, err)
	}
}

func TestRestoreRejectsTargetWithProcessLockHeld(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.db")
	original := []byte("original")
	if err := os.WriteFile(target, original, 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := database.AcquireProcessLock(target)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := Restore(filepath.Join(dir, "backup.db"), target); err == nil {
		t.Fatal("expected active-target lock rejection")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(original) {
		t.Fatalf("target=%q err=%v", got, err)
	}
}

func TestRestore(t *testing.T) {
	dir := t.TempDir()
	source, err := database.Open(filepath.Join(dir, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Exec(`CREATE TABLE restored (value TEXT); INSERT INTO restored VALUES ('yes')`); err != nil {
		t.Fatal(err)
	}
	backupPath, err := OnlineBackup(source, dir)
	if err != nil {
		t.Fatal(err)
	}
	source.Close()
	target := filepath.Join(dir, "target.db")
	// Restore invokes the driver's NewRestore against this staged target; the
	// row assertion below verifies the copied database content end to end.
	if err := Restore(backupPath, target); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", target)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got string
	if err := db.QueryRow(`SELECT value FROM restored`).Scan(&got); err != nil || got != "yes" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestRestoreRejectsOldSchemaWithoutChangingTarget(t *testing.T) {
	dir := t.TempDir()
	old, err := database.Open(filepath.Join(dir, "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`UPDATE goose_db_version SET version_id = ? WHERE is_applied = 1 AND version_id = ?`, database.CurrentSchemaVersion-1, database.CurrentSchemaVersion); err != nil {
		t.Fatal(err)
	}
	backupPath, err := OnlineBackup(old, dir)
	if err != nil {
		t.Fatal(err)
	}
	old.Close()
	target := filepath.Join(dir, "target.db")
	if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Restore(backupPath, target); err == nil {
		t.Fatal("expected incompatible schema error")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "preserve" {
		t.Fatalf("target=%q err=%v", got, err)
	}
}

func TestRestorePublishRenameFailurePreservesTarget(t *testing.T) {
	dir := t.TempDir()
	src, err := database.Open(filepath.Join(dir, "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	backupPath, err := OnlineBackup(src, dir)
	if err != nil {
		t.Fatal(err)
	}
	src.Close()
	target := filepath.Join(dir, "target.db")
	original := []byte("original target bytes")
	if err := os.WriteFile(target, original, 0600); err != nil {
		t.Fatal(err)
	}
	oldRename := renameFile
	renameFile = func(from, to string) error {
		if strings.HasPrefix(filepath.Base(from), ".router-restore-") && to == target {
			return os.ErrPermission
		}
		return oldRename(from, to)
	}
	defer func() { renameFile = oldRename }()
	if err := Restore(backupPath, target); err == nil {
		t.Fatal("expected replacement failure")
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(original) {
		t.Fatalf("target=%q err=%v", got, err)
	}
}

func TestRestoreReportsRollbackRenameFailure(t *testing.T) {
	dir := t.TempDir()
	src, err := database.Open(filepath.Join(dir, "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	backupPath, err := OnlineBackup(src, dir)
	if err != nil {
		t.Fatal(err)
	}
	src.Close()
	target := filepath.Join(dir, "target.db")
	if err := os.WriteFile(target, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	oldRename := renameFile
	renameFile = func(from, to string) error {
		if strings.HasPrefix(filepath.Base(from), ".router-restore-") && to == target {
			return os.ErrPermission
		}
		if strings.Contains(from, ".pre-restore-") && to == target {
			return errors.New("injected rollback failure")
		}
		return oldRename(from, to)
	}
	defer func() { renameFile = oldRename }()
	err = Restore(backupPath, target)
	if err == nil || !strings.Contains(err.Error(), "injected rollback failure") || !strings.Contains(err.Error(), "restore target.db") {
		t.Fatalf("expected reported rollback failure, got %v", err)
	}
}
