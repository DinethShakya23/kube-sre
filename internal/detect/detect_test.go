package detect

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/sensorium"
)

var t0 = time.Unix(1_800_000_000, 0)

func pod(ns, name, status string, at time.Time, watch string) sensorium.Observation {
	return sensorium.Observation{Kind: "pod_status", Namespace: ns, Name: name, TS: at,
		Fields: map[string]any{"status": status, "watch_type": watch}}
}

func event(ns, name, reason, msg string, at time.Time) sensorium.Observation {
	return sensorium.Observation{Kind: "event", Namespace: ns, Name: name, TS: at,
		Fields: map[string]any{"reason": reason, "message": msg, "involved_kind": "Pod", "event_type": "Warning"}}
}

func podBlock(name, re string, debounce int) DetectBlock {
	return DetectBlock{Playbook: name, DebounceSeconds: debounce,
		WatchPredicates: []WatchPredicate{{Kind: "Pod", StatusRegex: regexp.MustCompile(re)}}}
}

func TestFiresOnceAndClearsOnRecovery(t *testing.T) {
	e := NewEngine("c", []DetectBlock{podBlock("crash", `^CrashLoopBackOff$`, 0)})
	if f := e.Process(pod("ns", "p", "Running", t0, "ADDED")); len(f) != 0 {
		t.Fatal("healthy pod fired")
	}
	if f := e.Process(pod("ns", "p", "CrashLoopBackOff", t0, "MODIFIED")); len(f) != 1 || f[0].Evidence != "pod status=CrashLoopBackOff" {
		t.Fatalf("want one finding, got %+v", f)
	}
	if f := e.Process(pod("ns", "p", "CrashLoopBackOff", t0.Add(time.Second), "MODIFIED")); len(f) != 0 {
		t.Fatal("must not re-fire while the condition holds")
	}
	e.Process(pod("ns", "p", "Running", t0.Add(2*time.Second), "MODIFIED"))
	if f := e.Process(pod("ns", "p", "CrashLoopBackOff", t0.Add(3*time.Second), "MODIFIED")); len(f) != 1 {
		t.Fatal("should fire again after recovery")
	}
}

func TestDebounceWaitsThenFiresOnTick(t *testing.T) {
	e := NewEngine("c", []DetectBlock{podBlock("pending", `^Pending$`, 30)})
	if f := e.Process(pod("ns", "p", "Pending", t0, "ADDED")); len(f) != 0 {
		t.Fatal("fired before debounce")
	}
	if f := e.Tick(t0.Add(10 * time.Second)); len(f) != 0 {
		t.Fatal("fired too early")
	}
	f := e.Tick(t0.Add(31 * time.Second))
	if len(f) != 1 || f[0].Object != "p" {
		t.Fatalf("got %+v", f)
	}
	if len(e.Tick(t0.Add(40*time.Second))) != 0 {
		t.Fatal("tick must not re-fire")
	}
}

func TestDebounceClearedByRecovery(t *testing.T) {
	e := NewEngine("c", []DetectBlock{podBlock("pending", `^Pending$`, 30)})
	e.Process(pod("ns", "p", "Pending", t0, "ADDED"))
	e.Process(pod("ns", "p", "Running", t0.Add(5*time.Second), "MODIFIED"))
	if len(e.Tick(t0.Add(60*time.Second))) != 0 {
		t.Fatal("a pod that recovered inside the debounce must not fire")
	}
}

func TestDeletedDisarms(t *testing.T) {
	e := NewEngine("c", []DetectBlock{podBlock("term", `^Terminating$`, 30)})
	e.Process(pod("ns", "p", "Terminating", t0, "MODIFIED"))
	e.Process(pod("ns", "p", "Terminating", t0.Add(time.Second), "DELETED"))
	if len(e.Tick(t0.Add(time.Minute))) != 0 {
		t.Fatal("a normal termination must not fire")
	}
}

