package memory

import (
	"context"
	"reflect"
	"testing"
)

func TestSignalsAreDedupedAndTrimmed(t *testing.T) {
	got := SignalsFromInvestigation("c1", []string{"OOMKilled", " OOMKilled ", "", "CrashLoopBackOff"})
	if len(got) != 2 || got[0].Dst != "OOMKilled" || got[1].Dst != "CrashLoopBackOff" || got[0].Src != "c1" || got[0].Rel != "exhibits" || got[0].Verdict != "confirm" {
		t.Errorf("%+v", got)
	}
	if SignalsFromInvestigation("c1", nil) != nil {
		t.Error("nothing matched, no signals")
	}
}

func TestWritebackCreatesTheEdgeAndReinforcesIt(t *testing.T) {
	s := kgStore(t, nil)
	ctx := context.Background()
	sig := SignalsFromInvestigation("c1", []string{"OOMKilled"})
	if got := s.ApplyWriteback(ctx, "c1", sig); !reflect.DeepEqual(got, map[string]int{"ADD": 1}) {
		t.Errorf("%v", got)
	}
	// Reconcile is off, so a second signal is another ADD attempt that finds the open edge.
	s.ApplyWriteback(ctx, "c1", sig)
	edges := s.CurrentEdges(ctx, "c1", 10)
	if len(edges) != 1 || edges[0].Rel != "exhibits" || edges[0].Src != "Cluster//c1" || edges[0].Dst != "FailurePattern//OOMKilled" || edges[0].Attrs["x"] != nil {
		t.Fatalf("%+v", edges)
	}
}

func TestWritebackRecordsContradictionsAndSurvivesAFailedStore(t *testing.T) {
	s := kgStore(t, map[string]string{"MEMORY_WRITE_RECONCILE": "true"})
	ctx := context.Background()
	s.ApplyWriteback(ctx, "c1", SignalsFromInvestigation("c1", []string{"OOMKilled"}))
	got := s.ApplyWriteback(ctx, "c1", []EdgeSignal{{Src: "c1", Rel: "exhibits", Dst: "OOMKilled", Verdict: "contradict"}})
	if got["UPDATE"] != 1 {
		t.Errorf("a contradiction is recorded on the same edge: %v", got)
	}
	edges := s.CurrentEdges(ctx, "c1", 10)
	if len(edges) != 1 || edges[0].Attrs["investigation_confirmed"] != true || edges[0].Attrs["investigation_contradicted"] != true {
		t.Errorf("%+v", edges)
	}
	_, _ = s.DB.Exec(`DROP TABLE kg_edges`)
	_, _ = s.DB.Exec(`DROP TABLE kg_entities`)
	if got := s.ApplyWriteback(ctx, "c1", SignalsFromInvestigation("c1", []string{"X"})); got["ERROR"] != 1 {
		t.Errorf("must not raise: %v", got)
	}
}
