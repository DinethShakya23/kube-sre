package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/DinethShakya23/kube-sre/internal/redact"
)

// Operator preferences: how each user likes to operate.
//
//	explicit   set by the operator, confidence 1.0, never decays, never overwritten
//	           by inference
//	inferred   derived from the user's own behaviour; confidence grows as the
//	           behaviour repeats and decays when it stops, and stale low confidence
//	           ones are forgotten
//
// Inferred confidence never reaches 1.0, so an explicit preference always outranks
// an inferred one of the same key.
const (
	inferredCap  = 0.95
	inferredStep = 0.1
)

// ErrPrefsUnavailable means preferences could not be read, which is not the user
// having none. Both used to return an empty list, so a listing printed "no
// preferences" during a database outage, and an operator reading that had every
// reason to re-enter them.
var ErrPrefsUnavailable = errors.New("the preference store could not be read")

// Preference is one stored preference.
type Preference struct {
	Key        string  `json:"key"`
	Value      string  `json:"value"`
	Source     string  `json:"source"`
	Confidence float64 `json:"confidence"`
	Count      int     `json:"occurrence_count"`
}

// SetPreference upserts one preference and reports whether it was stored. An
// inferred write bumps the count and confidence toward the cap, and never
// changes an existing explicit preference.
func (s *Store) SetPreference(ctx context.Context, userID, key, value, source string, confidence *float64) bool {
	key = strings.TrimSpace(key)
	if r := []rune(key); len(r) > 120 {
		key = string(r[:120])
	}
	if userID == "" || key == "" {
		return false
	}
	value = redact.Secrets(value, 300)
	now := s.now()
	var err error
	if source == "inferred" {
		seed := 0.4
		if confidence != nil {
			seed = *confidence
		}
		if seed > inferredCap {
			seed = inferredCap
		}
		_, err = s.DB.ExecContext(ctx, s.DB.Q(`INSERT INTO user_prefs (user_id, key, value, source, confidence, occurrence_count, updated_at, last_seen_at)
			VALUES (?, ?, ?, 'inferred', ?, 1, ?, ?)
			ON CONFLICT (user_id, key) DO UPDATE SET
			  value = CASE WHEN user_prefs.source = 'explicit' THEN user_prefs.value ELSE excluded.value END,
			  confidence = CASE WHEN user_prefs.source = 'explicit' THEN user_prefs.confidence
			                    ELSE CASE WHEN user_prefs.confidence + ? > ? THEN ? ELSE user_prefs.confidence + ? END END,
			  occurrence_count = user_prefs.occurrence_count + 1,
			  updated_at = excluded.updated_at, last_seen_at = excluded.last_seen_at`),
			userID, key, value, seed, now, now, inferredStep, inferredCap, inferredCap, inferredStep)
	} else {
		_, err = s.DB.ExecContext(ctx, s.DB.Q(`INSERT INTO user_prefs (user_id, key, value, source, confidence, occurrence_count, updated_at, last_seen_at)
			VALUES (?, ?, ?, 'explicit', 1.0, 1, ?, ?)
			ON CONFLICT (user_id, key) DO UPDATE SET value = excluded.value, source = 'explicit', confidence = 1.0,
			  updated_at = excluded.updated_at, last_seen_at = excluded.last_seen_at`), userID, key, value, now, now)
	}
	if err != nil {
		slog.Warn("preference set failed", "key", key, "err", err)
		return false
	}
	return true
}

// RecallPreferences returns a user's active preferences, explicit first and then
// by confidence and recency. Inferred ones below the confidence floor or past the
// decay window are on their way to being forgotten and are left out.
func (s *Store) RecallPreferences(ctx context.Context, userID string, k int) ([]Preference, error) {
	if userID == "" {
		return nil, nil
	}
	decay := s.Cfg.PreferenceDecayDays
	if decay < 1 {
		decay = 1
	}
	rows, err := s.DB.QueryContext(ctx, s.DB.Q(`SELECT key, value, source, confidence, occurrence_count FROM user_prefs
		WHERE user_id = ? AND (source = 'explicit' OR (confidence >= ? AND last_seen_at > ?))
		ORDER BY (CASE WHEN source = 'explicit' THEN 1 ELSE 0 END) DESC, confidence DESC, last_seen_at DESC, key LIMIT ?`),
		userID, s.Cfg.PreferenceMinConf, s.now()-float64(decay)*86400, k)
	if err != nil {
		slog.Warn("preference recall failed", "err", err)
		return nil, fmt.Errorf("%w: %v", ErrPrefsUnavailable, err)
	}
	defer rows.Close()
	out := []Preference{}
	for rows.Next() {
		var p Preference
		if err := rows.Scan(&p.Key, &p.Value, &p.Source, &p.Confidence, &p.Count); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrPrefsUnavailable, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPrefsUnavailable, err)
	}
	return out, nil
}