func eventBlock() DetectBlock {
	return DetectBlock{Playbook: "oom", WatchPredicates: []WatchPredicate{{
		Kind: "Event", ReasonRegex: regexp.MustCompile(`^BackOff$`), MessageRegex: regexp.MustCompile(`restarting`)}}}
}

func TestEventPredicateNeedsBothRegexes(t *testing.T) {
	e := NewEngine("c", []DetectBlock{eventBlock()})
	if len(e.Process(event("ns", "p", "BackOff", "pulling image", t0))) != 0 {
		t.Fatal("message must match too")
	}
	if len(e.Process(event("ns", "p", "BackOff", "Back-off restarting failed container", t0))) != 1 {
		t.Fatal("should fire")
	}
	normal := event("ns", "q", "BackOff", "restarting", t0)
	normal.Fields["event_type"] = "Normal"
	if len(e.Process(normal)) != 0 {
		t.Fatal("only Warning events count")
	}
}

func TestEventKeyExpiresAfterTTL(t *testing.T) {
	e := NewEngine("c", []DetectBlock{eventBlock()})
	e.Process(event("ns", "p", "BackOff", "restarting", t0))
	e.Tick(t0.Add(11 * time.Minute))
	if len(e.Process(event("ns", "p", "BackOff", "restarting", t0.Add(12*time.Minute)))) != 1 {
		t.Fatal("stale key should allow a re-fire")
	}
}

func TestEvidenceWithheldForProtectedNamespaces(t *testing.T) {
	e := NewEngine("c", []DetectBlock{eventBlock()})
	e.Blocked = map[string]bool{"kube-system": true}
	f := e.Process(event(" Kube-System ", "p", "BackOff", "restarting secret-token-abc", t0))
	if len(f) != 1 || strings.Contains(f[0].Evidence, "secret-token") || !strings.Contains(f[0].Evidence, "reason=BackOff") {
		t.Fatalf("got %+v", f)
	}
	g := e.Process(event("apps", "p", "BackOff", "restarting "+strings.Repeat("x", 300), t0))
	if len(g[0].Evidence) > len("event reason=BackOff message=")+140 {
		t.Error("message should be cut to 140 chars")
	}
}

func TestShadowNeverNotifies(t *testing.T) {
	e := NewEngine("c", nil)
	e.Shadow = []DetectBlock{podBlock("cand", `^Error$`, 0)}
	called := 0
	e.OnFinding = func(Finding) { called++ }
	e.Process(pod("ns", "p", "Error", t0, "ADDED"))
	if called != 0 || len(e.RecentFindings(10, 0)) != 0 {
		t.Fatal("shadow findings must not reach the callback or the ring")
	}
	if sf := e.ShadowFindings(); len(sf) != 1 || sf[0].Source != "shadow" {
		t.Fatalf("shadow ring: %+v", sf)
	}
}

func TestShadowDebounceFiresOnTick(t *testing.T) {
	e := NewEngine("c", nil)
	e.Shadow = []DetectBlock{podBlock("cand", `^Pending$`, 10)}
	e.Process(pod("ns", "p", "Pending", t0, "ADDED"))
	e.Tick(t0.Add(11 * time.Second))
	if len(e.ShadowFindings()) != 1 {
		t.Fatal("shadow debounce should expire on tick")
	}
}

type rec struct{ episodes []string }

func (r *rec) Record(ep, kind string, _ map[string]any) { r.episodes = append(r.episodes, ep+"/"+kind) }

func TestCallbackPanicAndRecorder(t *testing.T) {
	e := NewEngine("c1", []DetectBlock{podBlock("crash", `Crash`, 0)})
	r := &rec{}
	e.Recorder = r
	e.OnFinding = func(Finding) { panic("boom") }
	if f := e.Process(pod("ns", "p", "CrashLoopBackOff", t0, "ADDED")); len(f) != 1 {
		t.Fatal("a panicking callback must not lose the finding")
	}
	if len(r.episodes) != 1 || r.episodes[0] != "findings:c1/finding" {
		t.Errorf("recorder: %v", r.episodes)
	}
}

