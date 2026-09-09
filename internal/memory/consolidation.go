package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// Consolidation is the "sleep" phase: deterministic passes over what was learned,
// with no model calls. Each pass is guarded on its own, so one failing pass never
// stops the passes after it, but a failure is said out loud:
//
// every pass returns an int, and 0 means both "it ran and there was nothing to do"
// and "it raised and its own guard caught it". Measured against a pool whose every
// statement raises, with a healthy idle pool as the control, the counters were
// byte identical. The failure register is what separates a healthy idle cluster from
// a dead subsystem.

const (
	ConsolidationInterval = 10 * time.Minute
	staleEdgeHours        = 24
)

// PlaybookDetect looks up the compiled detector of a playbook: its promql, its
// debounce, and whether it has one.
type PlaybookDetect func(name string) (promql []string, debounce int, ok bool)

// Pass is one extra consolidation pass, registered by name.
type Pass struct {
	Name string
	Run  func(ctx context.Context) int
}

// Consolidator runs the passes.
type Consolidator struct {
	Store     *Store
	DetectFor PlaybookDetect
	// Extra passes run after the built in ones, in order.
	Extra []Pass
}

// RunOnce runs every pass once and returns the per pass counters. startup also runs
// the one shot backfill. The counter "failed_passes" is added last, so "did anything
// happen" is read off the counters before the failure count joins them.
func (c *Consolidator) RunOnce(ctx context.Context, startup bool) map[string]int {
	s := c.Store
	stats := map[string]int{}
	if startup {
		stats["backfilled"] = s.BackfillFromRCAOutcomes(ctx)
	}
	stats["stale_edges_closed"] = s.CloseStaleEdges(ctx, staleEdgeHours)
	stats["detector_candidates"] = s.ProposeDetectorCandidates(ctx, c.DetectFor)
	if s.Cfg.PreferenceMemory {
		stats["prefs_inferred"] = s.InferFromBehaviour(ctx)
		stats["prefs_forgotten"] = s.DecayAndForget(ctx)
	}
	if s.Cfg.MemoryPromotion {
		stats["rules_promoted"] = s.PromoteFromEpisodes(ctx, DefaultActivateAt)
	}
	if s.Cfg.MemorySummaryTree {
		stats["summaries_built"] = s.BuildSummaryTree(ctx)
	}
	for _, p := range c.Extra {
		stats[p.Name] = p.Run(ctx)
	}

	didWork := false
	for _, v := range stats {
		didWork = didWork || v != 0
	}
	failures := s.Live.DrainPassFailures()
	stats["failed_passes"] = len(failures)
	if len(failures) > 0 {
		var parts []string
		for _, f := range failures {
			parts = append(parts, f[0]+": "+f[1])
		}
		b, _ := json.Marshal(stats)
		slog.Warn(fmt.Sprintf("consolidation_pass INCOMPLETE — %d of %d passes failed [%s] · counters %s", len(failures), len(stats)-1, strings.Join(parts, "; "), b))
	} else if didWork {
		b, _ := json.Marshal(stats)
		slog.Info("consolidation_pass " + string(b))
	}
	return stats
}

// Loop runs a pass every interval until ctx ends. It runs one at startup first.
func (c *Consolidator) Loop(ctx context.Context, interval time.Duration) {
	c.RunOnce(ctx, true)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.RunOnce(ctx, false)
		}
	}
}

// CloseStaleEdges closes graph edges whose pod entity has not been observed
// recently. The watch DELETED path normally closes them; this catches watcher
// downtime. It returns the edges closed.
func (s *Store) CloseStaleEdges(ctx context.Context, hours int) int {
	now := s.now()
	cutoff := now - float64(hours)*3600
	rows, err := s.DB.QueryContext(ctx, `SELECT id, attrs FROM kg_entities WHERE kind = 'Pod'`)
	if err != nil {
		slog.Warn("stale edge pass failed", "err", err)
		s.Live.RecordPassFailure("stale_edges_closed", err)
		return 0
	}
	var stale []string
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			s.Live.RecordPassFailure("stale_edges_closed", err)
			return 0
		}
		seen, _ := parseAttrs(raw)["last_seen"].(float64) // absent reads as 0, which is stale
		if seen < cutoff {
			stale = append(stale, id)
		}
	}
	rows.Close()

	closed := 0
	for _, id := range stale {
		res, err := s.DB.ExecContext(ctx, s.DB.Q(`UPDATE kg_edges SET valid_to = ? WHERE valid_to IS NULL AND valid_from < ? AND src = ?`), now, cutoff, id)
		if err != nil {
			slog.Warn("stale edge pass failed", "err", err)
			s.Live.RecordPassFailure("stale_edges_closed", err)
			return closed
		}
		n, _ := res.RowsAffected()
		closed += int(n)
	}
	return closed
}

// PlaybooksFromKey parses a structured pattern key, "playbook=A+B | ns=... |
// cluster=...", into its playbook names.
func PlaybooksFromKey(pattern string) []string {
	if !strings.HasPrefix(pattern, "playbook=") {
		return nil
	}
	head := strings.TrimSpace(strings.SplitN(pattern, "|", 2)[0])
	var out []string
	for _, p := range strings.Split(strings.TrimPrefix(head, "playbook="), "+") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ProposeDetectorCandidates turns verified, recurring patterns whose playbooks all
// carry compiled detect blocks into reviewable detector candidates. It is a
// deterministic derivation, and a human still has to review one before it reaches
// the watchtower. It returns the candidates created.
func (s *Store) ProposeDetectorCandidates(ctx context.Context, detectFor PlaybookDetect) int {
	if detectFor == nil {
		return 0
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT pattern_name, cluster_id FROM failure_patterns
		WHERE occurrence_count >= 2 AND confidence >= 0.9 AND demoted = FALSE`)
	if err != nil {
		slog.Warn("pattern fetch failed", "err", err)
		s.Live.RecordPassFailure("detector_candidates", err)
		return 0
	}
	type pat struct{ name, cluster string }
	var patterns []pat
	for rows.Next() {
		var p pat
		if err := rows.Scan(&p.name, &p.cluster); err != nil {
			rows.Close()
			s.Live.RecordPassFailure("detector_candidates", err)
			return 0
		}
		patterns = append(patterns, p)
	}
	rows.Close()
	sort.Slice(patterns, func(i, j int) bool { return patterns[i].name < patterns[j].name })

	created := 0
	for _, p := range patterns {
		var blocks []map[string]any
		for _, pb := range PlaybooksFromKey(p.name) {
			promql, debounce, ok := detectFor(pb)
			if !ok {
				blocks = nil // one playbook without a detector disqualifies the pattern
				break
			}
			if promql == nil {
				promql = []string{}
			}
			blocks = append(blocks, map[string]any{"playbook": pb, "promql": promql, "debounce_seconds": debounce})
		}
		if len(blocks) == 0 {
			continue
		}
		body, _ := json.Marshal(map[string]any{"derived_from_playbooks": blocks, "pattern": p.name})
		cluster := p.cluster
		if cluster == "" {
			cluster = "global"
		}
		res, err := s.DB.ExecContext(ctx, s.DB.Q(`INSERT INTO detectors (cluster_id, name, source, predicate, created_from, created_at)
			VALUES (?, ?, 'learned', ?, ?, ?) ON CONFLICT (cluster_id, name) DO NOTHING`),
			cluster, "learned:"+clip(p.name, 80), string(body), p.name, s.now())
		if err != nil {
			slog.Warn("candidate insert failed", "err", err)
			continue
		}
		if n, _ := res.RowsAffected(); n == 1 {
			created++
		}
	}
	return created
}
