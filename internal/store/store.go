package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/DinethShakya23/kube-sre/internal/config"
)

const (
	SQLite   = "sqlite"
	Postgres = "postgres"
)

type DB struct {
	*sql.DB
	Dialect string
}

// Open connects to Postgres when configured, otherwise to a local SQLite file.
func Open(cfg *config.Config) (*DB, error) {
	if cfg.Postgres() {
		db, err := sql.Open("pgx", cfg.DSN())
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(cfg.PGPoolMax)
		db.SetMaxIdleConns(cfg.PGPoolMin)
		if err = db.Ping(); err != nil {
			db.Close()
			return nil, fmt.Errorf("postgres: %w", err)
		}
		return &DB{DB: db, Dialect: Postgres}, nil
	}
	return OpenSQLite(cfg.SQLitePath)
}

func OpenSQLite(path string) (*DB, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// one writer at a time keeps sqlite from locking itself
	db.SetMaxOpenConns(1)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{DB: db, Dialect: SQLite}, nil
}

// Q rewrites ? placeholders to $1, $2 ... for Postgres.
func (d *DB) Q(query string) string {
	if d.Dialect != Postgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Migrate applies any migration not yet recorded, in version order.
func (d *DB) Migrate(ctx context.Context, migrations []Migration) error {
	_, err := d.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL)`)
	if err != nil {
		return err
	}
	applied := map[int]bool{}
	rows, err := d.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int
		if err = rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()

	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	for _, m := range migrations {
		if applied[m.Version] {
			continue
		}
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, m.SQL); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d %s: %w", m.Version, m.Name, err)
		}
		_, err = tx.ExecContext(ctx, d.Q(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`),
			m.Version, m.Name, time.Now().UTC().Format(time.RFC3339))
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Version is the highest applied migration, or 0.
func (d *DB) Version(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := d.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	return int(v.Int64), err
}