func TestRingIsBounded(t *testing.T) {
	e := NewEngine("c", []DetectBlock{podBlock("crash", `Crash`, 0)})
	for i := 0; i < findingsRingSize+20; i++ {
		e.Process(pod("ns", "p"+string(rune('a'+i%26))+string(rune('a'+i/26)), "CrashLoopBackOff", t0, "ADDED"))
	}
	if n := len(e.RecentFindings(0, 0)); n != findingsRingSize {
		t.Errorf("ring holds %d", n)
	}
	if n := len(e.RecentFindings(5, 0)); n != 5 {
		t.Errorf("limit: %d", n)
	}
}

func TestProjectETA(t *testing.T) {
	line := func(slope, start float64, n int) [][2]float64 {
		var s [][2]float64
		for i := 0; i < n; i++ {
			s = append(s, [2]float64{float64(i * 60), start + slope*float64(i*60)})
		}
		return s
	}
	eta, r2, ok := ProjectETA(line(1, 0, 10), 700, "rising", 0.5)
	if !ok || r2 < 0.99 || eta < 150 || eta > 160 {
		t.Errorf("rising: eta=%v r2=%v ok=%v", eta, r2, ok)
	}
	if _, _, ok := ProjectETA(line(-1, 1000, 10), 100, "falling", 0.5); !ok {
		t.Error("falling should project")
	}
	if _, _, ok := ProjectETA(line(-1, 1000, 10), 100, "rising", 0.5); ok {
		t.Error("moving away must not project")
	}
	if _, _, ok := ProjectETA(line(1, 0, 10), 5, "rising", 0.5); ok {
		t.Error("already past the threshold")
	}
	if _, _, ok := ProjectETA(line(0, 5, 10), 100, "rising", 0.5); ok {
		t.Error("flat series")
	}
	if _, _, ok := ProjectETA(line(1, 0, 1), 100, "rising", 0.5); ok {
		t.Error("one sample")
	}
	noisy := [][2]float64{{0, 1}, {60, 9}, {120, 2}, {180, 8}, {240, 3}}
	if _, _, ok := ProjectETA(noisy, 100, "rising", 0.9); ok {
		t.Error("noisy fit should be refused")
	}
}

type fakeSource struct {
	series []Series
	err    error
}

func (f fakeSource) QuerySeries(context.Context, string, int) ([]Series, error) {
	return f.series, f.err
}

func trendEngine(src SeriesSource) *Engine {
	e := NewEngine("c", []DetectBlock{{Playbook: "disk-fill", TrendPredicates: []TrendPredicate{{
		Metric: "usage", Threshold: 100, WindowMinutes: 30, ProjectionHorizonMin: 120,
		FireIfETAWithinMinutes: 30, Direction: "rising", MinR2: 0.5}}}})
	e.Series = src
	return e
}

func risingSeries() Series {
	var v [][2]float64
	for i := 0; i < 10; i++ {
		v = append(v, [2]float64{float64(i * 60), 50 + float64(i)*4})
	}
	return Series{Metric: map[string]string{"namespace": "db", "persistentvolumeclaim": "data"}, Values: v}
}

func TestTrendFiresPredictedOnceWithinRefireWindow(t *testing.T) {
	e := trendEngine(fakeSource{series: []Series{risingSeries()}})
	f := e.EvaluateTrends(context.Background(), t0)
	if len(f) != 1 || f[0].Severity != "predicted" || f[0].Source != "trend" || f[0].Object != "data" ||
		f[0].Namespace != "db" || f[0].ETAMinutes == nil || *f[0].ETAMinutes <= 0 {
		t.Fatalf("got %+v", f)
	}
	if len(e.EvaluateTrends(context.Background(), t0.Add(10*time.Minute))) != 0 {
		t.Error("same prediction must not repeat inside the window")
	}
	if len(e.EvaluateTrends(context.Background(), t0.Add(40*time.Minute))) != 1 {
		t.Error("should repeat after the window")
	}
	if blind, _, _ := e.Predictive(); blind {
		t.Error("not blind when queries work")
	}
}

