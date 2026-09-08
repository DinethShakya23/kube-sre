package perception

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/detect"
	"github.com/DinethShakya23/kube-sre/internal/playbooks"
)

func fakeKubectl(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kubectl")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newService(t *testing.T, env map[string]string, body string) *Service {
	t.Helper()
	cfg := config.Load(func(k string) string { return env[k] })
	s := NewService(cfg, playbooks.Load())
	s.Bin = fakeKubectl(t, body)
	s.ClusterID = func(context.Context) string { return "c1" }
	return s
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

const crashPod = `{"type":"ADDED","object":{"kind":"Pod","metadata":{"namespace":"shop","name":"web-1"},"status":{"phase":"Running","containerStatuses":[{"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}}}`

func TestDetectsAFailureFromTheWatchStream(t *testing.T) {
	s := newService(t, nil, `case "$*" in *pods*) echo '`+crashPod+`';; esac
exec sleep 30`)
	var fired []detect.Finding
	got := make(chan struct{}, 4)
	s.OnFinding = func(f detect.Finding) { fired = append(fired, f); got <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Stop("", "")
	waitFor(t, "a stream to connect", func() bool { return s.State().Sensorium == Active })
	// CrashLoopBackOff debounces for 60 seconds, so drive the clock instead of
	// waiting, ticking until the observation has reached the engine.
	deadline := time.Now().Add(10 * time.Second)
wait:
	for {
		s.Engine().Tick(time.Now().Add(2 * time.Minute))
		select {
		case <-got:
			break wait
		case <-time.After(100 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("no finding")
			}
		}
	}
	if len(fired) != 1 || fired[0].Playbook != "CrashLoopBackOff" || fired[0].Object != "web-1" || fired[0].Namespace != "shop" || fired[0].ClusterID != "c1" {
		t.Errorf("%+v", fired)
	}
	if recent := s.Engine().RecentFindings(10, 0); len(recent) != 1 {
		t.Errorf("%v", recent)
	}
	st := s.State()
	if !st.Watching() || st.Detectors != 20 || st.Predictive != Off || len(Gaps(st)) != 0 {
		t.Errorf("%+v gaps=%v", st, Gaps(st))
	}
	if h := s.Status(); h["enabled"] != true || h["watching"] != true || h["state"] != Running {
		t.Errorf("%v", h)
	}
}

func TestAbsenceReasonsAreDistinct(t *testing.T) {
	s := newService(t, nil, `exit 0`)
	if st := s.State(); st.Sensorium != Disabled || !strings.Contains(st.SensoriumReason, "has not started yet") {
		t.Errorf("%+v", st)
	}
	s.RecordDisabled()
	if st := s.State(); !strings.Contains(st.SensoriumReason, "SENSORIUM_ENABLED=false") {
		t.Errorf("%s", st.SensoriumReason)
	}
	if h := s.Status(); h["enabled"] != false || h["state"] != DisabledByFlag || h["watching"] != false {
		t.Errorf("%v", h)
	}
	s.RecordStartFailure(os.ErrPermission)
	if st := s.State(); !strings.Contains(st.SensoriumReason, "FAILED to start (permission denied)") || !strings.Contains(st.SensoriumReason, "outage rather than a setting") {
		t.Errorf("%s", st.SensoriumReason)
	}
	s.Stop(Standby, "another replica holds the singleton lock")
	if st := s.State(); !strings.Contains(st.SensoriumReason, "standby") || !strings.Contains(st.SensoriumReason, "not this replica's silence") {
		t.Errorf("%s", st.SensoriumReason)
	}
	s.Stop("", "")
	if st := s.State(); !strings.Contains(st.SensoriumReason, "shutting down") {
		t.Errorf("%s", st.SensoriumReason)
	}
	if g := Gaps(State{Sensorium: Disabled}); len(g) != 1 || g[0] == "" {
		t.Errorf("an empty reason still yields a sentence: %v", g)
	}
}

func TestMissingKubectlIsStoppedNotQuiet(t *testing.T) {
	s := newService(t, nil, `exit 0`)
	s.Bin = "/nonexistent/kubectl"
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop("", "")
	waitFor(t, "streams to stop", func() bool { return s.State().Sensorium == Stopped })
	st := s.State()
	if st.Watching() || s.Status()["watching"] != false || !strings.Contains(s.Status()["reason"].(string), "nothing is being observed") {
		t.Errorf("%+v %v", st, s.Status())
	}
	g := Gaps(st)
	if len(g) != 1 || !strings.Contains(g[0], "every kubectl watch stream has stopped") || !strings.Contains(g[0], "kubectl not found") {
		t.Errorf("%v", g)
	}
}

func TestReconnectingWhenTheStreamKeepsFailing(t *testing.T) {
	s := newService(t, nil, `echo 'Error from server (Forbidden): pods is forbidden' >&2; exit 1`)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop("", "")
	waitFor(t, "reconnecting", func() bool { return s.State().Sensorium == Reconnecting })
	g := Gaps(s.State())
	if len(g) != 1 || !strings.Contains(g[0], "reconnecting") || !strings.Contains(g[0], "Forbidden") {
		t.Errorf("%v", g)
	}
}

func TestNoDetectorsMeansNotStarted(t *testing.T) {
	cfg := config.Load(func(string) string { return "" })
	s := NewService(cfg, &playbooks.Registry{})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Absence(); r != NoDetectors || s.Engine() != nil {
		t.Errorf("%s", r)
	}
	if !strings.Contains(s.State().SensoriumReason, "loaded no compiled detectors") {
		t.Error(s.State().SensoriumReason)
	}
}

func TestGapsCoverEveryWayToBeBlind(t *testing.T) {
	scoped := Gaps(State{Sensorium: Active, WatchNamespaces: []string{"shop", "api"}})
	if len(scoped) != 1 || !strings.Contains(scoped[0], "only 2 namespace(s) (shop, api)") || !strings.Contains(scoped[0], "not a statement about the cluster") {
		t.Errorf("a scoped sensorium is blind outside its scope: %v", scoped)
	}
	shed := Gaps(State{Sensorium: Active, ShedTotal: 42, QueueHighWater: 10000})
	if len(shed) != 1 || !strings.Contains(shed[0], "dropped 42 event(s)") || !strings.Contains(shed[0], "high-water 10000") {
		t.Errorf("a shedding queue leaves every instrument healthy: %v", shed)
	}
	e := "prometheus down"
	blind := Gaps(State{Sensorium: Active, Predictive: Blind, PredictiveError: &e})
	if len(blind) != 1 || !strings.Contains(blind[0], "predictive detection is blind") || !strings.Contains(blind[0], "prometheus down") {
		t.Errorf("%v", blind)
	}
	if g := Gaps(State{Sensorium: Active, Predictive: Off}); len(g) != 0 {
		t.Errorf("predictive off is a choice, not a gap: %v", g)
	}
	if g := Gaps(State{Sensorium: Starting}); len(g) != 1 || !strings.Contains(g[0], "no kubectl watch stream has started") {
		t.Errorf("%v", g)
	}
	all := Gaps(State{Sensorium: Reconnecting, Predictive: Blind, WatchNamespaces: []string{"a"}, ShedTotal: 1})
	if len(all) != 4 {
		t.Errorf("independent gaps are all reported: %v", all)
	}
}

func TestPredictiveStateFollowsTheFlagAndTheEngine(t *testing.T) {
	s := newService(t, map[string]string{"PREDICTIVE_DETECTION_ENABLED": "true"}, `exec sleep 30`)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop("", "")
	if st := s.State(); st.PredictiveDetectors != 1 || st.Predictive != "active" {
		t.Errorf("only OOMKilled carries a trend predicate: %+v", st)
	}
	s.Engine().Series = nil
	s.Engine().EvaluateTrends(context.Background(), time.Now())
	if st := s.State(); st.Predictive != Blind || st.PredictiveError == nil {
		t.Errorf("a failed sweep makes the predictive layer blind: %+v", st)
	}
}

func TestQueueStatsSurviveAStop(t *testing.T) {
	s := newService(t, nil, `exec sleep 30`)
	if q := s.Queue(); q.MaxSize != 10000 || q.ShedTotal != 0 {
		t.Errorf("%+v", q)
	}
	s.Start(context.Background())
	s.Stop("", "")
	if q := s.Queue(); q.MaxSize != 10000 {
		t.Errorf("%+v", q)
	}
}

func TestStoredDetectorRefreshKeepsTheLoadedSetOnAFailedRead(t *testing.T) {
	cfg := config.Load(func(string) string { return "" })
	s := NewService(cfg, playbooks.Load())
	eng := detect.NewEngine("c", nil)
	block := detect.DetectBlock{Playbook: "nl:a"}
	var fail bool
	s.StoredDetectors = func(context.Context, string) ([]detect.DetectBlock, []detect.DetectBlock, error) {
		if fail {
			return nil, nil, errors.New("store down")
		}
		return []detect.DetectBlock{block}, []detect.DetectBlock{{Playbook: "nl:b"}}, nil
	}
	s.refreshStored(context.Background(), eng, "c")
	if len(eng.Detectors) != 1 || len(eng.Shadow) != 1 {
		t.Fatalf("%d %d", len(eng.Detectors), len(eng.Shadow))
	}
	fail = true
	s.refreshStored(context.Background(), eng, "c")
	if len(eng.Detectors) != 1 || len(eng.Shadow) != 1 {
		t.Errorf("a failed read disarmed the engine: %d %d", len(eng.Detectors), len(eng.Shadow))
	}
	fail = false
	s.StoredDetectors = func(context.Context, string) ([]detect.DetectBlock, []detect.DetectBlock, error) {
		return nil, nil, nil
	}
	s.refreshStored(context.Background(), eng, "c")
	if len(eng.Detectors) != 0 || len(eng.Shadow) != 0 {
		t.Errorf("a successful empty read should clear them: %d %d", len(eng.Detectors), len(eng.Shadow))
	}
}
