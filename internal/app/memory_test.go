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
