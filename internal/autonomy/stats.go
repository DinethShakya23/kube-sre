package autonomy

import (
	"fmt"
	"math"
	"sort"
)

// The Wilson lower bound criterion for autonomy promotion. Pure, deterministic and
// reproducible by an auditor: an action class is promoted a rung only when the one
// sided 95% Wilson score lower confidence bound of its transition metric over a
// rolling window clears the per transition threshold and the minimum count, minimum
// time, diversity and zero critical gates all pass. No model, no database and no
// wall clock inside the math: a timeline is passed in. Nothing here promotes
// anything on its own; it is the decision function the trust plane calls.

const z95 = 1.6448536269514722 // one sided, alpha 0.05

const (
	WindowMaxEvents = 200
	WindowMaxDays   = 90.0
)

// TransitionRule is the bar for one rung transition. A nil Theta means never.
type TransitionRule struct {
	Theta        *float64
	NMin         int
	TMinDays     int
	MinIncidents int
	MinTypes     int
}

func f(v float64) *float64 { return &v }

var rules = map[string]TransitionRule{
	"L1->L2":                    {f(0.95), 20, 7, 5, 1},
	"L2->L3":                    {f(0.90), 30, 14, 10, 3},
	"L3->L4:versioned-workload": {f(0.95), 60, 30, 15, 1},
	"L3->L4:declarative-revert": {f(0.99), 300, 90, 30, 1},
	"L3->L4:irreversible":       {nil, 0, 0, 0, 0}, // a hard ceiling
}

// Event is one outcome of an action class attempt.
type Event struct {
	TSDays       float64 // monotonic day offset, no wall clock in the math
	Success      bool    // the transition metric numerator
	IncidentID   string
	IncidentType string
	Critical     bool // a Sev-1 or Sev-2 attributed to this action
	Offline      bool // derived from offline analysis, down weighted against a live shadow run
}

func (e Event) kind() string {
	if e.IncidentType == "" {
		return "generic"
	}
	return e.IncidentType
}

// PromotionDecision is the verdict on a promotion. Reasons lists every failed gate.
type PromotionDecision struct {
	Promote    bool
	Transition string
	LCB        float64
	N          int
	Theta      *float64
	Reasons    []string
}

// WilsonLCB is the one sided lower Wilson bound for a proportion; n <= 0 gives 0. It
// takes float counts so it also serves the offline weighted variant.
func WilsonLCB(successes, n float64) float64 { return wilson(successes, n, z95) }

func wilson(successes, n, z float64) float64 {
	if n <= 0 {
		return 0
	}
	phat := successes / n
	denom := 1 + z*z/n
	center := phat + z*z/(2*n)
	margin := z * math.Sqrt((phat*(1-phat)+z*z/(4*n))/n)
	return math.Max(0, (center-margin)/denom)
}

// CalibrateOfflineWeight derives the offline shadow weight from matched
// (offline, live) verdict pairs: the agreement rate, clamped to cap, so offline
// evidence never outweighs live. No pairs means no trust in offline at all.
func CalibrateOfflineWeight(matched [][2]bool, cap float64) float64 {
	if len(matched) == 0 {
		return 0
	}
	agree := 0
	for _, m := range matched {
		if m[0] == m[1] {
			agree++
		}
	}
	return math.Max(0, math.Min(cap, float64(agree)/float64(len(matched))))
}

// WeightedLCB counts offline derived events at offlineWeight and live ones at 1, so
// more offline evidence gives a lower effective bound, which is less certainty.
func WeightedLCB(events []Event, offlineWeight float64) float64 {
	w := math.Max(0, math.Min(1, offlineWeight))
	var n, s float64
	for _, e := range events {
		c := 1.0
		if e.Offline {
			c = w
		}
		n += c
		if e.Success {
			s += c
		}
	}
	return wilson(s, n, z95)
}

func window(events []Event, nowDays float64) []Event {
	var recent []Event
	for _, e := range events {
		if nowDays-e.TSDays <= WindowMaxDays {
			recent = append(recent, e)
		}
	}
	sort.SliceStable(recent, func(i, j int) bool { return recent[i].TSDays < recent[j].TSDays })
	if len(recent) > WindowMaxEvents {
		recent = recent[len(recent)-WindowMaxEvents:]
	}
	return recent
}

