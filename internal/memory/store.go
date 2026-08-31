// Package memory is the agent's long term memory: operator preferences, learned
// failure patterns, past root cause analyses, and (in later files) episodes and a
// temporal knowledge graph.
//
// The store loads a pinned context for the coordinator prompt. Reads are ordered
// by priority and the whole block stays inside about 500 tokens.
package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/store"
)

const maxContextChars = 1800 // about 500 tokens

// ErrUnavailable means the pinned context could not be loaded, which is not the
// same as having none. An empty string is exactly what a brand new user with no
// preferences, hints or history produces, so a database outage once reached the
// coordinator as a clean slate: no preferences to honour, no prior RCA to build
// on, and nothing saying the lookup had failed.
var ErrUnavailable = errors.New("pinned memory context unavailable")

// Store reads and writes the memory tables.
type Store struct {
	DB  *store.DB
	Cfg *config.Config
	Now func() time.Time
	// Live counts what memory did, for /healthz.
	Live *Liveness
	// Guard, when set, decides whether a user derived write is admitted and keeps the
	// tamper evident audit chain.
	Guard WriteGuard
	// OnEpisode runs after an episode is written, for side effects such as recording
	// an autonomous attempt.
	OnEpisode func(ctx context.Context, id string, in EpisodeInput)
}

// Decision is the write guard's verdict.
type Decision struct {
	Admit  bool
	Reason string
	Trust  float64
}

// WriteGuard is the memory write admission guard and audit chain.
type WriteGuard interface {
	Admit(sourceKind, requester, text string) Decision
	Audit(ctx context.Context, clusterID, kind, refID string, payload map[string]any)
}

func NewStore(db *store.DB, cfg *config.Config) *Store {
	return &Store{DB: db, Cfg: cfg, Now: time.Now, Live: NewLiveness()}
}

func (s *Store) now() float64 { return float64(s.Now().UnixNano()) / 1e9 }

// Migrations for the tables in this file.
var StoreMigrations = []store.Migration{{
	Version: 40, Name: "memory store",
	SQL: `
CREATE TABLE IF NOT EXISTS user_prefs (
    user_id          TEXT NOT NULL,
    key              TEXT NOT NULL,
    value            TEXT NOT NULL,
    source           TEXT NOT NULL DEFAULT 'explicit',
    confidence       DOUBLE PRECISION NOT NULL DEFAULT 1.0,
    occurrence_count INTEGER NOT NULL DEFAULT 1,
    updated_at       DOUBLE PRECISION NOT NULL,
    last_seen_at     DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (user_id, key)
);
CREATE INDEX IF NOT EXISTS idx_user_prefs_active ON user_prefs (user_id, confidence DESC, last_seen_at DESC);
CREATE TABLE IF NOT EXISTS session_notes (
    id         {{PK}},
    session_id TEXT NOT NULL,
    note       TEXT NOT NULL,
    created_at DOUBLE PRECISION NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_session_notes_session_id ON session_notes (session_id);
CREATE TABLE IF NOT EXISTS rca_outcomes (
    id                {{PK}},
    session_id        TEXT NOT NULL,
    user_id           TEXT NOT NULL,
    root_cause        TEXT NOT NULL,
    confidence        DOUBLE PRECISION NOT NULL,
    recommended_fix   TEXT NOT NULL,
    outcome_feedback  TEXT,
    cluster_id        TEXT NOT NULL DEFAULT 'unknown',
    namespace         TEXT,
    verified_resolved BOOLEAN,
    playbooks_matched {{JSON}} NOT NULL DEFAULT '[]',
    created_by_role   TEXT,
    request_id        TEXT,
    created_at        DOUBLE PRECISION NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_rca_outcomes_user_id ON rca_outcomes (user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_rca_outcomes_cluster ON rca_outcomes (cluster_id, created_at DESC);
CREATE TABLE IF NOT EXISTS failure_patterns (
    pattern_name     TEXT NOT NULL,
    cluster_id       TEXT NOT NULL DEFAULT 'unknown',
    description      TEXT NOT NULL,
    recommended_fix  TEXT NOT NULL,
    confidence       DOUBLE PRECISION NOT NULL DEFAULT 0.0,
    occurrence_count INTEGER NOT NULL DEFAULT 0,
    namespace        TEXT,
    demoted          BOOLEAN NOT NULL DEFAULT FALSE,
    last_seen_at     DOUBLE PRECISION NOT NULL,
    created_at       DOUBLE PRECISION NOT NULL,
    updated_at       DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (pattern_name, cluster_id)
);
CREATE INDEX IF NOT EXISTS idx_failure_patterns_active ON failure_patterns (cluster_id, last_seen_at DESC);`,
}}

