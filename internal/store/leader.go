package store

import (
	"context"
	"database/sql"
	"hash/crc32"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Single writer election, so the control plane can run more than one replica.
//
// Every background worker is a singleton by assumption: the sensorium opens a watch
// and feeds a detector engine, the watchtower turns findings into investigations and,
// at A3, into cluster writes, consolidation rewrites the memory graph. None of them
// coordinate with a peer. Scale to two replicas and the second does not share the
// work, it repeats it: two engines firing the same finding, two watchtowers opening
// an investigation for it. That is duplicated autonomous action against a production
// cluster, and it looks exactly like the cluster having two problems.
//
// The election is a Postgres advisory lock, not a Kubernetes lease. The lock is
// session scoped: it lives exactly as long as the connection that took it, so a
// killed pod or severed network releases it with no lease to expire and no renewal
// loop to get wrong. The cost is that leadership is tied to a database session, so
// a failover drops it, which is the right trade: on failover the workers stop
// rather than double up.
//
// It fails closed. Anything that is not a confirmed acquisition means not leader: a
// missing watchtower degrades to no autonomous action, visible on /healthz and safe.

const lockNamespace = "kube-sre/singleton-workers"

// LockKey is a stable 63 bit advisory lock key for a scope. It is scoped per cluster
// id when one is set: two deployments managing different clusters off one database
// must each get a leader, or the second would sit in standby watching nothing.
func LockKey(scope string) int64 {
	raw := lockNamespace
	if scope != "" {
		raw += ":" + scope
	}
	return int64(crc32.ChecksumIEEE([]byte(raw))) & 0x7FFFFFFFFFFFFFFF
}

// Election holds leadership for as long as its dedicated connection lives. It owns a
// connection of its own: a pooled one is returned after each query, and an advisory
// lock returned to a pool is a lock handed to whoever draws that connection next.
type Election struct {
	DB        *DB
	Scope     string
	Poll      time.Duration
	OnAcquire func(ctx context.Context)
	OnLose    func(ctx context.Context)
	Identity  string

	mu     sync.Mutex
	conn   *sql.Conn
	leader bool
	cancel context.CancelFunc
	done   chan struct{}
}

func (e *Election) key() int64 { return LockKey(e.Scope) }

func (e *Election) identity() string {
	if e.Identity != "" {
		return e.Identity
	}
	if h := os.Getenv("HOSTNAME"); h != "" {
		return h
	}
	return "pid-" + itoa(os.Getpid())
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// IsLeader is whether this process currently holds the lock.
func (e *Election) IsLeader() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.leader
}

// Status is the block reported on /healthz.
func (e *Election) Status() map[string]any {
	return map[string]any{"enabled": true, "is_leader": e.IsLeader(), "identity": e.identity(), "lock_key": e.key()}
}

func (e *Election) closeConn() {
	e.mu.Lock()
	c := e.conn
	e.conn = nil
	e.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

// tryAcquire is one attempt. A failed attempt is "not leader", never an error.
func (e *Election) tryAcquire(ctx context.Context) bool {
	e.mu.Lock()
	c := e.conn
	e.mu.Unlock()
	if c == nil {
		nc, err := e.DB.Conn(ctx)
		if err != nil {
			slog.Warn("leader election: could not open a session, remaining standby", "err", err)
			return false
		}
		e.mu.Lock()
		e.conn, c = nc, nc
		e.mu.Unlock()
	}
	var got bool
	if err := c.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, e.key()).Scan(&got); err != nil {
		slog.Warn("leader election: acquisition attempt failed, remaining standby", "err", err)
		e.closeConn()
		return false
	}
	return got
}

// stillHeld is a cheap check on the session that owns the lock. A leader that lost
// its connection has already lost the lock, since the database released it when the
// session ended, and noticing promptly is what stops a partitioned pod acting as
// leader while a peer legitimately takes over.
func (e *Election) stillHeld(ctx context.Context) bool {
	e.mu.Lock()
	c := e.conn
	e.mu.Unlock()
	if c == nil {
		return false
	}
	var one int
	if err := c.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		slog.Warn("leader election: lost the connection holding the lock", "err", err)
		e.closeConn()
		return false
	}
	return true
}

func (e *Election) setLeader(v bool) {
	e.mu.Lock()
	e.leader = v
	e.mu.Unlock()
}

func (e *Election) acquired(ctx context.Context) {
	e.setLeader(true)
	slog.Info("leader election: ACQUIRED leadership", "identity", e.identity(), "key", e.key())
	if e.OnAcquire != nil {
		e.OnAcquire(ctx)
	}
}

// Start attempts acquisition once, synchronously, then keeps trying in the background.
// The first attempt is awaited so the common case, one replica and an uncontended
// lock, is already leading when startup finishes.
func (e *Election) Start(ctx context.Context) {
	if e.Poll <= 0 {
		e.Poll = 10 * time.Second
	}
	ctx, e.cancel = context.WithCancel(ctx)
	e.done = make(chan struct{})
	if e.tryAcquire(ctx) {
		e.acquired(ctx)
	} else {
		slog.Info("leader election: STANDBY, another replica holds the lock; the API serves normally and singleton workers stay idle here", "identity", e.identity())
	}
	go func() {
		defer close(e.done)
		t := time.NewTicker(e.Poll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if e.IsLeader() {
				if !e.stillHeld(ctx) {
					e.setLeader(false)
					slog.Warn("leader election: LOST leadership, stopping singleton workers so a peer can take over without duplicating them", "identity", e.identity())
					if e.OnLose != nil {
						e.OnLose(ctx)
					}
				}
			} else if e.tryAcquire(ctx) {
				e.acquired(ctx)
			}
		}
	}()
}

// Stop ends the loop, releases the lock and closes the session.
func (e *Election) Stop() {
	if e.cancel != nil {
		e.cancel()
		<-e.done
	}
	e.mu.Lock()
	c, leader := e.conn, e.leader
	e.mu.Unlock()
	if leader && c != nil {
		_, _ = c.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, e.key())
	}
	e.setLeader(false)
	e.closeConn()
}

// SingleProcessStatus is the /healthz block when there is no election: SQLite is one
// process by construction, and Postgres election can be switched off.
func SingleProcessStatus(reason string) map[string]any {
	return map[string]any{"enabled": false, "is_leader": true, "reason": reason}
}
