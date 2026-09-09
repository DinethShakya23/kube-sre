package memory

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// Learned rules and theme summaries: the two layers between one episode and the
// prompt.
//
// Rules. The gap this closes: per episode reflections were stored, and a detector
// compiler existed, but there was no path turning a recurring, verified reflection
// into a reusable semantic rule.
//
//	verified, recurring episodes -> semantic rule (IF context THEN guidance)
//	recurrence >= N -> active -> injected into the prompt
//
// It is non parametric and training free, so it fits a closed weights API. It runs in
// the consolidation pass, behind MEMORY_PROMOTION.
//
// Summaries. The episode store remembers individual incidents but could not answer a
// theme level question ("what keeps failing in payments?") without scanning every
// one. Episodes are grouped by cluster and theme (first playbook, else namespace, else
// "general") into one summary each, deterministically, with no model call. A summary
// is rebuilt only when its theme changed: new episodes arrived or the cluster's graph
// edge count moved. A quiet cluster costs zero rebuilds.

var RuleMigrations = migrationsOf(44, "rules and summaries", `
CREATE TABLE IF NOT EXISTS semantic_rules (
    id               {{PK}},
    cluster_id       TEXT NOT NULL,
    context          TEXT NOT NULL,
    guidance         TEXT NOT NULL,
    source           TEXT NOT NULL DEFAULT 'episode',
    recurrence_count INTEGER NOT NULL DEFAULT 1,
    confidence       DOUBLE PRECISION NOT NULL DEFAULT 0.5,
    status           TEXT NOT NULL DEFAULT 'candidate',
    created_at       DOUBLE PRECISION NOT NULL,
    last_seen_at     DOUBLE PRECISION NOT NULL,
    UNIQUE (cluster_id, context)
);
CREATE INDEX IF NOT EXISTS idx_semantic_rules_active ON semantic_rules (cluster_id, status);
CREATE TABLE IF NOT EXISTS memory_summaries (
    id              {{PK}},
    cluster_id      TEXT NOT NULL,
    level           INTEGER NOT NULL DEFAULT 1,
    theme_key       TEXT NOT NULL,
    summary         TEXT NOT NULL,
    member_count    INTEGER NOT NULL DEFAULT 0,
    verified_count  INTEGER NOT NULL DEFAULT 0,
    last_episode_at DOUBLE PRECISION,
    kg_watermark    BIGINT NOT NULL DEFAULT 0,
    created_at      DOUBLE PRECISION NOT NULL,
    updated_at      DOUBLE PRECISION NOT NULL,
    UNIQUE (cluster_id, level, theme_key)
);
CREATE INDEX IF NOT EXISTS idx_memory_summaries_cluster ON memory_summaries (cluster_id, level);`)

// DefaultActivateAt is how many times a rule must recur before it is injected.
const DefaultActivateAt = 2

// Rule is one learned IF THEN rule.
type Rule struct {
	Context    string  `json:"context"`
	Guidance   string  `json:"guidance"`
	Recurrence int     `json:"recurrence_count"`
	Confidence float64 `json:"confidence"`
}

// RecordRule upserts one rule. A repeat sighting bumps the recurrence and the
// confidence and, once it reaches activateAt, flips the rule to active. A demoted
// rule stays demoted: a human said no, and a recurrence does not overrule that. It
// returns whether the rule was stored.
func (s *Store) RecordRule(ctx context.Context, clusterID, context_, guidance, source string, confidence float64, activateAt int) bool {
	if strings.TrimSpace(context_) == "" || strings.TrimSpace(guidance) == "" {
		return false
	}
	now := s.now()
	// Recurrence and status are decided in one statement so two writers cannot race
	// past each other.
	_, err := s.DB.ExecContext(ctx, s.DB.Q(`INSERT INTO semantic_rules (cluster_id, context, guidance, source, confidence, status, created_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, 'candidate', ?, ?)
		ON CONFLICT (cluster_id, context) DO UPDATE SET
		  recurrence_count = semantic_rules.recurrence_count + 1,
		  guidance = excluded.guidance,
		  confidence = CASE WHEN semantic_rules.confidence + 0.1 > 1.0 THEN 1.0 ELSE semantic_rules.confidence + 0.1 END,
		  last_seen_at = excluded.last_seen_at,
		  status = CASE WHEN semantic_rules.status = 'demoted' THEN 'demoted'
		                WHEN semantic_rules.recurrence_count + 1 >= ? THEN 'active'
		                ELSE semantic_rules.status END`),
		clusterID, clip(context_, 200), clip(guidance, 1000), source, confidence, now, now, activateAt)
	if err != nil {
		slog.Warn("record rule failed", "context", clip(context_, 40), "err", err)
		return false
	}
	return true
}

