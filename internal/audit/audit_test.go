package audit

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/store"
)

func testLog(t *testing.T, migrate bool) (*Log, *store.DB) {
	t.Helper()
	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if migrate {
		if err := db.Migrate(context.Background(), Migrations); err != nil {
			t.Fatal(err)
		}
	}
	l := New(db)
	l.Start(context.Background())
	return l, db
}

func TestWriteInsertsARow(t *testing.T) {
	l, db := testLog(t, true)
	l.Write(context.Background(), Request{RequestID: "r1", SessionID: "s", UserID: "u", UserRole: "admin",
		Path: "/chat", Method: "POST", StatusCode: 200, DurationMS: 12.5})
	var path, role string
	var status int
	var ms float64
	if err := db.QueryRow(`SELECT path, user_role, status_code, duration_ms FROM request_log WHERE request_id = 'r1'`).
		Scan(&path, &role, &status, &ms); err != nil {
		t.Fatal(err)
	}
	if path != "/chat" || role != "admin" || status != 200 || ms != 12.5 {
		t.Errorf("row: %s %s %d %v", path, role, status, ms)
	}
	if s := l.Status(); s["enabled"] != true || s["dropped"].(int64) != 0 {
		t.Errorf("status %v", s)
	}
}

func TestMissingTableCountsAsDropped(t *testing.T) {
	l, _ := testLog(t, false)
	l.Write(context.Background(), Request{Path: "/x", Method: "GET"})
	l.Write(context.Background(), Request{Path: "/y", Method: "GET"})
	if s := l.Status(); s["dropped"].(int64) != 2 {
		t.Errorf("a rejected row is as unrecorded as one with no connection: %v", s)
	}
}

func TestUnavailableRetriesAndCounts(t *testing.T) {
	l, _ := testLog(t, true)
	l.mu.Lock()
	l.state, l.reason, l.lastAttempt = "unavailable", "down", time.Now()
	l.mu.Unlock()
	l.Write(context.Background(), Request{Path: "/x", Method: "GET"})
	if s := l.Status(); s["dropped"].(int64) != 1 || s["enabled"] != false {
		t.Errorf("inside the retry window the row is dropped and counted: %v", s)
	}
	l.mu.Lock()
	l.lastAttempt = time.Now().Add(-time.Minute)
	l.mu.Unlock()
	l.Write(context.Background(), Request{Path: "/y", Method: "GET"})
	if s := l.Status(); s["enabled"] != true || s["dropped"].(int64) != 1 {
		t.Errorf("after the interval it reconnects and writes: %v", s)
	}
}
