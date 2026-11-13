package backup

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Idriz-hax/model-stead/internal/database"
	_ "modernc.org/sqlite"
)

func TestApplyRetention(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	for _, day := range []int{1, 2, 3, 4} {
		name := "router-" + now.AddDate(0, 0, -day).Format("20060102T150405.000000000Z") + ".db"
		writeValidBackup(t, filepath.Join(dir, name))
	}
	// A newer filename is not enough: invalid backups must not displace the newest valid one.
	invalid := filepath.Join(dir, "router-"+now.Format("20060102T150405.000000000Z")+".db")
	if err := os.WriteFile(invalid, []byte("not sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"unknown.db", "router-20260101T000000Z.db", "router-20260101T000000.000000000Z.tmp"} {
		os.WriteFile(filepath.Join(dir, name), nil, 0600)
	}
	if err := os.Mkdir(filepath.Join(dir, "router-dir"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := ApplyRetention(dir, 48*time.Hour, 3, now); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if m := managedBackupName.FindStringSubmatch(e.Name()); m != nil {
			d, _ := time.Parse("20060102T150405.000000000Z", m[1])
			if now.Sub(d) > 48*time.Hour {
				t.Errorf("old backup retained: %s", e.Name())
			}
		}
	}
	newest := filepath.Join(dir, "router-"+now.AddDate(0, 0, -1).Format("20060102T150405.000000000Z")+".db")
	if _, err := os.Stat(newest); err != nil {
		t.Fatal("newest was not retained", err)
	}
	for _, name := range []string{"unknown.db", "router-20260101T000000Z.db", "router-20260101T000000.000000000Z.tmp"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("unmanaged file removed: %s", name)
		}
	}
}

func TestApplyRetentionCount(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	for i := 0; i < 4; i++ {
		n := "router-" + now.Add(-time.Duration(i)*time.Minute).Format("20060102T150405.000000000Z") + ".db"
		writeValidBackup(t, filepath.Join(dir, n))
	}
	if err := ApplyRetention(dir, 0, 2, now); err != nil {
		t.Fatal(err)
	}
	e, _ := os.ReadDir(dir)
	if len(e) != 2 {
		t.Fatalf("retained %d backups", len(e))
	}
}

func writeValidBackup(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE goose_db_version (id INTEGER PRIMARY KEY, version_id INTEGER, is_applied BOOLEAN)`)
	if err == nil {
		_, err = db.Exec(`INSERT INTO goose_db_version(version_id,is_applied) VALUES(?,1)`, database.CurrentSchemaVersion)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareDestinationChecksWritableDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "backups")
	if err := PrepareDestination(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("probe was not removed: %v", entries)
	}
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDestination(file); err == nil {
		t.Fatal("expected destination preparation failure")
	}
}