// ForgetPreference deletes one preference. It is idempotent.
func (s *Store) ForgetPreference(ctx context.Context, userID, key string) bool {
	key = strings.TrimSpace(key)
	if r := []rune(key); len(r) > 120 {
		key = string(r[:120])
	}
	if userID == "" || key == "" {
		return false
	}
	if _, err := s.DB.ExecContext(ctx, s.DB.Q(`DELETE FROM user_prefs WHERE user_id = ? AND key = ?`), userID, key); err != nil {
		slog.Warn("preference forget failed", "err", err)
		return false
	}
	return true
}

// InferFromBehaviour is the deterministic learning pass. It learns
// default_namespace: when a user's RCA outcomes from the last 30 days are
// concentrated in one namespace (at least the minimum count and a 60% share), that
// namespace is recorded as an inferred preference with the share as confidence. It
// returns the number of users updated.
func (s *Store) InferFromBehaviour(ctx context.Context) int {
	minOcc := s.Cfg.PreferenceMinOccur
	if minOcc < 1 {
		minOcc = 1
	}
	rows, err := s.DB.QueryContext(ctx, s.DB.Q(`SELECT user_id, namespace, COUNT(*) FROM rca_outcomes
		WHERE namespace IS NOT NULL AND namespace <> '' AND created_at > ? GROUP BY user_id, namespace`), s.now()-30*86400)
	if err != nil {
		slog.Warn("preference inference failed", "err", err)
		s.Live.RecordPassFailure("prefs_inferred", err)
		return 0
	}
	type nsCount struct {
		ns string
		n  int
	}
	perUser := map[string][]nsCount{}
	for rows.Next() {
		var u, ns string
		var n int
		if err := rows.Scan(&u, &ns, &n); err != nil {
			rows.Close()
			s.Live.RecordPassFailure("prefs_inferred", err)
			return 0
		}
		perUser[u] = append(perUser[u], nsCount{ns, n})
	}
	rows.Close()

	updated := 0
	for user, list := range perUser {
		total, best := 0, nsCount{}
		for _, c := range list {
			total += c.n
			if c.n > best.n || (c.n == best.n && c.ns < best.ns) {
				best = c
			}
		}
		share := float64(best.n) / float64(total)
		if best.n < minOcc || share < 0.6 {
			continue
		}
		if share > inferredCap {
			share = inferredCap
		}
		if s.SetPreference(ctx, user, "default_namespace", best.ns, "inferred", &share) {
			updated++
		}
	}
	if updated > 0 {
		slog.Info("inferred default namespace", "users", updated)
	}
	return updated
}

// DecayAndForget purges stale, low confidence inferred preferences: not reinforced
// within the decay window and below the confidence floor. Explicit ones are
// immortal. It returns the rows forgotten.
func (s *Store) DecayAndForget(ctx context.Context) int {
	decay := s.Cfg.PreferenceDecayDays
	if decay < 1 {
		decay = 1
	}
	res, err := s.DB.ExecContext(ctx, s.DB.Q(`DELETE FROM user_prefs WHERE source = 'inferred' AND confidence < ? AND last_seen_at < ?`),
		s.Cfg.PreferenceMinConf, s.now()-float64(decay)*86400)
	if err != nil {
		slog.Warn("preference decay failed", "err", err)
		s.Live.RecordPassFailure("prefs_forgotten", err)
		return 0
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		slog.Info("forgot stale inferred preferences", "count", n)
	}
	return int(n)
}
