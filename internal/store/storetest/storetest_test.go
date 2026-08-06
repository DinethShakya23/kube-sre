package storetest

import (
	"testing"

	"github.com/DinethShakya23/kube-sre/internal/store"
)

func TestNewUsesTheRequestedDialect(t *testing.T) {
	db := New(t)
	want := store.SQLite
	if Postgres() {
		want = store.Postgres
	}
	if db.Dialect != want {
		t.Fatalf("dialect %s, want %s", db.Dialect, want)
	}
	var one int
	if err := db.QueryRow(`SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatal(err)
	}
	if Postgres() {
		var schema string
		if err := db.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil || len(schema) < 3 || schema[:2] != "t_" {
			t.Fatalf("expected an isolated test schema, got %q (%v)", schema, err)
		}
	}
}
