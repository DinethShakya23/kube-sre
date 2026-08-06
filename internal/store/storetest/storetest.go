// Package storetest gives tests a database. It is SQLite by default. When
// KUBESRE_TEST_PG_DSN is set it is Postgres instead, in a schema of its own so
// tests cannot see each other. Run the suite both ways:
//
//	go test ./...
//	KUBESRE_TEST_PG_DSN=postgres://postgres:test@127.0.0.1:55432/kubesre go test -p 1 ./...
package storetest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"path/filepath"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/DinethShakya23/kube-sre/internal/store"
)

// Postgres reports whether the suite is running against Postgres.
func Postgres() bool { return dsn() != "" }

func dsn() string { return getenv("KUBESRE_TEST_PG_DSN") }

// New opens a fresh database with the given migrations applied.
func New(t testing.TB, migrations ...[]store.Migration) *store.DB {
	t.Helper()
	var db *store.DB
	if d := dsn(); d != "" {
		db = newPostgres(t, d)
	} else {
		var err error
		db, err = store.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
	}
	var all []store.Migration
	for _, m := range migrations {
		all = append(all, m...)
	}
	if err := db.Migrate(context.Background(), all); err != nil {
		t.Fatal(err)
	}
	return db
}

func newPostgres(t testing.TB, base string) *store.DB {
	t.Helper()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	raw, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(4)
	t.Cleanup(func() {
		raw.Close()
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		admin.Close()
	})
	return &store.DB{DB: raw, Dialect: store.Postgres}
}
