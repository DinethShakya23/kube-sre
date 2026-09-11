package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/DinethShakya23/kube-sre/internal/store"
	"github.com/DinethShakya23/kube-sre/internal/store/storetest"
)

var two = []store.Migration{{Version: 1, Name: "a", SQL: "CREATE TABLE IF NOT EXISTS sa (id INTEGER)"}, {Version: 2, Name: "b", SQL: "CREATE TABLE IF NOT EXISTS sb (id INTEGER)"}}

func TestSchemaStateClassification(t *testing.T) {
	ctx := context.Background()
	db := storetest.New(t)

	if st := db.SchemaState(ctx, two); st["state"] != "unrecorded" {
		t.Errorf("%v", st)
	}
	if err := db.Migrate(ctx, two[:1]); err != nil {
		t.Fatal(err)
	}
	// Only v1 is applied here (the harness may have applied others first, so use a fresh set).
	st := db.SchemaState(ctx, two)
	if st["state"] != "stale" || !strings.Contains(st["reason"].(string), "[2]") || st["matches"] != false {
		t.Errorf("%v", st)
	}
	if err := db.Migrate(ctx, two); err != nil {
		t.Fatal(err)
	}
	if st = db.SchemaState(ctx, two); st["state"] != "current" || st["applied_version"] != 2 || st["expected_version"] != 2 || st["reason"] != "" {
		t.Errorf("%v", st)
	}
	// A rolled back binary: the database knows a version this build does not ship.
	if st = db.SchemaState(ctx, two[:1]); st["state"] != "ahead" || !strings.Contains(st["reason"].(string), "rolled back") {
		t.Errorf("%v", st)
	}
}

func TestSchemaStateUnknownWhenTheLedgerCannotBeRead(t *testing.T) {
	db := storetest.New(t)
	_, _ = db.Exec(`DROP TABLE schema_migrations`)
	if st := db.SchemaState(context.Background(), two); st["state"] != "unknown" || st["matches"] != false || st["applied_version"] != nil {
		t.Errorf("an unreadable ledger is not a verdict about the schema: %v", st)
	}
}