// DemoteRule stops a rule being injected, for good.
func (s *Store) DemoteRule(ctx context.Context, clusterID, context_ string) bool {
	res, err := s.DB.ExecContext(ctx, s.DB.Q(`UPDATE semantic_rules SET status = 'demoted' WHERE cluster_id = ? AND context = ?`), clusterID, context_)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

type episodeRow struct {
	cluster, namespace, rootCause string
	playbooks                     []string
	verified                      bool
	outcome                       string
	startedAt                     float64
}

func (s *Store) episodesForRollup(ctx context.Context) ([]episodeRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT cluster_id, namespace, root_cause, playbooks, verified, outcome, started_at FROM episodes
		WHERE cluster_id <> '' AND summary <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []episodeRow
	for rows.Next() {
		var r episodeRow
		var ns, rc, oc *string
		var pb []byte
		var v *bool
		if err := rows.Scan(&r.cluster, &ns, &rc, &pb, &v, &oc, &r.startedAt); err != nil {
			return nil, err
		}
		r.namespace, r.rootCause, r.outcome, r.verified = deref(ns), deref(rc), deref(oc), v != nil && *v
		r.playbooks = parseStrings(pb)
		out = append(out, r)
	}
	return out, rows.Err()
}

// PromoteFromEpisodes is the consolidation pass: verified episodes are grouped by
// (cluster, playbook), and a signature seen at least minRecurrence times becomes a
// rule whose guidance is the latest verified root cause. It returns the rules
// created or reinforced, and is a no-op with MEMORY_PROMOTION off.
func (s *Store) PromoteFromEpisodes(ctx context.Context, minRecurrence int) int {
	if !s.Cfg.MemoryPromotion {
		return 0
	}
	eps, err := s.episodesForRollup(ctx)
	if err != nil {
		slog.Warn("promotion failed", "err", err)
		s.Live.RecordPassFailure("rules_promoted", err)
		return 0
	}
	type sig struct{ cluster, playbook string }
	type agg struct {
		n      int
		latest float64
		rc     string
	}
	groups := map[sig]*agg{}
	for _, e := range eps {
		if !e.verified {
			continue
		}
		for _, pb := range e.playbooks {
			g := groups[sig{e.cluster, pb}]
			if g == nil {
				g = &agg{}
				groups[sig{e.cluster, pb}] = g
			}
			g.n++
			if e.startedAt >= g.latest {
				g.latest, g.rc = e.startedAt, e.rootCause
			}
		}
	}
	keys := make([]sig, 0, len(groups))
	for k, g := range groups {
		if g.n >= minRecurrence {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].cluster != keys[j].cluster {
			return keys[i].cluster < keys[j].cluster
		}
		return keys[i].playbook < keys[j].playbook
	})
	promoted := 0
	for _, k := range keys {
		guidance := groups[k].rc
		if guidance == "" {
			guidance = "(verified fix; see linked episodes)"
		}
		if s.RecordRule(ctx, k.cluster, k.playbook, guidance, "episode", 0.5, minRecurrence) {
			promoted++
		}
	}
	if promoted > 0 {
		slog.Info("promoted semantic rules", "count", promoted)
	}
	return promoted
}

// ErrRulesUnavailable means the learned rules could not be read.
var ErrRulesUnavailable = fmt.Errorf("the learned rules could not be read")

