package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/detectstore"
	"github.com/DinethShakya23/kube-sre/internal/store/storetest"
)

func roll(t *testing.T, env map[string]string) *Store {
	t.Helper()
	db := storetest.New(t, StoreMigrations, EpisodeMigrations, KGMigrations, RuleMigrations, detectstore.Migrations)
	s := NewStore(db, config.Load(func(k string) string { return env[k] }))
	s.Now = func() time.Time { return t0 }
	return s
}

func verifiedEp(t *testing.T, s *Store, playbook, root string, ago float64) {
	t.Helper()
	ok := true
	write(t, s, "summary of "+playbook+" "+root, root, ago, func(in *EpisodeInput) {
		in.Verified, in.Playbooks, in.Outcome = &ok, []string{playbook}, "resolved"
	})
}

func TestARuleActivatesOnItsSecondSighting(t *testing.T) {
	s := roll(t, nil)
	ctx := context.Background()
	if !s.RecordRule(ctx, "c1", "CrashLoopBackOff", "raise the memory limit", "episode", 0.5, 2) {
		t.Fatal("not stored")
	}
	if got, _ := s.ActiveRules(ctx, "c1", 10); len(got) != 0 {
		t.Errorf("one sighting is a candidate: %+v", got)
	}
	s.RecordRule(ctx, "c1", "CrashLoopBackOff", "raise the memory limit to 1Gi", "episode", 0.5, 2)
	got, err := s.ActiveRules(ctx, "c1", 10)
	if err != nil || len(got) != 1 || got[0].Recurrence != 2 || got[0].Confidence != 0.6 || got[0].Guidance != "raise the memory limit to 1Gi" {
		t.Fatalf("%+v %v", got, err)
	}
	block := RenderRulesBlock(got)
	if !strings.Contains(block, "IF CrashLoopBackOff THEN raise the memory limit to 1Gi (seen ×2)") {
		t.Errorf("%s", block)
	}
	if RenderRulesBlock(nil) != "" {
		t.Error("no rules, no block")
	}
}

func TestADemotedRuleStaysDemoted(t *testing.T) {
	s := roll(t, nil)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		s.RecordRule(ctx, "c1", "ctx", "do x", "episode", 0.5, 2)
	}
	if !s.DemoteRule(ctx, "c1", "ctx") {
		t.Fatal("demote")
	}
	for i := 0; i < 3; i++ {
		s.RecordRule(ctx, "c1", "ctx", "do x", "episode", 0.5, 2)
	}
	if got, _ := s.ActiveRules(ctx, "c1", 10); len(got) != 0 {
		t.Errorf("recurrence overruled a human: %+v", got)
	}
	if s.RecordRule(ctx, "c1", " ", "x", "episode", 0.5, 2) || s.RecordRule(ctx, "c1", "x", " ", "episode", 0.5, 2) {
		t.Error("empty context or guidance")
	}
	if s.DemoteRule(ctx, "c1", "nope") {
		t.Error("unknown rule")
	}
}

func TestRulesAreScopedToTheirCluster(t *testing.T) {
	s := roll(t, nil)
	ctx := context.Background()
	s.RecordRule(ctx, "c1", "ctx", "do x", "episode", 0.9, 1)
	s.RecordRule(ctx, "c1", "ctx", "do x", "episode", 0.9, 1)
	if got, _ := s.ActiveRules(ctx, "c2", 10); len(got) != 0 {
		t.Errorf("%+v", got)
	}
}

func TestPromotionNeedsVerifiedRecurrenceAndTheFlag(t *testing.T) {
	ctx := context.Background()
	off := roll(t, nil)
	verifiedEp(t, off, "OOMKilled", "limit too low", 3)
	verifiedEp(t, off, "OOMKilled", "limit too low", 2)
	if n := off.PromoteFromEpisodes(ctx, 2); n != 0 {
		t.Errorf("flag off: %d", n)
	}

	s := roll(t, map[string]string{"MEMORY_PROMOTION": "true"})
	verifiedEp(t, s, "OOMKilled", "old cause", 5)
	verifiedEp(t, s, "OOMKilled", "the latest cause", 1)
	verifiedEp(t, s, "ImagePullBackOff", "bad tag", 2) // only once
	write(t, s, "unverified thing", "x", 1, func(in *EpisodeInput) { in.Playbooks = []string{"Pending"} })
	write(t, s, "unverified thing again", "x", 1, func(in *EpisodeInput) { in.Playbooks = []string{"Pending"} })

	if n := s.PromoteFromEpisodes(ctx, 2); n != 1 {
		t.Fatalf("promoted %d", n)
	}
	// The first pass writes a candidate; the pass after it is the second sighting.
	if got, _ := s.ActiveRules(ctx, "c1", 10); len(got) != 0 {
		t.Errorf("a new rule is a candidate: %+v", got)
	}
	s.PromoteFromEpisodes(ctx, 2)
	got, _ := s.ActiveRules(ctx, "c1", 10)
	if len(got) != 1 || got[0].Context != "OOMKilled" || got[0].Guidance != "the latest cause" {
		t.Errorf("%+v", got)
	}
}

