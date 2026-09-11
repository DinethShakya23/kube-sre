package memory

import (
	"context"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/sensorium"
	"github.com/DinethShakya23/kube-sre/internal/store/storetest"
)

func svcRig(t *testing.T, env map[string]string) (*Service, *Store) {
	t.Helper()
	db := storetest.New(t, StoreMigrations, EpisodeMigrations, KGMigrations, GuardMigrations)
	s := NewStore(db, config.Load(func(k string) string { return env[k] }))
	v := NewService(s)
	v.Sleep = func(time.Duration) {}
	return v, s
}

func podO(name string) sensorium.Observation {
	return sensorium.Observation{Kind: "pod_status", ClusterID: "c1", Namespace: "prod", Name: name, TS: time.Now(),
		Fields: map[string]any{"status": "Running", "node": "n1"}}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestQueueShedsAndCountsAndIgnoresOtherKinds(t *testing.T) {
	v, _ := svcRig(t, map[string]string{"MEMORY_OBS_QUEUE_MAXSIZE": "1"})
	v.Enqueue(podO("a"))
	v.Enqueue(podO("b"))
	v.Enqueue(sensorium.Observation{Kind: "event"})
	st := v.Status()
	if st["observations_dropped"] != int64(1) || st["observation_backlog"] != 1 || st["healthy"] != false {
		t.Errorf("%v", st)
	}
}

func TestDrainWritesTheGraph(t *testing.T) {
	v, s := svcRig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go v.Drain(ctx)
	v.Enqueue(podO("web"))
	waitFor(t, "an edge", func() bool { return len(s.CurrentEdges(ctx, "c1", 5)) == 1 })
	if st := v.Status(); st["state"] != "ready" || st["healthy"] != true || st["enabled"] != true {
		t.Errorf("%v", st)
	}
}

func TestPersistentIngestFailureDegradesThenRecovers(t *testing.T) {
	v, s := svcRig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go v.Drain(ctx)
	_, _ = s.DB.Exec(`ALTER TABLE kg_entities RENAME TO kg_entities_gone`)
	for i := 0; i < 4; i++ {
		v.Enqueue(podO("x"))
	}
	waitFor(t, "degraded", func() bool { return v.Status()["state"] == "degraded" })
	if st := v.Status(); st["healthy"] != false || st["enabled"] != false || st["reason"] == "" {
		t.Errorf("%v", st)
	}
	_, _ = s.DB.Exec(`ALTER TABLE kg_entities_gone RENAME TO kg_entities`)
	v.Enqueue(podO("y"))
	waitFor(t, "recovery", func() bool { return v.Status()["state"] == "ready" })
	if v.Status()["ingest_failures"] != int64(0) {
		t.Errorf("%v", v.Status())
	}
}

func TestHierarchyOffCountsEverythingAsDropped(t *testing.T) {
	v, _ := svcRig(t, map[string]string{"MEMORY_HIERARCHY_ENABLED": "false"})
	if v.Active() {
		t.Error("no queue")
	}
	v.Enqueue(podO("a"))
	v.Enqueue(podO("b"))
	st := v.Status()
	if st["state"] != "flag" || st["observations_dropped"] != int64(2) || st["enabled"] != false {
		t.Errorf("an empty graph must be explainable: %v", st)
	}
	v.Drain(context.Background()) // returns at once
}

func TestChainVerifierRecordsAndReportsStaleness(t *testing.T) {
	v, s := svcRig(t, map[string]string{"MEMORY_SECURITY_HARDENING": "true", "MEMORY_CHAIN_VERIFY_INTERVAL_S": "100"})
	g := NewGuard(s.DB, 5, 0.35)
	v.Guard, s.Guard = g, g
	ctx := context.Background()
	if v.Status()["chain"].(map[string]any)["state"] != "never-checked" {
		t.Errorf("%v", v.Status()["chain"])
	}
	g.Audit(ctx, "unknown", "episode_write", "", map[string]any{"a": 1})
	v.VerifyChainOnce(ctx)
	if c := v.Status()["chain"].(map[string]any); c["state"] != "intact" {
		t.Errorf("%v", c)
	}
	_, _ = s.DB.Exec(`UPDATE memory_audit SET payload = '{"a":2}'`)
	v.VerifyChainOnce(ctx)
	st := v.Status()
	if st["chain"].(map[string]any)["state"] != "TAMPERED" || st["healthy"] != false || len(st["symptoms"].([]string)) == 0 {
		t.Errorf("%v", st)
	}
}

func TestWithoutAGuardTheVerifierDoesNothing(t *testing.T) {
	v, _ := svcRig(t, nil)
	v.VerifyChainOnce(context.Background())
	if v.Status()["chain"].(map[string]any)["state"] != "off" {
		t.Errorf("%v", v.Status()["chain"])
	}
}