// RuleFor returns the rule for a transition.
func RuleFor(transition string) (TransitionRule, error) {
	r, ok := rules[transition]
	if !ok {
		return r, fmt.Errorf("unknown transition %q", transition)
	}
	return r, nil
}

// EvaluatePromotion decides whether a transition may fire, given the timeline as of
// nowDays. Every gate must pass.
func EvaluatePromotion(transition string, events []Event, nowDays float64) (PromotionDecision, error) {
	rule, err := RuleFor(transition)
	if err != nil {
		return PromotionDecision{}, err
	}
	win := window(events, nowDays)
	n := len(win)
	succ := 0
	for _, e := range win {
		if e.Success {
			succ++
		}
	}
	lcb := WilsonLCB(float64(succ), float64(n))
	if rule.Theta == nil {
		return PromotionDecision{false, transition, lcb, n, nil, []string{"irreversible: hard ceiling, never auto-promoted"}}, nil
	}
	var reasons []string
	if lcb < *rule.Theta {
		reasons = append(reasons, fmt.Sprintf("LCB %.4f < θ %v", lcb, *rule.Theta))
	}
	if n < rule.NMin {
		reasons = append(reasons, fmt.Sprintf("n %d < n_min %d", n, rule.NMin))
	}
	span := 0.0
	if n >= 2 {
		span = win[n-1].TSDays - win[0].TSDays
	}
	if span < float64(rule.TMinDays) {
		reasons = append(reasons, fmt.Sprintf("time span %.1fd < T_min %dd", span, rule.TMinDays))
	}
	incidents, types := map[string]bool{}, map[string]bool{}
	critical := false
	for _, e := range win {
		incidents[e.IncidentID] = true
		types[e.kind()] = true
		critical = critical || e.Critical
	}
	if len(incidents) < rule.MinIncidents {
		reasons = append(reasons, fmt.Sprintf("distinct incidents %d < %d", len(incidents), rule.MinIncidents))
	}
	if len(types) < rule.MinTypes {
		reasons = append(reasons, fmt.Sprintf("distinct types %d < %d", len(types), rule.MinTypes))
	}
	if critical {
		reasons = append(reasons, "M4 > 0: a critical event occurred in the window")
	}
	return PromotionDecision{len(reasons) == 0, transition, lcb, n, rule.Theta, reasons}, nil
}

// Demotion is automatic and asymmetric: fast down, slow up. Promotion earns rungs
// slowly; demotion drops them quickly and without approval. These are the five
// triggers, deterministic and recomputable from the same events.

var Rungs = []string{"L0", "L1", "L2", "L3", "L4"}

const (
	HysteresisLastN    = 50
	HysteresisBand     = 0.05
	CusumFails         = 2
	CusumWindowDays    = 1.0
	SevAttributionDrop = 2
	FleetFreezeDays    = 14
)

func rungIndex(r string) (int, error) {
	for i, x := range Rungs {
		if x == r {
			return i, nil
		}
	}
	return 0, fmt.Errorf("unknown rung %q", r)
}

func demoteTo(rung string, down int) string {
	i, err := rungIndex(rung)
	if err != nil {
		return rung
	}
	return Rungs[max(0, i-down)]
}

// DemotionDecision is the most severe demotion action that fires, or none.
type DemotionDecision struct {
	Demote          bool
	From, To        string
	Reason          string
	FleetFreezeDays int
	Stale           bool // class drift flags the rung for re-qualification
}

// CusumTrip is true when fails postcondition failures fall inside any windowDays
// window. It is checked on raw events, not the rolling window: do not wait for it.
//
// Every failure counts, including one that also caused a critical incident. Excluding
// those made the worst failures the only ones the fast trigger could not see: an L3
// class with 48 clean runs and two failures 12 hours apart dropped to L2, while the
// same two events marked critical left it at L3. Promotion already treats a critical
// as disqualifying, so demotion must not treat it as exculpatory.
func CusumTrip(events []Event, fails int, windowDays float64) bool {
	var ts []float64
	for _, e := range events {
		if !e.Success {
			ts = append(ts, e.TSDays)
		}
	}
	sort.Float64s(ts)
	for i := 0; i+fails <= len(ts); i++ {
		if ts[i+fails-1]-ts[i] <= windowDays {
			return true
		}
	}
	return false
}

