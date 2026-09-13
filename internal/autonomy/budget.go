package autonomy

import (
	"fmt"
	"sync/atomic"

	"github.com/DinethShakya23/kube-sre/internal/config"
)

// The blast radius and spend gate: an additional gate on autonomous writes, layered on
// top of the autonomy ladder and the A3 allowlist and never a replacement. Three fail
// closed brakes:
//
//	kill switch    engaged means deny every autonomous write (the setting or a runtime toggle)
//	change freeze  deny by default during a moratorium (the setting, or an injected window)
//	spend cap      deny before breach: an action whose projected cost would push the scope
//	               over its cap is denied before it runs
//
// The asymmetry: it fails closed for agent write authority (governance unreachable
// means no auto write), but never touches the data or cluster plane, so it can never
// block a running workload or a human break glass.

// Decision is a gate verdict.
type Decision struct {
	Allow  bool
	Reason string
}

// Window is a [Start, End) span of unix seconds.
type Window struct{ Start, End float64 }

// Budget holds the runtime brake state.
type Budget struct {
	Cfg  *config.Config
	kill atomic.Bool
}

func NewBudget(cfg *config.Config) *Budget { return &Budget{Cfg: cfg} }

// EngageKillSwitch and DisengageKillSwitch flip the runtime toggle. It is per process
// and is not shared between replicas, so setting KI_V5_KILL_SWITCH and restarting is
// what stops a fleet.
func (b *Budget) EngageKillSwitch()    { b.kill.Store(true) }
func (b *Budget) DisengageKillSwitch() { b.kill.Store(false) }

// KillSwitchEngaged composes both sources, so every gate agrees.
func (b *Budget) KillSwitchEngaged() bool { return b.kill.Load() || b.Cfg.V5KillSwitch }

// CheckSpend denies before breach. A cap that is not positive means unlimited.
func CheckSpend(current, projected, cap float64) Decision {
	if cap <= 0 {
		return Decision{Allow: true}
	}
	if current+projected > cap {
		return Decision{false, fmt.Sprintf("projected spend %.4g > cap %.4g", current+projected, cap)}
	}
	return Decision{Allow: true}
}

// InChangeFreeze is true when now falls inside any window.
func InChangeFreeze(now float64, windows []Window) bool {
	for _, w := range windows {
		if w.Start <= now && now < w.End {
			return true
		}
	}
	return false
}

// ChangeFreezeActive is true when a freeze is declared, by the operator or by a
// window. It is the counterpart of KillSwitchEngaged for the same reason: both brakes
// have two sources and are read by two gates. The kill switch got one function that
// composes its sources; the freeze did not, so a declared freeze stopped the
// watchtower and left the write chokepoint, which passes it no windows, allowing.
func (b *Budget) ChangeFreezeActive(now *float64, windows []Window) bool {
	if b.Cfg.V5ChangeFreeze {
		return true
	}
	return now != nil && len(windows) > 0 && InChangeFreeze(*now, windows)
}

// WriteRequest is the input to GateWrite. Governance defaults to reachable.
type WriteRequest struct {
	GovernanceDown bool
	CurrentSpend   float64
	ProjectedSpend float64
	SpendCap       float64
	Now            *float64
	FreezeWindows  []Window
}

// GateWrite is the full composable write gate, fail closed, first denial wins:
// governance, kill switch, change freeze, spend.
func (b *Budget) GateWrite(r WriteRequest) Decision {
	switch {
	case r.GovernanceDown:
		return Decision{false, "governance unreachable — fail-closed (no write authority)"}
	case b.KillSwitchEngaged():
		return Decision{false, "kill switch engaged"}
	case b.ChangeFreezeActive(r.Now, r.FreezeWindows):
		return Decision{false, "change freeze in effect"}
	}
	return CheckSpend(r.CurrentSpend, r.ProjectedSpend, r.SpendCap)
}

// AutoWritePermitted is the settings driven gate for the watchtower's A3 path: deny
// on an engaged kill switch or a declared freeze, otherwise allow (the spend cap is
// enforced by GateWrite, where a usage figure exists).
//
// Neither brake is gated on KI_V5_BLAST_RADIUS_BUDGET, deliberately. They are not
// features to opt into, they are an operator saying stop. Allowing on the feature
// flag before consulting them meant an engaged kill switch did nothing while the
// status endpoint reported it engaged: an operator breaking glass mid incident was
// told the agent had stopped writing while it went on auto fixing.
func (b *Budget) AutoWritePermitted() Decision {
	switch {
	case b.KillSwitchEngaged():
		return Decision{false, "kill switch engaged"}
	case b.ChangeFreezeActive(nil, nil):
		return Decision{false, "change freeze in effect"}
	}
	return Decision{Allow: true}
}

