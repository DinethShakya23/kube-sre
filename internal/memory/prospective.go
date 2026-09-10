package memory

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Prospective memory: "remember to re-check condition C at or after time T". The
// canonical use is post fix verification. After the watchtower applies an autonomous
// fix it schedules a re-check ("did the fix hold?"); the consolidation pass fires the
// due ones and records the outcome. Scheduling is idempotent per (cluster, dedup
// key), so a flapping fix refreshes one re-check instead of piling up a backlog.

var ProspectiveMigrations = migrationsOf(45, "prospective memory", `
CREATE TABLE IF NOT EXISTS prospective_memory (
    id                {{PK}},
    cluster_id        TEXT NOT NULL,
    namespace         TEXT NOT NULL DEFAULT '',
    condition         TEXT NOT NULL,
    check_query       TEXT,
    dedup_key         TEXT NOT NULL,
    due_at            DOUBLE PRECISION NOT NULL,
    status            TEXT NOT NULL DEFAULT 'pending',
    outcome           TEXT,
    source_episode_id TEXT,
    created_by        TEXT,
    created_at        DOUBLE PRECISION NOT NULL,
    fired_at          DOUBLE PRECISION,
    UNIQUE (cluster_id, dedup_key)
);
CREATE INDEX IF NOT EXISTS idx_prospective_due ON prospective_memory (status, due_at);`)

// FireBatch bounds how many due re-checks one pass fires.
const FireBatch = 50

// Recheck is a due re-check row handed to the dispatcher.
type Recheck struct {
	ID              int64
	ClusterID       string
	Namespace       string
	Condition       string
	CheckQuery      string
	SourceEpisodeID string
}

// Outcomes a dispatcher may return.
const (
	OutcomeResolved    = "resolved"
	OutcomeStillBroken = "still_broken"
	OutcomeUnverified  = "unverified" // the read failed; not a grade
	OutcomeSkippedA0   = "skipped_a0"
	OutcomeError       = "error"
)

// DispatchFn re-verifies one due check and returns an outcome. It must be read only.
type DispatchFn func(ctx context.Context, r Recheck, level string) string

// LevelFn resolves a namespace's autonomy level.
type LevelFn func(namespace string) string

// terminal maps an outcome to its final status. A graded re-check is done; one that
// can never fire here (observe only namespace) is cancelled; anything else, an
// error or an unreadable cluster, goes back to pending so the next pass retries it.
// "unverified" must never be listed: it means we could not look, and closing a row on
// it is the exact failure this table exists to avoid. A retry does not advance the
// due time, so an unreadable cluster means up to FireBatch re-reads per pass.
var terminal = map[string]string{OutcomeResolved: "done", OutcomeStillBroken: "done", OutcomeSkippedA0: "cancelled"}

// ScheduleRecheck records a re-check due at dueAt (unix seconds). Re-scheduling the
// same dedup key refreshes the due time and reopens a row that already fired. It
// returns the row id, or 0.
func (s *Store) ScheduleRecheck(ctx context.Context, clusterID, condition string, dueAt float64, namespace, checkQuery, dedupKey, sourceEpisode, createdBy string) int64 {
	condition = strings.TrimSpace(condition)
	if clusterID == "" || condition == "" {
		return 0
	}
	if dedupKey == "" {
		dedupKey = condition
	}
	_, err := s.DB.ExecContext(ctx, s.DB.Q(`INSERT INTO prospective_memory
		(cluster_id, namespace, condition, check_query, dedup_key, due_at, source_episode_id, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (cluster_id, dedup_key) DO UPDATE SET condition = excluded.condition, check_query = excluded.check_query,
		  due_at = excluded.due_at, status = 'pending', outcome = NULL, fired_at = NULL`),
		clusterID, namespace, clip(condition, 1000), nullIfEmpty(checkQuery), clip(strings.TrimSpace(dedupKey), 500), dueAt,
		nullIfEmpty(sourceEpisode), nullIfEmpty(createdBy), s.now())
	if err != nil {
		slog.Warn("prospective schedule failed", "err", err)
		return 0
	}
	var id int64
	if err := s.DB.QueryRowContext(ctx, s.DB.Q(`SELECT id FROM prospective_memory WHERE cluster_id = ? AND dedup_key = ?`),
		clusterID, clip(strings.TrimSpace(dedupKey), 500)).Scan(&id); err != nil {
		return 0
	}
	return id
}