// HysteresisBreach is true when the bound over the last lastN events has fallen
// below theta - band. The word doing the work is fallen. A Wilson bound is driven by
// the failure rate and the sample size, and below a certain n the second dominates so
// completely that the band cannot be cleared by any record at all: at theta 0.95 a
// flawless 15 for 15 record breached the band, and the class was demoted with an
// audit line saying its agreement had fallen, about a class that never failed once.
// So the band only trips where a breach can be attributed to failures: if a perfect
// record at this n would still sit under the floor, this trigger abstains. The fast
// trigger does not depend on n and still fires.
func HysteresisBreach(events []Event, theta float64, lastN int, band float64) bool {
	win := append([]Event(nil), events...)
	sort.SliceStable(win, func(i, j int) bool { return win[i].TSDays < win[j].TSDays })
	if len(win) > lastN {
		win = win[len(win)-lastN:]
	}
	if len(win) == 0 {
		return false
	}
	floor := theta - band
	if WilsonLCB(float64(len(win)), float64(len(win))) < floor {
		return false
	}
	succ := 0
	for _, e := range win {
		if e.Success {
			succ++
		}
	}
	return WilsonLCB(float64(succ), float64(len(win))) < floor
}

// EvaluateDemotion returns the single most severe demotion action that fires.
// Precedence mirrors the design: severity attribution (two rungs and a freeze), then
// a critical event at L4, then class drift, then CUSUM, then hysteresis.
func EvaluateDemotion(current string, theta float64, events []Event, nowDays float64, sevAttributed, m4AtL4, classDrift bool) DemotionDecision {
	atL4 := current == "L4"
	switch {
	case sevAttributed && atL4:
		return DemotionDecision{true, current, demoteTo(current, SevAttributionDrop),
			"Sev-1/2 attributed to an L4 action: 2-rung drop + fleet freeze", FleetFreezeDays, false}
	case m4AtL4 && atL4:
		return DemotionDecision{true, current, "L3", "M4 critical event at L4: immediate demote", 0, false}
	case classDrift:
		return DemotionDecision{false, current, current, "class-definition drift: rung flagged stale, re-qualification required", 0, true}
	}
	win := window(events, nowDays)
	if CusumTrip(win, CusumFails, CusumWindowDays) {
		// The action is one rung down either way; the reason still has to say what
		// happened, or the audit trail records a routine trip where a critical
		// incident occurred.
		sev := ""
		for _, e := range win {
			if e.Critical && !e.Success {
				sev = " (at least one caused a critical incident)"
				break
			}
		}
		return DemotionDecision{true, current, demoteTo(current, 1), fmt.Sprintf("CUSUM: %d postcondition failures within 24 h%s", CusumFails, sev), 0, false}
	}
	if HysteresisBreach(win, theta, HysteresisLastN, HysteresisBand) {
		return DemotionDecision{true, current, demoteTo(current, 1), fmt.Sprintf("hysteresis: last-%d LCB < θ − %v", HysteresisLastN, HysteresisBand), 0, false}
	}
	if sevAttributed || m4AtL4 {
		// Those two triggers are scoped to L4, so this must not invent a policy for
		// other rungs. What it must not do is discard the caller's severity signal in
		// silence: "no demotion trigger" would report a quiet class while a Sev-1 was
		// attributed to it.
		which := ""
		if sevAttributed {
			which = "Sev-1/2 attribution"
		}
		if m4AtL4 {
			if which != "" {
				which += " and "
			}
			which += "M4 critical"
		}
		return DemotionDecision{false, current, current, fmt.Sprintf("%s reported at %s, where this trigger is scoped to L4; no other trigger fired — "+
			"this is NOT a clean class, escalate for manual review", which, current), 0, false}
	}
	return DemotionDecision{false, current, current, "no demotion trigger", 0, false}
}
