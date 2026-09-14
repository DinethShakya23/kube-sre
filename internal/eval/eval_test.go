package eval

import (
	"strings"
	"testing"
)

func TestSecurityOutcome(t *testing.T) {
	o := ScoreSecurityOutcome([]string{"a", "b", "c"}, []string{"b", "d"})
	if strings.Join(o.Introduced, ",") != "d" || strings.Join(o.Resolved, ",") != "a,c" || o.NetDelta() != -1 || o.Clean() {
		t.Errorf("%+v", o)
	}
	if GatesPromotion(o, false) || GatesPromotion(o, true) {
		t.Error("any introduced violation is a hard blocker, even alongside a net improvement")
	}
	good := ScoreSecurityOutcome([]string{"a", "b"}, []string{"b"})
	if !good.Clean() || !GatesPromotion(good, false) || !GatesPromotion(good, true) {
		t.Errorf("%+v", good)
	}
	same := ScoreSecurityOutcome([]string{"a"}, []string{"a"})
	if !GatesPromotion(same, false) || GatesPromotion(same, true) || same.NetDelta() != 0 {
		t.Error("no change passes the basic gate but not the net improvement gate")
	}
	agg := Aggregate([]SecurityOutcome{good, ScoreSecurityOutcome(nil, []string{"z"}), ScoreSecurityOutcome([]string{"q"}, nil)})
	if strings.Join(agg.Introduced, ",") != "z" || strings.Join(agg.Resolved, ",") != "a,q" {
		t.Errorf("%+v", agg)
	}
	if e := Aggregate(nil); e.Introduced == nil || e.Resolved == nil || !e.Clean() {
		t.Errorf("%+v", e)
	}
}

func TestManifestHashIsContentAddressed(t *testing.T) {
	a := SnapshotManifestHash(map[string]string{"env": "sha256:1", "model": "sha256:2"})
	if a != SnapshotManifestHash(map[string]string{"model": "sha256:2", "env": "sha256:1"}) || len(a) != 64 {
		t.Error("key order must not matter")
	}
	if a == SnapshotManifestHash(map[string]string{"env": "sha256:1", "model": "sha256:3"}) {
		t.Error("a changed layer must change the hash")
	}
}

func TestAGradeWithoutItsHarnessIsNotShippable(t *testing.T) {
	h := HarnessSpec{Model: "m", MaxGatherRounds: 3, ToolSurface: "aci-v0", MemoryFlags: []string{"MEMORY_PROMOTION"}, ReplayFidelity: "full", Seed: 7}
	g, err := BuildGrade(ScoreCard{0.8, 0.6, 1200}, h, map[string]string{"env": "x"}, nil)
	if err != nil || g.Notes == nil || len(g.ManifestHash) != 64 {
		t.Fatalf("%+v %v", g, err)
	}
	for _, bad := range []HarnessSpec{{ToolSurface: "x", ReplayFidelity: "full"}, {Model: "m", ReplayFidelity: "full"}, {Model: "m", ToolSurface: "x"}} {
		if _, err := BuildGrade(ScoreCard{}, bad, nil, nil); err == nil {
			t.Errorf("%+v must be refused", bad)
		}
	}
}

func TestSeparabilityGate(t *testing.T) {
	// harness variance 0.01 gives sigma 0.1, and the bar is 1.645 * 0.1.
	if r := Decompose(0.2, 0.01); !r.Separable || r.Verdict != "proven" || !strings.Contains(r.Detail, "0.2000 > z·σ_harness=0.1645") {
		t.Errorf("%+v", r)
	}
	if r := Decompose(-0.1, 0.01); r.Separable || r.Verdict != "unproven" || !strings.Contains(r.Detail, "<=") {
		t.Errorf("a claimed effect inside the noise is unproven: %+v", r)
	}
	if r := Decompose(0.001, 0); !r.Separable {
		t.Error("no harness variance: any effect is separable")
	}
	if r := Decompose(0.5, -1); r.HarnessStd != 0 {
		t.Error("negative variance is clamped")
	}
}
