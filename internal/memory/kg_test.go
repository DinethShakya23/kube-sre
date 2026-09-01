package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/sensorium"
	"github.com/DinethShakya23/kube-sre/internal/store/storetest"
)

func kgStore(t *testing.T, env map[string]string) *Store {
	t.Helper()
	db := storetest.New(t, StoreMigrations, EpisodeMigrations, KGMigrations)
	s := NewStore(db, config.Load(func(k string) string { return env[k] }))
	now := t0
	s.Now = func() time.Time { return now }
	return s
}

func podObs(name, node, owner, watch string, at time.Time) sensorium.Observation {
	return sensorium.Observation{Kind: "pod_status", ClusterID: "c1", Namespace: "prod", Name: name, TS: at,
		Fields: map[string]any{"status": "Running", "node": node, "owner": owner, "watch_type": watch, "uid": "u-" + name, "resource_version": "7"}}
}

func TestUpsertEntityMergesAttrs(t *testing.T) {
	s := kgStore(t, nil)
	ctx := context.Background()
	a := s.UpsertEntity(ctx, "c1", "Pod", "web", "prod", map[string]any{"x": 1})
	b := s.UpsertEntity(ctx, "c1", "Pod", "web", "prod", map[string]any{"y": 2})
	if a == "" || a != b {
		t.Fatalf("ids %q %q", a, b)
	}
	var raw []byte
	_ = s.DB.QueryRow(s.DB.Q(`SELECT attrs FROM kg_entities WHERE id = ?`), a).Scan(&raw)
	got := parseAttrs(raw)
	if got["y"] != float64(2) || got["x"] != float64(1) {
		t.Errorf("attrs %v", got)
	}
}

func TestOpenEdgeIsIdempotentAndCloseStamps(t *testing.T) {
	s := kgStore(t, nil)
	ctx := context.Background()
	p := s.UpsertEntity(ctx, "c1", "Pod", "web", "prod", nil)
	n := s.UpsertEntity(ctx, "c1", "Node", "n1", "", nil)
	s.OpenEdge(ctx, "c1", p, "runs_on", n, EdgeOpts{})
	s.OpenEdge(ctx, "c1", p, "runs_on", n, EdgeOpts{})
	if got := len(s.CurrentEdges(ctx, "c1", 10)); got != 1 {
		t.Fatalf("open edges %d", got)
	}
	s.CloseEdge(ctx, "c1", p, "runs_on", "", 0)
	if got := len(s.CurrentEdges(ctx, "c1", 10)); got != 0 {
		t.Errorf("still open: %d", got)
	}
}

func TestPodMoveClosesOldRunsOn(t *testing.T) {
	s := kgStore(t, nil)
	ctx := context.Background()
	s.IngestPodObservation(ctx, podObs("web", "n1", "Deployment/web", "ADDED", t0))
	s.IngestPodObservation(ctx, podObs("web", "n2", "Deployment/web", "MODIFIED", t0))
	cur := s.CurrentEdges(ctx, "c1", 10)
	runs := 0
	for _, e := range cur {
		if e.Rel == "runs_on" {
			runs++
			if !strings.HasSuffix(e.Dst, "/n2") {
				t.Errorf("dst %s", e.Dst)
			}
		}
	}
	if runs != 1 || len(cur) != 2 { // runs_on + owns
		t.Errorf("edges %+v", cur)
	}
}

func TestDeletedPodClosesEverything(t *testing.T) {
	s := kgStore(t, nil)
	ctx := context.Background()
	s.IngestPodObservation(ctx, podObs("web", "n1", "Deployment/web", "ADDED", t0))
	s.IngestPodObservation(ctx, podObs("web", "n1", "Deployment/web", "DELETED", t0))
	if got := s.CurrentEdges(ctx, "c1", 10); len(got) != 0 {
		t.Errorf("%+v", got)
	}
}

func TestObservationRef(t *testing.T) {
	o := podObs("web", "n1", "", "ADDED", t0)
	if got := ObservationRef(o); got != "pod_status:u-web@7" {
		t.Errorf("%q", got)
	}
	delete(o.Fields, "resource_version")
	if got := ObservationRef(o); got != "pod_status:u-web" {
		t.Errorf("%q", got)
	}
	delete(o.Fields, "uid")
	if ObservationRef(o) != "" {
		t.Error("no uid should give no ref")
	}
}

