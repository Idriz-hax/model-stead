package database

import (
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// CurrentSchemaVersion is the latest Goose migration required by this build.
const CurrentSchemaVersion int64 = 5

func Open(path string) (*sql.DB, error) {
	db, err := OpenExisting(path)
	if err != nil {
		return nil, err
	}
	goose.SetBaseFS(migrations)
	if err = goose.SetDialect("sqlite3"); err == nil {
		err = goose.Up(db, "migrations")
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("run database migrations: %w", err)
	}
	return db, nil
}

// OpenExisting opens a SQLite database with the normal connection pragmas without migrating it.
func OpenExisting(path string) (*sql.DB, error) {
	return sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
}
