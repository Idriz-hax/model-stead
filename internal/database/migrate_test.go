package database

import "testing"

func TestOpenRunsMigrations(t *testing.T) {
	db, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM schema_migrations_placeholder").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("unexpected row count %d", count)
	}
}

func TestAuthMigrationAndForeignKeys(t *testing.T) {
	db, err := Open(t.TempDir() + "/auth.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var enabled int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 {
		t.Fatalf("foreign keys disabled: %d", enabled)
	}
	if _, err := db.Exec(`INSERT INTO api_keys (id,user_id,name,key_prefix,key_hash,status) VALUES ('k','missing','x','x','x','active')`); err == nil {
		t.Fatal("expected foreign-key constraint")
	}
}