func TestChangesWindowAndBlock(t *testing.T) {
	s := kgStore(t, nil)
	ctx := context.Background()
	s.IngestPodObservation(ctx, podObs("web", "n1", "", "ADDED", t0))
	s.Now = func() time.Time { return t0.Add(time.Minute) }
	s.IngestPodObservation(ctx, podObs("web", "n2", "", "MODIFIED", t0))
	s.Now = func() time.Time { return t0.Add(2 * time.Minute) }

	ch, err := s.Changes(ctx, "c1", float64(t0.Unix())-1, float64(t0.Add(2*time.Minute).Unix()))
	if err != nil || len(ch) != 3 { // open, close, open
		t.Fatalf("%v %+v", err, ch)
	}
	block, err := s.RecentChangesBlock(ctx, "c1", 15, 12)
	if err != nil || !strings.Contains(block, "closed Pod/prod/web -runs_on-> Node//n1") {
		t.Errorf("%v\n%s", err, block)
	}
	capped, _ := s.RecentChangesBlock(ctx, "c1", 15, 1)
	if !strings.Contains(capped, "(+2 more)") {
		t.Errorf("cap: %s", capped)
	}
	if empty, err := s.RecentChangesBlock(ctx, "other", 15, 12); err != nil || empty != "" {
		t.Errorf("calm cluster: %q %v", empty, err)
	}
}

func TestChangesReadFailureIsNotCalm(t *testing.T) {
	s := kgStore(t, nil)
	if _, err := s.DB.Exec(`DROP TABLE kg_edges`); err != nil {
		t.Fatal(err)
	}
	block, err := s.RecentChangesBlock(context.Background(), "c1", 15, 12)
	if !errors.Is(err, ErrKGUnavailable) || block != "" {
		t.Errorf("%q %v", block, err)
	}
}

func TestAsOfHonoursRetraction(t *testing.T) {
	s := kgStore(t, nil)
	ctx := context.Background()
	p := s.UpsertEntity(ctx, "c1", "Pod", "web", "prod", nil)
	n := s.UpsertEntity(ctx, "c1", "Node", "n1", "", nil)
	s.OpenEdge(ctx, "c1", p, "runs_on", n, EdgeOpts{})
	before := float64(t0.Unix())
	s.Now = func() time.Time { return t0.Add(time.Hour) }
	if got := s.RetractEdge(ctx, "c1", p, "runs_on", ""); got != 1 {
		t.Fatalf("retracted %d", got)
	}
	if got := s.AsOf(ctx, "c1", before, before+1); len(got) != 1 {
		t.Errorf("believed then: %+v", got)
	}
	if got := s.AsOf(ctx, "c1", before, 0); len(got) != 0 {
		t.Errorf("believed now: %+v", got)
	}
}

func TestBitemporalUsesEventTime(t *testing.T) {
	ev := float64(t0.Unix()) - 300
	for _, on := range []bool{false, true} {
		env := map[string]string{}
		if on {
			env["MEMORY_BITEMPORAL_ENABLED"] = "true"
		}
		s := kgStore(t, env)
		ctx := context.Background()
		p := s.UpsertEntity(ctx, "c1", "Pod", "web", "prod", nil)
		n := s.UpsertEntity(ctx, "c1", "Node", "n1", "", nil)
		s.OpenEdge(ctx, "c1", p, "runs_on", n, EdgeOpts{EventTime: ev})
		lag := s.MeanIngestLagSeconds(ctx, "c1", 60)
		if !on {
			if lag != nil {
				t.Error("lag reported with the flag off")
			}
			continue
		}
		if lag == nil || *lag < 299 || *lag > 301 {
			t.Errorf("lag %v", lag)
		}
	}
}