// ── failure domains ──────────────────────────────────────────────────────────

// ZoneDisruptionOK asks whether disrupting one more target in a zone keeps it within
// its unavailability cap. A zone of one is never auto disrupted unless the cap is at
// least 1.
func ZoneDisruptionOK(zoneTotal, unavailable int, maxFrac float64) Decision {
	if zoneTotal <= 0 {
		return Decision{false, "unknown zone size — fail-closed"}
	}
	projected := float64(unavailable+1) / float64(zoneTotal)
	if projected > maxFrac {
		return Decision{false, fmt.Sprintf("would make %.0f%% of the zone unavailable (cap %.0f%%)", projected*100, maxFrac*100)}
	}
	return Decision{Allow: true}
}

// InMaintenanceWindow is true inside an allowed window. No windows means no schedule
// is configured, so always allowed.
func InMaintenanceWindow(now float64, windows []Window) bool {
	return len(windows) == 0 || InChangeFreeze(now, windows)
}

// GateDisruption composes the schedule and the failure domain checks, first denial
// wins (schedule, then domain).
func GateDisruption(zoneTotal, unavailable int, maxFrac float64, now *float64, windows []Window) Decision {
	if now != nil && len(windows) > 0 && !InMaintenanceWindow(*now, windows) {
		return Decision{false, "outside the allowed maintenance window"}
	}
	return ZoneDisruptionOK(zoneTotal, unavailable, maxFrac)
}

// ── staged propagation ───────────────────────────────────────────────────────

// Stage is what to do next. A change touching many targets must never apply to all of
// them at once: it is released in bounded stages with a mandatory wait between, so a
// bad change is caught on the first stage's blast radius and not on the fleet.
type Stage struct {
	Batch   []string
	Waiting bool
	Done    bool
	Reason  string
}

// NextStage decides the next propagation stage. All applied means done; a prior stage
// whose window has not elapsed means waiting with an empty batch; otherwise the next
// stageSize targets not yet applied, in order.
func NextStage(targets, applied []string, stageSize int, windowSeconds float64, lastStage *float64, now float64) Stage {
	done := map[string]bool{}
	for _, a := range applied {
		done[a] = true
	}
	var remaining []string
	for _, t := range targets {
		if !done[t] {
			remaining = append(remaining, t)
		}
	}
	if len(remaining) == 0 {
		return Stage{Done: true, Reason: "all targets applied"}
	}
	if lastStage != nil && now-*lastStage < windowSeconds {
		return Stage{Waiting: true, Reason: fmt.Sprintf("stage window: %.0fs until next stage", windowSeconds-(now-*lastStage))}
	}
	size := stageSize
	if size < 1 {
		size = 1
	}
	if size > len(remaining) {
		size = len(remaining)
	}
	return Stage{Batch: remaining[:size], Reason: fmt.Sprintf("releasing %d of %d remaining", size, len(remaining))}
}

// IsInstantGlobal is true when the config would apply everything in one stage, the
// thing forbidden for more than one target.
func IsInstantGlobal(targets []string, stageSize int) bool {
	return len(targets) > 1 && stageSize >= len(targets)
}

// ── the composite ────────────────────────────────────────────────────────────

// Verdict is the composed blast radius decision.
type Verdict struct {
	Allow   bool
	Batch   []string // targets cleared to change this stage
	Reasons []string // every brake that fired; empty means allowed
}

// Compose is the single decision the action path calls before a disruptive change.
// Order: budget, failure domain, propagation. Budget or domain denial denies
// outright; a waiting or finished stage is allowed with an empty batch (nothing to
// apply this tick); otherwise it allows with the stage's batch.
func Compose(budget Decision, stage Stage, domain *Decision) Verdict {
	switch {
	case !budget.Allow:
		return Verdict{false, nil, []string{"budget: " + budget.Reason}}
	case domain != nil && !domain.Allow:
		return Verdict{false, nil, []string{"failure-domain: " + domain.Reason}}
	case stage.Waiting:
		return Verdict{true, []string{}, []string{"staged: " + stage.Reason}}
	case stage.Done:
		return Verdict{true, []string{}, []string{"staged: all targets applied"}}
	}
	var reasons []string
	if stage.Reason != "" {
		reasons = []string{"staged: " + stage.Reason}
	}
	return Verdict{true, append([]string(nil), stage.Batch...), reasons}
}