func TestSummariesAreRebuiltOnlyWhenTheThemeChanged(t *testing.T) {
	ctx := context.Background()
	if roll(t, nil).BuildSummaryTree(ctx) != 0 {
		t.Error("flag off")
	}
	s := roll(t, map[string]string{"MEMORY_SUMMARY_TREE": "true"})
	verifiedEp(t, s, "OOMKilled", "cause one", 4)
	verifiedEp(t, s, "OOMKilled", "cause two", 3)
	if n := s.BuildSummaryTree(ctx); n != 0 {
		t.Errorf("two episodes are below the minimum of three: %d", n)
	}
	verifiedEp(t, s, "OOMKilled", "cause three", 2)
	write(t, s, "a namespace themed one", "", 1, func(in *EpisodeInput) { in.Namespace = "shop" })

	if n := s.BuildSummaryTree(ctx); n != 1 {
		t.Fatalf("built %d", n)
	}
	if n := s.BuildSummaryTree(ctx); n != 0 {
		t.Errorf("a quiet theme must not be rebuilt: %d", n)
	}
	// New episode: the theme moved.
	verifiedEp(t, s, "OOMKilled", "cause four", 0.5)
	if n := s.BuildSummaryTree(ctx); n != 1 {
		t.Errorf("new episodes rebuild the theme: %d", n)
	}
	// The graph moved: the watermark changes even with no new episode.
	p := s.UpsertEntity(ctx, "c1", "Pod", "web", "prod", nil)
	n := s.UpsertEntity(ctx, "c1", "Node", "n1", "", nil)
	s.OpenEdge(ctx, "c1", p, "runs_on", n, EdgeOpts{})
	if got := s.BuildSummaryTree(ctx); got != 1 {
		t.Errorf("a graph change rebuilds it: %d", got)
	}

	sums, err := s.RecallThemeSummaries(ctx, "what keeps failing with OOMKilled", "c1", 3)
	if err != nil || len(sums) != 1 || sums[0].Members != 4 || sums[0].Verified != 4 {
		t.Fatalf("%+v %v", sums, err)
	}
	if !strings.Contains(sums[0].Summary, "Theme 'OOMKilled': 4 episodes (4 verified).") || !strings.Contains(sums[0].Summary, "Outcomes: resolved.") ||
		!strings.Contains(sums[0].Summary, "cause four | cause three | cause two") {
		t.Errorf("%s", sums[0].Summary)
	}
	if b := RenderSummariesBlock(sums); !strings.HasPrefix(b, "## Memory themes (this cluster)\n- Theme 'OOMKilled'") {
		t.Errorf("%s", b)
	}
	if got, _ := s.RecallThemeSummaries(ctx, "zzz qqq xxx jjj", "c1", 3); len(got) != 0 {
		t.Errorf("an unrelated query should match nothing: %+v", got)
	}
	if got, _ := s.RecallThemeSummaries(ctx, "OOMKilled", "c2", 3); len(got) != 0 {
		t.Errorf("another cluster: %+v", got)
	}
}

func TestSummaryReadFailureIsNotAnEmptyList(t *testing.T) {
	s := roll(t, nil)
	_, _ = s.DB.Exec(`DROP TABLE memory_summaries`)
	if _, err := s.RecallThemeSummaries(context.Background(), "x y z", "c1", 3); !errors.Is(err, ErrSummariesUnavailable) {
		t.Errorf("%v", err)
	}
	_, _ = s.DB.Exec(`DROP TABLE semantic_rules`)
	if _, err := s.ActiveRules(context.Background(), "c1", 3); !errors.Is(err, ErrRulesUnavailable) {
		t.Errorf("%v", err)
	}
}

func TestStaleEdgesAreClosedForPodsNotSeenRecently(t *testing.T) {
	s := roll(t, nil)
	ctx := context.Background()
	stale := s.UpsertEntity(ctx, "c1", "Pod", "gone", "prod", map[string]any{"last_seen": float64(t0.Unix()) - 48*3600})
	fresh := s.UpsertEntity(ctx, "c1", "Pod", "alive", "prod", map[string]any{"last_seen": float64(t0.Unix()) - 60})
	never := s.UpsertEntity(ctx, "c1", "Pod", "nolastseen", "prod", nil)
	node := s.UpsertEntity(ctx, "c1", "Node", "n1", "", map[string]any{"last_seen": 0})
	old := s
	old.Now = func() time.Time { return t0.Add(-72 * time.Hour) }
	for _, p := range []string{stale, fresh, never, node} {
		old.OpenEdge(ctx, "c1", p, "runs_on", node, EdgeOpts{})
	}
	s.Now = func() time.Time { return t0 }
	if n := s.CloseStaleEdges(ctx, 24); n != 2 {
		t.Fatalf("closed %d (the stale pod and the one never seen)", n)
	}
	open := s.CurrentEdges(ctx, "c1", 10)
	if len(open) != 2 {
		t.Errorf("%+v", open)
	}
	if n := s.CloseStaleEdges(ctx, 24); n != 0 {
		t.Errorf("idempotent: %d", n)
	}
}