func TestReconcile(t *testing.T) {
	ctx := context.Background()
	off := kgStore(t, nil)
	p := off.UpsertEntity(ctx, "c1", "Pod", "web", "prod", nil)
	n1 := off.UpsertEntity(ctx, "c1", "Node", "n1", "", nil)
	n2 := off.UpsertEntity(ctx, "c1", "Node", "n2", "", nil)
	if got := off.ReconcileEdge(ctx, "c1", p, "runs_on", n1, EdgeOpts{}, nil); got != "ADD" {
		t.Errorf("flag off: %s", got)
	}

	s := kgStore(t, map[string]string{"MEMORY_WRITE_RECONCILE": "true"})
	p = s.UpsertEntity(ctx, "c1", "Pod", "web", "prod", nil)
	n1 = s.UpsertEntity(ctx, "c1", "Node", "n1", "", nil)
	n2 = s.UpsertEntity(ctx, "c1", "Node", "n2", "", nil)
	low := 0.1
	if got := s.ReconcileEdge(ctx, "c1", p, "runs_on", n1, EdgeOpts{}, &low); got != "NOOP" {
		t.Errorf("low salience: %s", got)
	}
	if got := s.ReconcileEdge(ctx, "c1", p, "runs_on", n1, EdgeOpts{}, nil); got != "ADD" {
		t.Errorf("add: %s", got)
	}
	if got := s.ReconcileEdge(ctx, "c1", p, "runs_on", n1, EdgeOpts{}, nil); got != "NOOP" {
		t.Errorf("repeat: %s", got)
	}
	if got := s.ReconcileEdge(ctx, "c1", p, "runs_on", n1, EdgeOpts{Attrs: map[string]any{"k": "v"}}, nil); got != "UPDATE" {
		t.Errorf("update: %s", got)
	}
	high := 0.9
	if got := s.ReconcileEdge(ctx, "c1", p, "runs_on", n2, EdgeOpts{}, &high); got != "RETRACT" {
		t.Errorf("supersede: %s", got)
	}
	cur := s.CurrentEdges(ctx, "c1", 10)
	if len(cur) != 1 || !strings.HasSuffix(cur[0].Dst, "/n2") {
		t.Errorf("after supersede: %+v", cur)
	}
}

func TestLinkIncidentAndPPR(t *testing.T) {
	ctx := context.Background()
	off := kgStore(t, nil)
	if got := off.PPRBlastRadius(ctx, "c1", []string{"x"}, 2, 5, 100); got != nil {
		t.Error("flag off should return nothing")
	}

	s := kgStore(t, map[string]string{"MEMORY_KG_PPR": "true"})
	s.IngestPodObservation(ctx, podObs("web-1", "n1", "Deployment/web", "ADDED", t0))
	s.IngestPodObservation(ctx, podObs("web-2", "n1", "Deployment/web", "ADDED", t0))
	s.IngestPodObservation(ctx, podObs("db-1", "n9", "StatefulSet/db", "ADDED", t0))
	s.LinkIncident(ctx, "c1", "prod", "web-1", "inc-1", "", "ep-1")

	node := s.UpsertEntity(ctx, "c1", "Node", "n1", "", nil)
	rel := s.PPRBlastRadius(ctx, "c1", []string{node}, 3, 10, 500)
	if len(rel) == 0 {
		t.Fatal("no blast radius")
	}
	names := map[string]bool{}
	for _, r := range rel {
		names[r.Entity] = true
	}
	if !names["Pod/prod/web-1"] || !names["Pod/prod/web-2"] {
		t.Errorf("missing pods on the node: %+v", rel)
	}
	if names["Pod/prod/db-1"] {
		t.Errorf("unrelated pod leaked: %+v", rel)
	}
	if rel[0].Entity != "Pod/prod/web-1" && rel[0].Entity != "Pod/prod/web-2" && rel[0].Entity != "Incident//inc-1" && !strings.HasPrefix(rel[0].Entity, "Deployment") {
		t.Errorf("unexpected top: %+v", rel[0])
	}
	for i := 1; i < len(rel); i++ {
		if rel[i].Score > rel[i-1].Score {
			t.Error("not sorted")
		}
	}
}

func TestPPRScoresFavourNearNodes(t *testing.T) {
	edges := [][2]string{{"a", "b"}, {"b", "c"}, {"c", "d"}}
	sc := pprScores(edges, map[string]bool{"a": true})
	if !(sc["b"] > sc["c"] && sc["c"] > sc["d"]) {
		t.Errorf("%v", sc)
	}
}
