package main

import (
	"os"
	"path/filepath"
	"testing"

	"database/sql"

	"github.com/Idriz-hax/model-stead/internal/config"
	"github.com/Idriz-hax/model-stead/internal/database"
)

func TestOperatorBackupAndRestore(t *testing.T) {
	dir := t.TempDir()
	var cfg config.Config
	cfg.Database.Path = filepath.Join(dir, "router.db")
	cfg.Backup.Directory = filepath.Join(dir, "backups")
	cfg.Backup.MaxAge = "720h"
	cfg.Backup.MaxBackups = 14
	cfg.Backup.Enabled = false // Manual backup is available regardless of scheduled-backup configuration.
	db, err := database.Open(cfg.Database.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE cli_test(value TEXT); INSERT INTO cli_test VALUES('saved')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := runOperatorCommand([]string{"backup"}, cfg); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(cfg.Backup.Directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("backup files=%v err=%v", entries, err)
	}
	source := filepath.Join(cfg.Backup.Directory, entries[0].Name())
	backupDB, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	var integrity string
	if err := backupDB.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("backup integrity=%q err=%v", integrity, err)
	}
	var backupValue string
	if err := backupDB.QueryRow(`SELECT value FROM cli_test`).Scan(&backupValue); err != nil || backupValue != "saved" {
		t.Fatalf("backup value=%q err=%v", backupValue, err)
	}
	backupDB.Close()
	lock, err := database.AcquireProcessLock(cfg.Database.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := runOperatorCommand([]string{"restore", source}, cfg); err == nil {
		t.Fatal("restore succeeded while locked")
	}
	lock.Close()
	if err := runOperatorCommand([]string{"restore", source}, cfg); err != nil {
		t.Fatal(err)
	}
	db, err = database.OpenExisting(cfg.Database.Path)
	if err != nil {
		t.Fatal(err)
	}
	var value string
	if err := db.QueryRow(`SELECT value FROM cli_test`).Scan(&value); err != nil || value != "saved" {
		t.Fatalf("value=%q err=%v", value, err)
	}
	db.Close()

	before, err := os.ReadFile(cfg.Database.Path)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(dir, "corrupt.db")
	if err := os.WriteFile(corrupt, []byte("not sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runOperatorCommand([]string{"restore", corrupt}, cfg); err == nil {
		t.Fatal("corrupt restore succeeded")
	}
	after, err := os.ReadFile(cfg.Database.Path)
	if err != nil || string(before) != string(after) {
		t.Fatalf("target changed on failed restore: %v", err)
	}
}

func TestOperatorCommandRejectsMalformedArguments(t *testing.T) {
	var cfg config.Config
	for _, args := range [][]string{{"backup", "extra"}, {"restore"}, {"restore", "source", "extra"}} {
		t.Run(args[0]+"-argument-count", func(t *testing.T) {
			if err := runOperatorCommand(args, cfg); err == nil {
				t.Fatalf("runOperatorCommand(%q) succeeded", args)
			}
		})
	}
}

func TestSetupRequiresHTTPSOnlyWhenPending(t *testing.T) {
	for _, tc := range []struct {
		name    string
		url     string
		pending bool
		want    bool
	}{
		{"pending without url", "", true, false},
		{"pending http", "http://example.com", true, false},
		{"pending valid https", "https://example.com", true, true},
		{"pending empty https host", "https://", true, false},
		{"pending https path", "https://example.com/path", true, false},
		{"pending https query", "https://example.com?x=1", true, false},
		{"pending https userinfo", "https://user@example.com", true, false},
		{"not pending without url", "", false, true},
		{"not pending http", "http://example.com", false, true},
		{"not pending malformed", "https://", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := setupURLValid(tc.url, tc.pending); got != tc.want {
				t.Fatalf("setupURLValid(%q, %v) = %v, want %v", tc.url, tc.pending, got, tc.want)
			}
		})
	}
}