// ActiveRules are the prompt injectable rules for a cluster, most confident first.
func (s *Store) ActiveRules(ctx context.Context, clusterID string, limit int) ([]Rule, error) {
	rows, err := s.DB.QueryContext(ctx, s.DB.Q(`SELECT context, guidance, recurrence_count, confidence FROM semantic_rules
		WHERE cluster_id = ? AND status = 'active' ORDER BY confidence DESC, recurrence_count DESC, context LIMIT ?`), clusterID, limit)
	if err != nil {
		slog.Warn("active rules failed", "err", err)
		return nil, fmt.Errorf("%w: %v", ErrRulesUnavailable, err)
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var r Rule
		if err := rows.Scan(&r.Context, &r.Guidance, &r.Recurrence, &r.Confidence); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrRulesUnavailable, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RenderRulesBlock is the compact prompt block of learned rules.
func RenderRulesBlock(rules []Rule) string {
	if len(rules) == 0 {
		return ""
	}
	lines := []string{"## Learned rules (this cluster)"}
	for _, r := range rules {
		lines = append(lines, fmt.Sprintf("- IF %s THEN %s (seen ×%d)", r.Context, clip(strings.ReplaceAll(r.Guidance, "\n", " "), 200), r.Recurrence))
	}
	return strings.Join(lines, "\n")
}

// ── theme summaries ──────────────────────────────────────────────────────────

// Summary is one theme summary.
type Summary struct {
	Theme    string
	Summary  string
	Members  int
	Verified int
	Sim      float64
}

func renderSummary(theme string, n, verified int, outcomes, recentRCs []string) string {
	parts := []string{fmt.Sprintf("Theme '%s': %d episodes (%d verified).", theme, n, verified)}
	if len(outcomes) > 0 {
		parts = append(parts, "Outcomes: "+strings.Join(outcomes, ", ")+".")
	}
	if len(recentRCs) > 0 {
		parts = append(parts, "Recent root causes: "+strings.Join(recentRCs, " | ")+".")
	}
	return clip(strings.Join(parts, " "), 1500)
}

// BuildSummaryTree rebuilds the theme summaries whose theme changed, and returns how
// many were written. It is a no-op with MEMORY_SUMMARY_TREE off. Regeneration is
// tied to change: the summary is rewritten only if its newest episode, member count
// or the cluster's graph edge count moved, never on a clock alone.
func (s *Store) BuildSummaryTree(ctx context.Context) int {
	if !s.Cfg.MemorySummaryTree {
		return 0
	}
	eps, err := s.episodesForRollup(ctx)
	if err != nil {
		slog.Warn("summaries fetch failed", "err", err)
		s.Live.RecordPassFailure("summaries_built", err)
		return 0
	}
	watermark := map[string]int64{}
	wr, err := s.DB.QueryContext(ctx, `SELECT cluster_id, COUNT(*) FROM kg_edges GROUP BY cluster_id`)
	if err != nil {
		slog.Warn("summaries watermark failed", "err", err)
		s.Live.RecordPassFailure("summaries_built", err)
		return 0
	}
	for wr.Next() {
		var c string
		var n int64
		if wr.Scan(&c, &n) == nil {
			watermark[c] = n
		}
	}
	wr.Close()

	type key struct{ cluster, theme string }
	type group struct {
		n, verified int
		last        float64
		outcomes    map[string]bool
		rcs         []episodeRow
	}
	groups := map[key]*group{}
	for _, e := range eps {
		theme := "general"
		switch {
		case len(e.playbooks) > 0 && e.playbooks[0] != "":
			theme = e.playbooks[0]
		case e.namespace != "":
			theme = e.namespace
		}
		g := groups[key{e.cluster, theme}]
		if g == nil {
			g = &group{outcomes: map[string]bool{}}
			groups[key{e.cluster, theme}] = g
		}
		g.n++
		if e.verified {
			g.verified++
		}
		if e.startedAt > g.last {
			g.last = e.startedAt
		}
		if e.outcome != "" {
			g.outcomes[e.outcome] = true
		}
		if e.rootCause != "" {
			g.rcs = append(g.rcs, e)
		}
	}
	keys := make([]key, 0, len(groups))
	for k, g := range groups {
		if g.n >= s.Cfg.MemorySummaryMin {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].cluster != keys[j].cluster {
			return keys[i].cluster < keys[j].cluster
		}
		return keys[i].theme < keys[j].theme
	})

	written := 0
	now := s.now()
	for _, k := range keys {
		g := groups[k]
		var outs []string
		for o := range g.outcomes {
			outs = append(outs, o)
		}
		sort.Strings(outs)
		sort.SliceStable(g.rcs, func(i, j int) bool { return g.rcs[i].startedAt > g.rcs[j].startedAt })
		var recent []string
		for i := 0; i < len(g.rcs) && i < 3; i++ {
			recent = append(recent, clip(strings.ReplaceAll(g.rcs[i].rootCause, "\n", " "), 120))
		}
		text := renderSummary(k.theme, g.n, g.verified, outs, recent)
		// Only rewrite when the theme actually changed; a quiet theme is never rebuilt.
		res, err := s.DB.ExecContext(ctx, s.DB.Q(fmt.Sprintf(`INSERT INTO memory_summaries
			(cluster_id, level, theme_key, summary, member_count, verified_count, last_episode_at, kg_watermark, created_at, updated_at)
			VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (cluster_id, level, theme_key) DO UPDATE SET
			  summary = excluded.summary, member_count = excluded.member_count, verified_count = excluded.verified_count,
			  last_episode_at = excluded.last_episode_at, kg_watermark = excluded.kg_watermark, updated_at = excluded.updated_at
			WHERE excluded.last_episode_at %s memory_summaries.last_episode_at
			   OR excluded.member_count <> memory_summaries.member_count
			   OR excluded.kg_watermark <> memory_summaries.kg_watermark`, s.distinctFrom())),
			k.cluster, k.theme, text, g.n, g.verified, g.last, watermark[k.cluster], now, now)
		if err != nil {
			slog.Warn("summary upsert failed", "theme", k.theme, "err", err)
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			written++
		}
	}
	if written > 0 {
		slog.Info("rebuilt theme summaries", "count", written)
	}
	return written
}

// ErrSummariesUnavailable means the theme summaries could not be read.
var ErrSummariesUnavailable = fmt.Errorf("the theme summaries could not be read")

// RecallThemeSummaries returns the top k theme summaries for a theme level question,
// by trigram similarity, above the recall floor.
func (s *Store) RecallThemeSummaries(ctx context.Context, query, clusterID string, k int) ([]Summary, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	rows, err := s.DB.QueryContext(ctx, s.DB.Q(`SELECT theme_key, summary, member_count, verified_count FROM memory_summaries WHERE cluster_id = ? AND level = 1`), clusterID)
	if err != nil {
		slog.Warn("summary recall failed", "err", err)
		return nil, fmt.Errorf("%w: %v", ErrSummariesUnavailable, err)
	}
	defer rows.Close()
	q := trigrams(clip(query, 500))
	var out []Summary
	for rows.Next() {
		var sm Summary
		if err := rows.Scan(&sm.Theme, &sm.Summary, &sm.Members, &sm.Verified); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrSummariesUnavailable, err)
		}
		sm.Sim = similarity(q, trigrams(sm.Theme+" "+sm.Summary))
		if sm.Sim > s.Cfg.MemorySimFloor {
			out = append(out, sm)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Sim != out[j].Sim {
			return out[i].Sim > out[j].Sim
		}
		return out[i].Members > out[j].Members
	})
	if len(out) > k {
		out = out[:k]
	}
	return out, rows.Err()
}

// RenderSummariesBlock is the compact prompt block for theme level context.
func RenderSummariesBlock(sums []Summary) string {
	if len(sums) == 0 {
		return ""
	}
	lines := []string{"## Memory themes (this cluster)"}
	for _, s := range sums {
		lines = append(lines, "- "+s.Summary)
	}
	return strings.Join(lines, "\n")
}
