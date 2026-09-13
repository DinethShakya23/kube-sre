package autonomy

import (
	"strings"
	"testing"

	"github.com/DinethShakya23/kube-sre/internal/config"
)

func envCfg(env map[string]string) *config.Config {
	return config.Load(func(k string) string { return env[k] })
}

func TestKillSwitchComposesBothSourcesAndIsNotGatedOnTheFeatureFlag(t *testing.T) {
	b := NewBudget(envCfg(nil))
	if d := b.AutoWritePermitted(); !d.Allow {
		t.Errorf("default: %+v", d)
	}
	b.EngageKillSwitch()
	if d := b.AutoWritePermitted(); d.Allow || d.Reason != "kill switch engaged" {
		t.Errorf("a runtime engage must stop writes even with KI_V5_BLAST_RADIUS_BUDGET off: %+v", d)
	}
	b.DisengageKillSwitch()
	if !b.AutoWritePermitted().Allow {
		t.Error("disengaged")
	}
	if d := NewBudget(cfg(map[string]string{"KI_V5_KILL_SWITCH": "true"})).AutoWritePermitted(); d.Allow {
		t.Errorf("setting: %+v", d)
	}
}

func TestAFreezeDeclaredByTheSettingStopsBothGates(t *testing.T) {
	b := NewBudget(cfg(map[string]string{"KI_V5_CHANGE_FREEZE": "true"}))
	if b.AutoWritePermitted().Allow {
		t.Error("watchtower gate")
	}
	if d := b.GateWrite(WriteRequest{}); d.Allow || d.Reason != "change freeze in effect" {
		t.Errorf("the write chokepoint passes no windows and no clock, and must still see the freeze: %+v", d)
	}
	open := NewBudget(envCfg(nil))
	now := 150.0
	if d := open.GateWrite(WriteRequest{Now: &now, FreezeWindows: []Window{{100, 200}}}); d.Allow {
		t.Errorf("inside a window: %+v", d)
	}
	now = 200 // end is exclusive
	if d := open.GateWrite(WriteRequest{Now: &now, FreezeWindows: []Window{{100, 200}}}); !d.Allow {
		t.Errorf("%+v", d)
	}
}

func TestGateWritePrecedence(t *testing.T) {
	b := NewBudget(cfg(map[string]string{"KI_V5_KILL_SWITCH": "true", "KI_V5_CHANGE_FREEZE": "true"}))
	if d := b.GateWrite(WriteRequest{GovernanceDown: true}); !strings.Contains(d.Reason, "governance unreachable") {
		t.Errorf("governance first: %+v", d)
	}
	if d := b.GateWrite(WriteRequest{}); d.Reason != "kill switch engaged" {
		t.Errorf("kill before freeze: %+v", d)
	}
	plain := NewBudget(envCfg(nil))
	if d := plain.GateWrite(WriteRequest{CurrentSpend: 9, ProjectedSpend: 2, SpendCap: 10}); d.Allow || !strings.Contains(d.Reason, "projected spend 11 > cap 10") {
		t.Errorf("%+v", d)
	}
	if !plain.GateWrite(WriteRequest{CurrentSpend: 9, ProjectedSpend: 1, SpendCap: 10}).Allow {
		t.Error("exactly at the cap is allowed")
	}
}

func TestSpendCapZeroMeansUnlimited(t *testing.T) {
	for _, c := range []float64{0, -1} {
		if !CheckSpend(1e9, 1e9, c).Allow {
			t.Errorf("cap %v", c)
		}
	}
}

func TestZoneDisruption(t *testing.T) {
	if d := ZoneDisruptionOK(0, 0, 0.34); d.Allow || !strings.Contains(d.Reason, "fail-closed") {
		t.Errorf("unknown size: %+v", d)
	}
	if !ZoneDisruptionOK(3, 0, 0.34).Allow {
		t.Error("1 of 3 is inside a 34% cap")
	}
	if d := ZoneDisruptionOK(3, 1, 0.34); d.Allow || !strings.Contains(d.Reason, "67%") {
		t.Errorf("%+v", d)
	}
	if ZoneDisruptionOK(1, 0, 0.34).Allow {
		t.Error("a zone of one has no redundancy")
	}
	if !ZoneDisruptionOK(1, 0, 1.0).Allow {
		t.Error("unless the cap allows everything")
	}
}

func TestMaintenanceWindows(t *testing.T) {
	if !InMaintenanceWindow(5, nil) {
		t.Error("no schedule means always allowed")
	}
	now := 5.0
	if d := GateDisruption(10, 0, 0.5, &now, []Window{{100, 200}}); d.Allow || d.Reason != "outside the allowed maintenance window" {
		t.Errorf("schedule is checked first: %+v", d)
	}
	now = 150
	if !GateDisruption(10, 0, 0.5, &now, []Window{{100, 200}}).Allow {
		t.Error("inside the window and the zone")
	}
	if GateDisruption(10, 5, 0.5, nil, nil).Allow {
		t.Error("the domain still applies with no schedule")
	}
}

func TestStagedPropagationNeverReleasesEverythingAtOnce(t *testing.T) {
	targets := []string{"a", "b", "c", "d"}
	s := NextStage(targets, nil, 2, 300, nil, 1000)
	if strings.Join(s.Batch, ",") != "a,b" || s.Reason != "releasing 2 of 4 remaining" {
		t.Errorf("%+v", s)
	}
	last := 1000.0
	if s = NextStage(targets, []string{"a", "b"}, 2, 300, &last, 1100); !s.Waiting || len(s.Batch) != 0 || !strings.Contains(s.Reason, "200s until next stage") {
		t.Errorf("%+v", s)
	}
	if s = NextStage(targets, []string{"a", "b"}, 2, 300, &last, 1300); strings.Join(s.Batch, ",") != "c,d" {
		t.Errorf("window elapsed: %+v", s)
	}
	if s = NextStage(targets, targets, 2, 300, &last, 9999); !s.Done {
		t.Errorf("%+v", s)
	}
	if s = NextStage(targets, nil, 0, 300, nil, 0); len(s.Batch) != 1 {
		t.Errorf("a stage size below 1 is 1: %+v", s)
	}
	if !IsInstantGlobal(targets, 4) || IsInstantGlobal(targets, 3) || IsInstantGlobal([]string{"a"}, 5) {
		t.Error("instant global is more than one target released in one stage")
	}
}

func TestComposeIsFirstDenialWins(t *testing.T) {
	allow, deny := Decision{Allow: true}, Decision{false, "no"}
	stage := Stage{Batch: []string{"a"}, Reason: "releasing 1 of 3 remaining"}
	if v := Compose(deny, stage, &deny); v.Allow || v.Reasons[0] != "budget: no" || v.Batch != nil {
		t.Errorf("%+v", v)
	}
	if v := Compose(allow, stage, &deny); v.Allow || v.Reasons[0] != "failure-domain: no" {
		t.Errorf("%+v", v)
	}
	if v := Compose(allow, Stage{Waiting: true, Reason: "wait"}, nil); !v.Allow || len(v.Batch) != 0 || v.Reasons[0] != "staged: wait" {
		t.Errorf("%+v", v)
	}
	if v := Compose(allow, Stage{Done: true}, nil); !v.Allow || len(v.Batch) != 0 || v.Reasons[0] != "staged: all targets applied" {
		t.Errorf("%+v", v)
	}
	if v := Compose(allow, stage, &allow); !v.Allow || strings.Join(v.Batch, ",") != "a" || len(v.Reasons) != 1 {
		t.Errorf("%+v", v)
	}
}
