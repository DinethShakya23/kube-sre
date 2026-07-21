package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/DinethShakya23/kube-sre/internal/config"
)

func TestQRewritesForPostgres(t *testing.T) {
	pg := &DB{Dialect: Postgres}
	if got := pg.Q("INSERT INTO t (a, b) VALUES (?, ?)"); got != "INSERT INTO t (a, b) VALUES ($1, $2)" {
		t.Errorf("got %s", got)
	}
	lite := &DB{Dialect: SQLite}
	if got := lite.Q("SELECT ?"); got != "SELECT ?" {
		t.Errorf("sqlite should be unchanged: %s", got)
	}
}

func TestOpenPicksSQLiteByDefault(t *testing.T) {
	cfg := config.Load(func(k string) string {
		if k == "SQLITE_PATH" {
			return filepath.Join(t.TempDir(), "sub", "x.db")
		}
		return ""
	})
	db, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.Dialect != SQLite {
		t.Errorf("dialect=%s", db.Dialect)
	}
}

func TestMigrateSortsAndAppliesOnce(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	ms := []Migration{
		{2, "add b", `CREATE TABLE b (id INTEGER)`},
		{1, "add a", `CREATE TABLE a (id INTEGER)`},
	}
	if err = db.Migrate(ctx, ms); err != nil {
		t.Fatal(err)
	}
	if err = db.Migrate(ctx, ms); err != nil {
		t.Fatalf("second run must be a no-op: %v", err)
	}
	if v, _ := db.Version(ctx); v != 2 {
		t.Errorf("version=%d", v)
	}
	if _, err = db.Exec(`INSERT INTO a (id) VALUES (1)`); err != nil {
		t.Error(err)
	}
}

func TestMigrateRollsBackOnError(t *testing.T) {
	db, _ := OpenSQLite(filepath.Join(t.TempDir(), "r.db"))
	defer db.Close()
	ctx := context.Background()
	err := db.Migrate(ctx, []Migration{{1, "bad", `CREATE TABLE ok (id INTEGER); THIS IS NOT SQL`}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if v, _ := db.Version(ctx); v != 0 {
		t.Errorf("failed migration must not be recorded, version=%d", v)
	}
}