func TestPlaybooksFromKey(t *testing.T) {
	cases := map[string][]string{
		"playbook=A+B | ns=shop | cluster=c1": {"A", "B"},
		"playbook=OOMKilled":                  {"OOMKilled"},
		"free text pattern":                   nil,
		"playbook=":                           nil,
		"playbook=A++B":                       {"A", "B"},
	}
	for in, want := range cases {
		got := PlaybooksFromKey(in)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%q: %v", in, got)
		}
	}
}

func seedPattern(t *testing.T, s *Store, name string, times int) {
	t.Helper()
	ok := true
	for i := 0; i < times; i++ {
		t0 := t0.Add(time.Duration(i) * 25 * time.Hour) // clear the cooldown between sightings
		s.Now = func() time.Time { return t0 }
		s.RecordOutcome(context.Background(), RCAOutcome{SessionID: "s", UserID: "u", RootCause: name, Confidence: 0.95, RecommendedFix: "fix",
			ClusterID: "c1", Verified: &ok})
	}
	s.Now = func() time.Time { return t0 }
}

func TestDetectorCandidatesFromRecurringPatterns(t *testing.T) {
	s := roll(t, nil)
	ctx := context.Background()
	seedPattern(t, s, "playbook=CrashLoopBackOff+OOMKilled | ns=shop", 2)
	seedPattern(t, s, "playbook=NoDetectorHere", 2)
	seedPattern(t, s, "playbook=OneTime", 1)
	seedPattern(t, s, "not a structured key", 2)

	detectFor := func(name string) ([]string, int, bool) {
		switch name {
		case "CrashLoopBackOff":
			return []string{"up"}, 30, true
		case "OOMKilled":
			return nil, 0, true
		}
		return nil, 0, false
	}
	if n := s.ProposeDetectorCandidates(ctx, detectFor); n != 1 {
		t.Fatalf("created %d", n)
	}
	if n := s.ProposeDetectorCandidates(ctx, detectFor); n != 0 {
		t.Errorf("idempotent: %d", n)
	}
	var name, status, pred string
	if err := s.DB.QueryRow(`SELECT name, status, predicate FROM detectors`).Scan(&name, &status, &pred); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, "learned:playbook=CrashLoopBackOff+OOMKilled") || status != "candidate" || !strings.Contains(strings.ReplaceAll(pred, " ", ""), `"debounce_seconds":30`) {
		t.Errorf("%s %s %s", name, status, pred)
	}
	if n := s.ProposeDetectorCandidates(ctx, nil); n != 0 {
		t.Errorf("no playbook lookup: %d", n)
	}
}

func TestConsolidationRunsEveryPassAndSaysWhenOneFailed(t *testing.T) {
	s := roll(t, map[string]string{"MEMORY_PROMOTION": "true", "MEMORY_SUMMARY_TREE": "true"})
	ctx := context.Background()
	c := &Consolidator{Store: s}
	extraRuns := 0
	c.Extra = []Pass{{Name: "custom", Run: func(context.Context) int { extraRuns++; return 3 }}}

	stats := c.RunOnce(ctx, true)
	for _, k := range []string{"backfilled", "stale_edges_closed", "detector_candidates", "prefs_inferred", "prefs_forgotten", "rules_promoted", "summaries_built", "custom", "failed_passes"} {
		if _, ok := stats[k]; !ok {
			t.Errorf("missing counter %s in %v", k, stats)
		}
	}
	if stats["failed_passes"] != 0 || extraRuns != 1 {
		t.Errorf("%v", stats)
	}
	if _, ok := c.RunOnce(ctx, false)["backfilled"]; ok {
		t.Error("the backfill is startup only")
	}

	// A healthy idle pool and a dead one must not return the same answer.
	_, _ = s.DB.Exec(`DROP TABLE episodes`)
	_, _ = s.DB.Exec(`DROP TABLE kg_entities`)
	stats = c.RunOnce(ctx, false)
	if stats["failed_passes"] < 2 {
		t.Errorf("failures must be visible in the result: %v", stats)
	}
	if stats["custom"] != 3 || extraRuns != 3 {
		t.Errorf("one failing pass must not stop the rest: %v", stats)
	}
	if again := c.RunOnce(ctx, false); again["failed_passes"] < 2 {
		t.Errorf("still broken, still reported: %v", again)
	}
}
