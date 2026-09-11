package store_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/store"
	"github.com/DinethShakya23/kube-sre/internal/store/storetest"
)

func pgOnly(t *testing.T) *store.DB {
	t.Helper()
	db := storetest.New(t)
	if db.Dialect != store.Postgres {
		t.Skip("advisory locks need Postgres")
	}
	return db
}

func until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestLockKeyIsStableAndScoped(t *testing.T) {
	if store.LockKey("") != store.LockKey("") || store.LockKey("a") == store.LockKey("b") || store.LockKey("a") == store.LockKey("") {
		t.Error("keys must be stable and differ by scope")
	}
	if store.LockKey("cluster-1") < 0 {
		t.Error("63 bit")
	}
}

func TestOnlyOneReplicaLeadsAndAPeerTakesOver(t *testing.T) {
	db := pgOnly(t)
	ctx := context.Background()
	var acquired, lost atomic.Int32
	mk := func(id string) *store.Election {
		return &store.Election{DB: db, Scope: "leader-test-" + t.Name(), Poll: 30 * time.Millisecond, Identity: id,
			OnAcquire: func(context.Context) { acquired.Add(1) }, OnLose: func(context.Context) { lost.Add(1) }}
	}
	a, b := mk("a"), mk("b")
	a.Start(ctx)
	b.Start(ctx)
	defer b.Stop()
	if !a.IsLeader() || b.IsLeader() {
		t.Fatalf("a=%v b=%v", a.IsLeader(), b.IsLeader())
	}
	if st := b.Status(); st["is_leader"] != false || st["identity"] != "b" {
		t.Errorf("%v", st)
	}
	a.Stop()
	until(t, "b to take over", b.IsLeader)
	if acquired.Load() != 2 {
		t.Errorf("acquired %d", acquired.Load())
	}
}

func TestALostSessionIsNoticedAndLeadershipDropped(t *testing.T) {
	db := pgOnly(t)
	var lost atomic.Int32
	e := &store.Election{DB: db, Scope: "lose-" + t.Name(), Poll: 30 * time.Millisecond, OnLose: func(context.Context) { lost.Add(1) }}
	e.Start(context.Background())
	defer e.Stop()
	if !e.IsLeader() {
		t.Fatal("should lead")
	}
	// Kill the session that holds the lock from another connection.
	_, err := db.Exec(`SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype = 'advisory' AND granted AND pid <> pg_backend_pid()
		AND objid = $1::bigint & 4294967295`, store.LockKey("lose-"+t.Name()))
	if err != nil {
		t.Fatal(err)
	}
	until(t, "leadership to drop", func() bool { return !e.IsLeader() })
	if lost.Load() != 1 {
		t.Errorf("lost %d", lost.Load())
	}
	// And it can win it back.
	until(t, "reacquire", e.IsLeader)
}
