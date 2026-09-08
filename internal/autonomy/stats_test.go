package autonomy

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func run(n int, failAt map[int]bool, spanDays float64, types int) []Event {
	var ev []Event
	for i := 0; i < n; i++ {
		ev = append(ev, Event{TSDays: spanDays * float64(i) / float64(n-1), Success: !failAt[i], IncidentID: fmt.Sprint("i", i), IncidentType: fmt.Sprint("t", i%types)})
	}
	return ev
}

func TestWilsonKnownValues(t *testing.T) {
	if WilsonLCB(0, 0) != 0 {
		t.Error("no data")
	}
	if got := WilsonLCB(20, 20); math.Abs(got-0.8808) > 0.001 {
		t.Errorf("20/20: %v", got)
	}
	if got := WilsonLCB(0, 10); got != 0 {
		t.Errorf("0/10: %v", got)
	}
	if WilsonLCB(50, 100) >= 0.5 || WilsonLCB(50, 100) <= 0.4 {
		t.Errorf("50/100: %v", WilsonLCB(50, 100))
	}
}

func TestPromotionNeedsEveryGate(t *testing.T) {
	good := run(40, nil, 20, 3)
	d, err := EvaluatePromotion("L2->L3", good, 20)
	if err != nil || !d.Promote || len(d.Reasons) != 0 {
		t.Fatalf("%+v %v", d, err)
	}
	short := run(40, nil, 3, 3)
	d, _ = EvaluatePromotion("L2->L3", short, 3)
	if d.Promote || !strings.Contains(strings.Join(d.Reasons, ";"), "time span") {
		t.Errorf("%+v", d)
	}
	d, _ = EvaluatePromotion("L2->L3", run(40, nil, 20, 1), 20)
	if d.Promote || !strings.Contains(strings.Join(d.Reasons, ";"), "distinct types 1 < 3") {
		t.Errorf("%+v", d)
	}
	few := run(10, nil, 20, 3)
	if d, _ = EvaluatePromotion("L2->L3", few, 20); d.Promote || !strings.Contains(strings.Join(d.Reasons, ";"), "n 10 < n_min 30") {
		t.Errorf("%+v", d)
	}
	crit := run(40, nil, 20, 3)
	crit[5].Critical = true
	if d, _ = EvaluatePromotion("L2->L3", crit, 20); d.Promote || !strings.Contains(strings.Join(d.Reasons, ";"), "M4 > 0") {
		t.Errorf("%+v", d)
	}
	if d, _ = EvaluatePromotion("L2->L3", run(40, map[int]bool{1: true, 2: true, 3: true, 4: true}, 20, 3), 20); d.Promote {
		t.Errorf("failures should lower the bound: %+v", d)
	}
}

func TestIrreversibleNeverPromotes(t *testing.T) {
	d, _ := EvaluatePromotion("L3->L4:irreversible", run(500, nil, 200, 5), 200)
	if d.Promote || d.Theta != nil {
		t.Errorf("%+v", d)
	}
	if _, err := EvaluatePromotion("L9->L10", nil, 0); err == nil {
		t.Error("unknown transition")
	}
}

func TestWindowKeepsTheLatestEventsInRange(t *testing.T) {
	var ev []Event
	for i := 0; i < 300; i++ {
		ev = append(ev, Event{TSDays: float64(i) * 0.1, Success: true, IncidentID: fmt.Sprint(i)})
	}
	ev = append(ev, Event{TSDays: -500, Success: false, IncidentID: "ancient"})
	w := window(ev, 30)
	if len(w) != WindowMaxEvents || w[0].TSDays < 9 {
		t.Errorf("%d first=%v", len(w), w[0].TSDays)
	}
}

func TestOfflineWeighting(t *testing.T) {
	if CalibrateOfflineWeight(nil, 0.5) != 0 {
		t.Error("no pairs")
	}
	if got := CalibrateOfflineWeight([][2]bool{{true, true}, {true, true}, {false, true}, {true, true}}, 0.5); got != 0.5 {
		t.Errorf("clamped: %v", got)
	}
	if got := CalibrateOfflineWeight([][2]bool{{true, false}, {true, true}}, 0.9); got != 0.5 {
		t.Errorf("agreement: %v", got)
	}
	live := []Event{{Success: true}, {Success: true}, {Success: true}, {Success: true}}
	off := []Event{{Success: true, Offline: true}, {Success: true, Offline: true}, {Success: true, Offline: true}, {Success: true, Offline: true}}
	if WeightedLCB(off, 0.5) >= WeightedLCB(live, 0.5) {
		t.Error("offline evidence should count for less")
	}
	if WeightedLCB(off, 0) != 0 {
		t.Error("a zero weight means no evidence")
	}
}

func TestCusumCountsCriticalFailures(t *testing.T) {
	ev := run(48, nil, 30, 3)
	ev = append(ev, Event{TSDays: 30, Success: false, IncidentID: "a", Critical: true}, Event{TSDays: 30.5, Success: false, IncidentID: "b", Critical: true})
	if !CusumTrip(ev, 2, 1) {
		t.Error("two failures 12h apart must trip, critical or not")
	}
	d := EvaluateDemotion("L3", 0.9, ev, 31, false, false, false)
	if !d.Demote || d.To != "L2" || !strings.Contains(d.Reason, "caused a critical incident") {
		t.Errorf("%+v", d)
	}
	spread := []Event{{TSDays: 0}, {TSDays: 5}}
	if CusumTrip(spread, 2, 1) {
		t.Error("failures days apart are not a trip")
	}
}

func TestHysteresisAbstainsWhereAPerfectRecordWouldBreach(t *testing.T) {
	flawless := run(15, nil, 10, 3)
	if HysteresisBreach(flawless, 0.95, 50, 0.05) {
		t.Error("15 for 15 is not evidence of decline")
	}
	if d := EvaluateDemotion("L2", 0.95, flawless, 10, false, false, false); d.Demote {
		t.Errorf("%+v", d)
	}
	worn := run(60, map[int]bool{10: true, 30: true, 45: true, 50: true, 55: true}, 60, 3)
	if !HysteresisBreach(worn, 0.95, 50, 0.05) {
		t.Error("an established record with failures should breach")
	}
}

func TestDemotionPrecedenceAndScoping(t *testing.T) {
	ev := run(40, nil, 30, 3)
	d := EvaluateDemotion("L4", 0.95, ev, 30, true, false, false)
	if !d.Demote || d.To != "L2" || d.FleetFreezeDays != 14 {
		t.Errorf("sev at L4: %+v", d)
	}
	if d = EvaluateDemotion("L4", 0.95, ev, 30, false, true, false); d.To != "L3" {
		t.Errorf("m4: %+v", d)
	}
	if d = EvaluateDemotion("L2", 0.9, ev, 30, false, false, true); d.Demote || !d.Stale {
		t.Errorf("drift: %+v", d)
	}
	d = EvaluateDemotion("L2", 0.9, ev, 30, true, false, false)
	if d.Demote || !strings.Contains(d.Reason, "NOT a clean class") {
		t.Errorf("a severity signal below L4 must not read as clean: %+v", d)
	}
	if d = EvaluateDemotion("L0", 0.9, run(30, map[int]bool{1: true, 2: true}, 0.5, 3), 1, false, false, false); d.To != "L0" {
		t.Errorf("cannot drop below L0: %+v", d)
	}
	if d = EvaluateDemotion("L2", 0.9, ev, 30, false, false, false); d.Demote || d.Reason != "no demotion trigger" {
		t.Errorf("%+v", d)
	}
}