// ── the pinned context ───────────────────────────────────────────────────────

// LoadContext returns the pinned context for a coordinator prompt: operator
// preferences, known failure patterns, session notes and recent RCA history.
func (s *Store) LoadContext(ctx context.Context, userID, sessionID, clusterID string) (string, error) {
	if err := s.DB.PingContext(ctx); err != nil {
		slog.Warn("memory store could not load context", "err", err)
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	var parts, failed []string
	sections := []struct {
		label string
		load  func() ([]string, error)
	}{
		{"operator preferences", func() ([]string, error) { return s.loadPrefs(ctx, userID) }},
		{"failure hints", func() ([]string, error) { return s.loadFailureHints(ctx, clusterID) }},
		{"session notes", func() ([]string, error) { return s.loadSessionNotes(ctx, sessionID) }},
		{"past RCA", func() ([]string, error) { return s.loadPastRCA(ctx, userID, clusterID) }},
	}
	for _, sec := range sections {
		// Per section, so one failing query does not cost the other three, but never
		// silently: a query error in exactly one section would otherwise produce a
		// context that reads as complete, and the model would conclude the user has no
		// stored preferences. "Could not load" is not "has none" for a partial failure
		// any more than for a total one.
		got, err := sec.load()
		if err != nil {
			slog.Warn("memory store section failed", "section", sec.label, "err", err)
			failed = append(failed, sec.label)
			continue
		}
		parts = append(parts, got...)
	}
	var nonEmpty []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	combined := strings.Join(nonEmpty, "\n\n")
	if len(failed) > 0 {
		// Prepended, not appended: the tail is what the cap removes, and a notice the
		// model never sees is the same as no notice.
		notice := partialFailureNotice(failed)
		if combined != "" {
			combined = notice + "\n\n" + combined
		} else {
			combined = notice
		}
	}
	if r := []rune(combined); len(r) > maxContextChars {
		combined = string(r[:maxContextChars]) + "\n... [context truncated]"
	}
	return combined, nil
}

// UnavailableNotice is what the model reads when the whole pinned context failed.
func UnavailableNotice() string {
	return "## Memory unavailable\nStored operator preferences, failure hints and past RCA could NOT be loaded - this is not the " +
		"same as there being none. Do not assume the user has no preferences or that this issue has no precedent; " +
		"say that stored history could not be checked."
}

func partialFailureNotice(failed []string) string {
	return "## Memory partially unavailable\nStored " + strings.Join(failed, ", ") + " could NOT be read - this is not the same as " +
		"there being none. Do not assume the user has no preferences or that this issue has no precedent; say that part of " +
		"the stored history could not be checked."
}

// Every loader below caps its rows so the whole block stays inside the token
// budget. The cap is right; presenting the result as the operator's COMPLETE
// remembered state is not: an operator with 12 preferences once got 8 in the
// prompt, and "NEVER drain node-07, it hosts the license server" was one of the 4
// dropped, under a header reading "remembered". A capped section must not read as
// a complete one, for the same reason a missing section must not read as an empty
// one. Each loader asks for cap+1 rows and renders cap: getting the extra row back
// is proof that more exist, and not getting it is proof that they do not. One
// query, and the notice never claims a count it did not measure.
func moreNotice(noun string) string {
	return "  ... MORE " + noun + " are stored than the ones listed above and are NOT shown here " +
		"(oldest or lowest ranked omitted for space). Absence from this list is NOT evidence that none exists - " +
		"ask, or say you only checked the most relevant."
}

func (s *Store) loadPrefs(ctx context.Context, userID string) ([]string, error) {
	if !s.Cfg.PreferenceMemory {
		return nil, nil
	}
	decay := s.Cfg.PreferenceDecayDays
	if decay < 1 {
		decay = 1
	}
	cutoff := s.now() - float64(decay)*86400
	rows, err := s.DB.QueryContext(ctx, s.DB.Q(`
		SELECT key, value, source, confidence FROM user_prefs
		WHERE user_id = ? AND (source = 'explicit' OR (confidence >= ? AND last_seen_at > ?))
		ORDER BY (CASE WHEN source = 'explicit' THEN 1 ELSE 0 END) DESC, confidence DESC, key
		LIMIT 9`), userID, s.Cfg.PreferenceMinConf, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines []string
	n := 0
	for rows.Next() {
		var key, value, source string
		var conf float64
		if err := rows.Scan(&key, &value, &source, &conf); err != nil {
			return nil, err
		}
		n++
		if n > 8 {
			lines = append(lines, moreNotice("operator preferences"))
			break
		}
		if source == "inferred" {
			lines = append(lines, fmt.Sprintf("  %s: %s  (inferred, confidence %.0f%%)", key, value, conf*100))
		} else {
			lines = append(lines, fmt.Sprintf("  %s: %s", key, value))
		}
	}
	if len(lines) == 0 {
		return nil, rows.Err()
	}
	return []string{"## Operator Preferences (remembered)\n" + strings.Join(lines, "\n")}, rows.Err()
}

// loadFailureHints reads learned failure patterns with confidence of at least 0.9
// and at least two occurrences. They are filtered by the current cluster and the
// decay window, so patterns from other clusters and stale ones do not pollute the
// prompt.
func (s *Store) loadFailureHints(ctx context.Context, clusterID string) ([]string, error) {
	decay := s.Cfg.ReflexionDecayDays
	if decay < 1 {
		decay = 1
	}
	cutoff := s.now() - float64(decay)*86400
	rows, err := s.DB.QueryContext(ctx, s.DB.Q(`
		SELECT pattern_name, description, recommended_fix, occurrence_count FROM failure_patterns
		WHERE confidence >= 0.9 AND occurrence_count >= 2 AND demoted = ?
		  AND cluster_id IN (?, 'unknown') AND last_seen_at > ?
		ORDER BY occurrence_count DESC, last_seen_at DESC LIMIT 6`), false, clusterID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []string
	n := 0
	for rows.Next() {
		var name, desc, fix string
		var count int
		if err := rows.Scan(&name, &desc, &fix, &count); err != nil {
			return nil, err
		}
		n++
		if n > 5 {
			items = append(items, moreNotice("known failure patterns for this cluster"))
			break
		}
		items = append(items, fmt.Sprintf("  - [%s] (seen %d×) %s\n    → Fix: %s", name, count, desc, fix))
	}
	if n == 0 {
		return nil, rows.Err()
	}
	slog.Info("failure hints loaded", "count", min(n, 5), "cluster", clusterID)
	return []string{"## Known Failure Patterns (this cluster)\n" + strings.Join(items, "\n")}, rows.Err()
}

func (s *Store) loadSessionNotes(ctx context.Context, sessionID string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, s.DB.Q(`SELECT note FROM session_notes WHERE session_id = ? ORDER BY created_at DESC LIMIT 4`), sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notes []string
	n := 0
	for rows.Next() {
		var note string
		if err := rows.Scan(&note); err != nil {
			return nil, err
		}
		n++
		if n > 3 {
			notes = append(notes, moreNotice("notes from this session"))
			break
		}
		notes = append(notes, "  - "+note)
	}
	if n == 0 {
		return nil, rows.Err()
	}
	return []string{"## Session Notes\n" + strings.Join(notes, "\n")}, rows.Err()
}

// loadPastRCA reads the last three unrefuted RCA outcomes for the user on this
// cluster. It is cluster scoped, so someone investigating prod does not see hints
// from their dev cluster sessions.
func (s *Store) loadPastRCA(ctx context.Context, userID, clusterID string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, s.DB.Q(`
		SELECT root_cause, recommended_fix, namespace, created_at, verified_resolved FROM rca_outcomes
		WHERE user_id = ? AND cluster_id IN (?, 'unknown') AND verified_resolved IS NOT FALSE
		ORDER BY verified_resolved DESC NULLS LAST, created_at DESC LIMIT 4`), userID, clusterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines []string
	n := 0
	for rows.Next() {
		var cause, fix string
		var ns sql.NullString
		var at float64
		var verified sql.NullBool
		if err := rows.Scan(&cause, &fix, &ns, &at, &verified); err != nil {
			return nil, err
		}
		n++
		if n > 3 {
			lines = append(lines, moreNotice("past RCA outcomes for this cluster"))
			break
		}
		nsText := ""
		if ns.Valid && ns.String != "" {
			nsText = " ns=" + ns.String
		}
		mark := "?"
		if verified.Valid && verified.Bool {
			mark = "✓"
		}
		// The stored fix is cut for the prompt, and says so: a remediation cut
		// mid command reads as a complete command.
		preview := fix
		if r := []rune(fix); len(r) > 160 {
			preview = string(r[:160]) + " …[fix truncated, not the whole command]"
		}
		date := time.Unix(int64(at), 0).UTC().Format("2006-01-02")
		lines = append(lines, fmt.Sprintf("  - [%s%s %s] %s\n    → %s", date, nsText, mark, cause, preview))
	}
	if n == 0 {
		return nil, rows.Err()
	}
	return []string{"## Recent RCA History (this cluster)\n" + strings.Join(lines, "\n")}, rows.Err()
}

// ── the outcome recorder ─────────────────────────────────────────────────────

// RCAOutcome is one recorded investigation result.
type RCAOutcome struct {
	SessionID, UserID, RootCause string
	Confidence                   float64
	RecommendedFix               string
	Feedback                     *string // resolved | partial | regression | incorrect
	ClusterID                    string
	Namespace                    string
	Verified                     *bool
	Playbooks                    []string
	Role                         string
	RequestID                    string
}

// RecordOutcome persists an RCA outcome, and seeds a failure pattern if it is
// eligible. Pattern seeding needs confidence of at least 0.9 AND a verified
// resolution: without verification a pattern cannot promote, which is the gate
// that stops "the agent typed kubectl" masquerading as "the agent fixed the
// cluster". It never returns an error: a memory write must not break a turn.
func (s *Store) RecordOutcome(ctx context.Context, o RCAOutcome) {
	if o.ClusterID == "" {
		o.ClusterID = "unknown"
	}
	pb, _ := json.Marshal(append([]string{}, o.Playbooks...))
	_, err := s.DB.ExecContext(ctx, s.DB.Q(`INSERT INTO rca_outcomes
		(session_id, user_id, root_cause, confidence, recommended_fix, outcome_feedback, cluster_id, namespace,
		 verified_resolved, playbooks_matched, created_by_role, request_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		o.SessionID, o.UserID, o.RootCause, o.Confidence, o.RecommendedFix, nullStr(o.Feedback), o.ClusterID,
		nullIfEmpty(o.Namespace), nullBool(o.Verified), string(pb), nullIfEmpty(o.Role), nullIfEmpty(o.RequestID), s.now())
	if err != nil {
		slog.Warn("record rca outcome failed", "err", err)
		return
	}
	regression := o.Feedback != nil && *o.Feedback == "regression"
	eligible := o.Confidence >= 0.9 && o.Verified != nil && *o.Verified && !regression
	if eligible {
		s.maybeSeedPattern(ctx, o)
	}
	slog.Info("reflexion outcome recorded", "session", o.SessionID, "cluster", o.ClusterID,
		"verified", o.Verified != nil && *o.Verified, "confidence", o.Confidence, "eligible_for_pattern", eligible)
}

func nullStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullBool(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

// maybeSeedPattern upserts a failure pattern scoped to (name, cluster). It honours
// a cooldown: if the pattern was updated within REFLEXION_PATTERN_COOLDOWN_HOURS
// the occurrence count does not bump, so a test loop cannot inflate the counter.
func (s *Store) maybeSeedPattern(ctx context.Context, o RCAOutcome) {
	name := clipRunes(o.RootCause, 120)
	now := s.now()
	cooldown := s.Cfg.ReflexionCooldownHrs
	if cooldown < 0 {
		cooldown = 0
	}
	var count int
	var last float64
	err := s.DB.QueryRowContext(ctx, s.DB.Q(`SELECT occurrence_count, last_seen_at FROM failure_patterns WHERE pattern_name = ? AND cluster_id = ?`),
		name, o.ClusterID).Scan(&count, &last)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = s.DB.ExecContext(ctx, s.DB.Q(`INSERT INTO failure_patterns
			(pattern_name, cluster_id, description, recommended_fix, confidence, occurrence_count, namespace, last_seen_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`),
			name, o.ClusterID, name, o.RecommendedFix, o.Confidence, nullIfEmpty(o.Namespace), now, now, now)
		if err != nil {
			slog.Warn("seed failure pattern failed", "err", err)
			return
		}
		slog.Info("reflexion seeded pattern", "pattern", name, "cluster", o.ClusterID)
	case err != nil:
		slog.Warn("seed failure pattern failed", "err", err)
	case now-last < float64(cooldown)*3600:
		// Refresh last seen and the fix; keep the count steady.
		_, err = s.DB.ExecContext(ctx, s.DB.Q(`UPDATE failure_patterns SET recommended_fix = ?,
			confidence = CASE WHEN confidence > ? THEN confidence ELSE ? END, last_seen_at = ?, updated_at = ?
			WHERE pattern_name = ? AND cluster_id = ?`), o.RecommendedFix, o.Confidence, o.Confidence, now, now, name, o.ClusterID)
		if err != nil {
			slog.Warn("refresh failure pattern failed", "err", err)
			return
		}
		slog.Info("reflexion cooldown active, count not bumped", "pattern", name)
	default:
		_, err = s.DB.ExecContext(ctx, s.DB.Q(`UPDATE failure_patterns SET occurrence_count = occurrence_count + 1, recommended_fix = ?,
			confidence = CASE WHEN confidence > ? THEN confidence ELSE ? END, last_seen_at = ?, updated_at = ?, demoted = ?
			WHERE pattern_name = ? AND cluster_id = ?`), o.RecommendedFix, o.Confidence, o.Confidence, now, now, false, name, o.ClusterID)
		if err != nil {
			slog.Warn("bump failure pattern failed", "err", err)
			return
		}
		slog.Info("reflexion bumped pattern", "pattern", name, "count", count+1)
	}
}

// DemotePattern marks a pattern so it stops being injected. Idempotent. Called when
// a pattern produces a regression or the user flags a hint as wrong; it returns to
// the prompt only through fresh verified outcomes.
func (s *Store) DemotePattern(ctx context.Context, name, clusterID string) {
	if clusterID == "" {
		clusterID = "unknown"
	}
	if _, err := s.DB.ExecContext(ctx, s.DB.Q(`UPDATE failure_patterns SET demoted = ?, updated_at = ? WHERE pattern_name = ? AND cluster_id = ?`),
		true, s.now(), clipRunes(name, 120), clusterID); err != nil {
		slog.Warn("demote pattern failed", "err", err)
		return
	}
	slog.Info("reflexion demoted pattern", "pattern", name, "cluster", clusterID)
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
