// Package audit writes one request_log row per API request.
//
// Log never returns an error: failures are warnings. A failed connect at
// startup is the expected case on Kubernetes, since the API pod is often
// scheduled before the database accepts connections, so the connection is
// retried from the write path (at most once per RetryInterval) instead of
// disabling auditing for the life of the process. The state is reportable, and
// dropped rows are counted, so "the table looks empty" becomes a fact.
package audit

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/store"
)

type Request struct {
	RequestID, SessionID, UserID, UserRole string
	Path, Method                           string
	StatusCode                             int
	DurationMS                             float64
}

type Log struct {
	db            *store.DB
	RetryInterval time.Duration

	mu          sync.Mutex
	state       string // starting | ready | unavailable
	reason      string
	dropped     int64
	lastAttempt time.Time
}

func New(db *store.DB) *Log {
	return &Log{db: db, state: "starting", RetryInterval: 30 * time.Second}
}

// Start makes the first connection attempt.
func (l *Log) Start(ctx context.Context) { l.open(ctx) }

func (l *Log) open(ctx context.Context) {
	l.mu.Lock()
	l.lastAttempt = time.Now()
	l.mu.Unlock()
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := l.db.PingContext(pctx)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		l.state, l.reason = "unavailable", err.Error()
		slog.Warn("audit could not reach the database, requests are NOT being audited", "retry", l.RetryInterval, "err", err)
		return
	}
	l.state, l.reason = "ready", ""
	slog.Info("audit ready")
}

// Status is the shape reported on /healthz.
func (l *Log) Status() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return map[string]any{
		"enabled": l.state == "ready", "state": l.state, "reason": l.reason, "dropped": l.dropped,
	}
}

// drop counts an unrecorded request and speaks at a cadence that cannot flood.
// quiet is for the write failure path, which already logged its own reason.
func (l *Log) drop(quiet bool) {
	l.mu.Lock()
	l.dropped++
	n, state, reason := l.dropped, l.state, l.reason
	l.mu.Unlock()
	if !quiet && (n == 1 || n%100 == 0) {
		slog.Warn("audit requests not recorded", "count", n, "state", state, "reason", reason)
	}
}

// Write inserts one row. It never returns an error.
func (l *Log) Write(ctx context.Context, r Request) {
	l.mu.Lock()
	state, last := l.state, l.lastAttempt
	l.mu.Unlock()
	if state != "ready" {
		if time.Since(last) >= l.RetryInterval {
			l.open(ctx)
		}
		l.mu.Lock()
		state = l.state
		l.mu.Unlock()
		if state != "ready" {
			l.drop(false)
			return
		}
	}
	_, err := l.db.ExecContext(ctx, l.db.Q(`INSERT INTO request_log
		(request_id, session_id, user_id, user_role, path, method, status_code, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		r.RequestID, r.SessionID, r.UserID, r.UserRole, r.Path, r.Method, r.StatusCode, r.DurationMS)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "request_log") && (strings.Contains(msg, "does not exist") || strings.Contains(msg, "no such table")) {
			slog.Warn("audit: the request_log table is missing, run: kube-sre db-init")
		} else {
			slog.Warn("audit failed to write a request_log row", "err", err)
		}
		// Accepted by the pool and rejected by the database is as unrecorded as
		// a row that never had a connection.
		l.drop(true)
	}
}

var Migrations = []store.Migration{{
	Version: 20, Name: "request log",
	SQL: `
CREATE TABLE IF NOT EXISTS request_log (
    id          {{PK}},
    request_id  TEXT,
    session_id  TEXT,
    user_id     TEXT,
    user_role   TEXT,
    path        TEXT NOT NULL,
    method      TEXT NOT NULL,
    status_code INTEGER,
    duration_ms DOUBLE PRECISION,
    created_at  {{TS}} NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_request_log_user_id ON request_log (user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_request_log_session_id ON request_log (session_id, created_at DESC);`,
}}