// claimDue flips due pending rows to fired in the statement that reads them, so two
// concurrent passes never fire the same row twice.
func (s *Store) claimDue(ctx context.Context) ([]Recheck, error) {
	lock := ""
	if s.distinctFrom() == "IS DISTINCT FROM" {
		lock = " FOR UPDATE SKIP LOCKED"
	}
	now := s.now()
	rows, err := s.DB.QueryContext(ctx, s.DB.Q(`UPDATE prospective_memory SET status = 'fired', fired_at = ?
		WHERE id IN (SELECT id FROM prospective_memory WHERE status = 'pending' AND due_at <= ? ORDER BY due_at LIMIT ?`+lock+`)
		RETURNING id, cluster_id, namespace, condition, check_query, source_episode_id`), now, now, FireBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Recheck
	for rows.Next() {
		var r Recheck
		var q, src *string
		if err := rows.Scan(&r.ID, &r.ClusterID, &r.Namespace, &r.Condition, &q, &src); err != nil {
			return nil, err
		}
		r.CheckQuery, r.SourceEpisodeID = deref(q), deref(src)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunProspectiveOnce fires every due pending re-check through the autonomy ladder,
// exactly like the watchtower: a re-check in an observe only namespace never fires.
// It returns the number fired and is a no-op with MEMORY_PROSPECTIVE off.
func (s *Store) RunProspectiveOnce(ctx context.Context, level LevelFn, atLeastA1 func(level string) bool, dispatch DispatchFn) int {
	if !s.Cfg.MemoryProspective {
		return 0
	}
	due, err := s.claimDue(ctx)
	if err != nil {
		slog.Warn("prospective claim failed", "err", err)
		s.Live.RecordPassFailure("prospective_fired", err)
		return 0
	}
	fired := 0
	for _, r := range due {
		lv := level(r.Namespace)
		outcome := OutcomeSkippedA0
		if atLeastA1(lv) {
			outcome = s.safeDispatch(ctx, dispatch, r, lv)
		}
		status, ok := terminal[outcome]
		if !ok {
			status = "pending"
		}
		if _, err := s.DB.ExecContext(ctx, s.DB.Q(`UPDATE prospective_memory SET outcome = ?, status = ? WHERE id = ?`), outcome, status, r.ID); err != nil {
			slog.Warn("prospective outcome not recorded", "id", r.ID, "err", err)
		}
		fired++
	}
	if fired > 0 {
		slog.Info("prospective re-checks fired", "count", fired)
	}
	return fired
}

func (s *Store) safeDispatch(ctx context.Context, dispatch DispatchFn, r Recheck, level string) (outcome string) {
	defer func() {
		if p := recover(); p != nil {
			slog.Warn("prospective dispatch panicked", "id", r.ID, "err", p)
			outcome = OutcomeError
		}
	}()
	if dispatch == nil {
		return OutcomeUnverified
	}
	return dispatch(ctx, r, level)
}

// ── retention ────────────────────────────────────────────────────────────────

// PruneBatch is the most rows deleted per table per pass, so a first run against
// years of history takes many passes and not one lock holding statement.
const PruneBatch = 5000

// RetentionRule is one prunable table: what ages out, measured on which column, and
// why it is safe.
type RetentionRule struct {
	Table, TSColumn, Why string
	// Epoch says the clock column holds unix seconds and not a timestamp.
	Epoch bool
	// Where limits which rows are eligible, such as finished work items.
	Where string
	// FloorDays is a minimum age this table is never pruned below.
	FloorDays int
	FloorWhy  string
}

// RetentionRules are the tables that may age out. These are telemetry and finished
// work items: nothing reads them after the window and nothing else's correctness
// depends on a row surviving.
var RetentionRules = []RetentionRule{
	{Table: "request_log", TSColumn: "created_at", Why: "API access telemetry; read by nothing after the fact and the fastest growing table."},
	{Table: "session_notes", TSColumn: "created_at", Epoch: true, Why: "scratch notes; nothing recalls them across sessions."},
	{Table: "prospective_memory", TSColumn: "created_at", Epoch: true, Where: "status IN ('done', 'cancelled')",
		Why: "re-checks already graded; the grade lives on in the episode."},
}

// RetentionRefused are the tables this pass never touches, with the reason. The
// hash chained ledgers are the sharp case: deleting the newest rows breaks no link,
// which is why the head anchors exist, and a scheduled prune would make the
// install's own housekeeping indistinguishable from tampering. Pruning a ledger
// needs a verified archive and a declared gap first, a deliberate manual act.
var RetentionRefused = map[string]string{
	"decision_log":      "hash chained tamper evidence; shorten it only through export then a declared truncation",
	"memory_audit":      "the same chain over the memory write path",
	"decision_log_head": "the anchor a truncation is checked against; deleting it deletes the evidence",
	"memory_chain_head": "the anchor for the memory write chain",
	"chain_truncation":  "the record that explains a gap in a chain",
	"episodes":          "learned operational history, and what the chains point at; forgetting is a deliberate act",
}

// EffectiveDays is the age a rule actually prunes at: the setting, clamped up by any floor.
func (r RetentionRule) EffectiveDays(days int) int {
	if r.FloorDays > days {
		return r.FloorDays
	}
	return days
}

// PruneOnce deletes rows past the retention window, at most PruneBatch per table,
// and returns the total. It is a no-op unless MEMORY_RETENTION_DAYS is positive.
// Tables and columns are package constants, never caller input.
func (s *Store) PruneOnce(ctx context.Context) int {
	days := s.Cfg.MemoryRetentionDays
	if days <= 0 {
		return 0
	}
	total := 0
	for _, rule := range RetentionRules {
		eff := rule.EffectiveDays(days)
		cutoff := time.Unix(int64(s.now()), 0).Add(-time.Duration(eff) * 24 * time.Hour)
		extra := ""
		if rule.Where != "" {
			extra = " AND " + rule.Where
		}
		res, err := s.DB.ExecContext(ctx, s.DB.Q(`DELETE FROM `+rule.Table+` WHERE id IN (SELECT id FROM `+rule.Table+
			` WHERE `+rule.TSColumn+` < ?`+extra+` LIMIT `+itoa(PruneBatch)+`)`), s.retentionArg(rule, cutoff))
		if err != nil {
			slog.Warn("retention prune failed", "table", rule.Table, "err", err)
			s.Live.RecordPassFailure("retention_pruned", err)
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			slog.Info("retention pruned", "table", rule.Table, "rows", n, "older_than_days", eff, "clamped", eff != days)
			total += int(n)
		}
	}
	return total
}

// retentionArg is the cutoff in the form the clock column compares. SQLite keeps a
// timestamp as text, so the argument must be text in the same layout.
func (s *Store) retentionArg(r RetentionRule, cutoff time.Time) any {
	switch {
	case r.Epoch:
		return float64(cutoff.UnixNano()) / 1e9
	case s.distinctFrom() == "IS NOT":
		return cutoff.UTC().Format("2006-01-02 15:04:05")
	}
	return cutoff.UTC()
}

func itoa(n int) string { return strconv.Itoa(n) }