func TestTrendOutageIsRememberedNotSilent(t *testing.T) {
	e := trendEngine(fakeSource{err: errors.New("prometheus down")})
	if f := e.EvaluateTrends(context.Background(), t0); len(f) != 0 {
		t.Fatal("no findings on outage")
	}
	blind, since, msg := e.Predictive()
	if !blind || since == 0 || msg != "prometheus down" {
		t.Errorf("blind=%v since=%v msg=%q", blind, since, msg)
	}
	e.Series = fakeSource{series: []Series{risingSeries()}}
	e.EvaluateTrends(context.Background(), t0.Add(time.Minute))
	if blind, _, _ := e.Predictive(); blind {
		t.Error("recovers once a query succeeds")
	}
	if blind, _, _ := trendEngine(nil).Predictive(); blind {
		t.Error("blind only after a failed sweep")
	}
}

func TestParseBlock(t *testing.T) {
	raw := map[string]any{
		"debounce_seconds": 30,
		"watch_predicates": []any{
			map[string]any{"kind": "Pod", "status_regex": "^Pending$"},
			map[string]any{"kind": "Event", "reason_regex": "^Failed$", "message_regex": "pull", "involved_kind": "Pod"},
			map[string]any{"status_regex": "no kind, skipped"},
			"not a map",
		},
		"trend_predicates": []any{
			map[string]any{"metric": "m", "threshold": 90, "min_r2": 0, "window_minutes": 0, "fire_if_eta_within_minutes": "15"},
			map[string]any{"metric": "no threshold"},
		},
	}
	b, err := ParseBlock("pb", raw)
	if err != nil || b == nil {
		t.Fatalf("err=%v block=%v", err, b)
	}
	if b.DebounceSeconds != 30 || len(b.WatchPredicates) != 2 || len(b.TrendPredicates) != 1 {
		t.Fatalf("%+v", b)
	}
	tp := b.TrendPredicates[0]
	if tp.MinR2 != 0 {
		t.Error("min_r2 of zero is a real setting")
	}
	if tp.WindowMinutes != 30 {
		t.Error("a zero window falls back to the default")
	}
	if tp.FireIfETAWithinMinutes != 15 || tp.Direction != "rising" || tp.ProjectionHorizonMin != 120 {
		t.Errorf("%+v", tp)
	}
	if b.WatchPredicates[1].InvolvedKind != "Pod" || b.WatchPredicates[1].MessageRegex == nil {
		t.Errorf("%+v", b.WatchPredicates[1])
	}
}

func TestParseBlockRefusals(t *testing.T) {
	if b, err := ParseBlock("x", nil); b != nil || err != nil {
		t.Error("nil block is LLM-only")
	}
	if b, _ := ParseBlock("x", map[string]any{"promql": []any{"up == 0"}}); b != nil {
		t.Error("promql alone must not make a live detector")
	}
	bad := map[string]any{"watch_predicates": []any{map[string]any{"kind": "Pod", "status_regex": "("}}}
	if _, err := ParseBlock("x", bad); err == nil {
		t.Error("bad regex is an error")
	}
	neg := map[string]any{"debounce_seconds": -5, "watch_predicates": []any{map[string]any{"kind": "Pod", "status_regex": "x"}}}
	if b, _ := ParseBlock("x", neg); b.DebounceSeconds != 0 {
		t.Error("negative debounce falls back to zero")
	}
}

func TestFindingDictShape(t *testing.T) {
	e := NewEngine("c", []DetectBlock{podBlock("crash", `Crash`, 0)})
	f := e.Process(pod("ns", "p", "CrashLoopBackOff", t0, "ADDED"))[0]
	d := f.Dict()
	for _, k := range []string{"type", "id", "playbook", "cluster_id", "namespace", "object", "evidence", "first_seen", "fired_at", "source", "severity", "eta_minutes"} {
		if _, ok := d[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
	if !strings.HasPrefix(f.ID, "fnd-") || len(f.ID) != 16 {
		t.Errorf("id %q", f.ID)
	}
}
