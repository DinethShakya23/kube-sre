package app

import (
	"context"
	"strings"
	"testing"

	"github.com/DinethShakya23/kube-sre/internal/agent"
	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/memory"
	"github.com/DinethShakya23/kube-sre/internal/sensorium"
	"github.com/DinethShakya23/kube-sre/internal/store/storetest"
)

func adapter(t *testing.T) *memoryAdapter {
	t.Helper()
	db := storetest.New(t, memory.StoreMigrations, memory.EpisodeMigrations, memory.KGMigrations)
	return newMemoryAdapter(memory.NewStore(db, config.Load(func(string) string { return "" })))
}

func TestRecordThenLoadRecallsTheEpisode(t *testing.T) {
	m := adapter(t)
	ctx := context.Background()
	ok := true
	m.Record(ctx, agent.Outcome{
		SessionID: "s1", UserID: "u", ClusterID: "c1", Namespace: "prod", RootCause: "payments pod OOMKilled, limit too low",
		Confidence: 0.95, RecommendedFix: "kubectl set resources deploy/payments --limits=memory=512Mi", Verified: &ok,
		Summary: "payments pods restarting with OOMKilled", TriggerSource: "user",
	})
	got := m.Load(ctx, agent.LoadRequest{UserID: "u", SessionID: "s2", ClusterID: "c1", Query: "payments pods OOMKilled again"})
	if !strings.Contains(got, "Similar past episodes") || !strings.Contains(got, "OOMKilled") {
		t.Errorf("recall missing:\n%s", got)
	}
}

func TestLoadSaysWhenReadsFail(t *testing.T) {
	m := adapter(t)
	for _, tbl := range []string{"episodes", "kg_edges"} {
		if _, err := m.store.DB.Exec("DROP TABLE " + tbl); err != nil {
			t.Fatal(err)
		}
	}
	got := m.Load(context.Background(), agent.LoadRequest{UserID: "u", ClusterID: "c1", Query: "anything at all"})
	if !strings.Contains(got, "Similar past episodes unavailable") || !strings.Contains(got, "Recent cluster changes unavailable") {
		t.Errorf("failures not announced:\n%s", got)
	}
}

func TestLoadIsQuietWhenNothingHappened(t *testing.T) {
	m := adapter(t)
	if got := m.Load(context.Background(), agent.LoadRequest{UserID: "u", ClusterID: "c1", Query: "hello"}); got != "" {
		t.Errorf("expected empty, got:\n%s", got)
	}
}

func TestGraphFeedShedsAndCounts(t *testing.T) {
	m := adapter(t)
	g := newGraphFeed(m.store, 1)
	pod := sensorium.Observation{Kind: "pod_status", ClusterID: "c1", Name: "a"}
	g.Offer(pod)
	g.Offer(pod)
	g.Offer(sensorium.Observation{Kind: "event"})
	if g.Dropped() != 1 || len(g.queue) != 1 {
		t.Errorf("dropped %d queued %d", g.Dropped(), len(g.queue))
	}
}

func TestLoadInjectsRulesAndThemesWhenEnabledAndSaysWhenTheyFail(t *testing.T) {
	db := storetest.New(t, memory.StoreMigrations, memory.EpisodeMigrations, memory.KGMigrations, memory.RuleMigrations)
	cfg := config.Load(func(k string) string {
		return map[string]string{"MEMORY_PROMOTION": "true", "MEMORY_SUMMARY_TREE": "true"}[k]
	})
	store := memory.NewStore(db, cfg)
	m := newMemoryAdapter(store)
	ctx := context.Background()
	store.RecordRule(ctx, "c1", "OOMKilled", "raise the memory limit", "episode", 0.5, 1)
	store.RecordRule(ctx, "c1", "OOMKilled", "raise the memory limit", "episode", 0.5, 1)
	ok := true
	for i := 0; i < 3; i++ {
		store.WriteEpisode(ctx, memory.EpisodeInput{ClusterID: "c1", TriggerKind: "detector", Summary: "OOMKilled again in payments", RootCause: "limit",
			Playbooks: []string{"OOMKilled"}, Verified: &ok, Outcome: "resolved", StartedAt: float64(1000 + i)})
	}
	store.BuildSummaryTree(ctx)

	got := m.Load(ctx, agent.LoadRequest{UserID: "u", ClusterID: "c1", Query: "OOMKilled payments"})
	if !strings.Contains(got, "## Learned rules (this cluster)") || !strings.Contains(got, "IF OOMKilled THEN raise the memory limit") ||
		!strings.Contains(got, "## Memory themes (this cluster)") {
		t.Errorf("%s", got)
	}

	_, _ = db.Exec(`DROP TABLE semantic_rules`)
	_, _ = db.Exec(`DROP TABLE memory_summaries`)
	got = m.Load(ctx, agent.LoadRequest{UserID: "u", ClusterID: "c1", Query: "OOMKilled payments"})
	if !strings.Contains(got, "Learned rules unavailable") || !strings.Contains(got, "Memory themes unavailable") {
		t.Errorf("failures must be announced:\n%s", got)
	}
}
