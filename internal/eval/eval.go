// Package eval is the deterministic core of the evaluation work. The live fault
// injection driver needs a cluster; this is the pure spine under it: the disclosure
// record every reported number ships with, a content addressed snapshot hash, the
// separability gate, and the security outcome gate for fix pull requests.
package eval

import (
	"encoding/json"
	"errors"
	"math"
	"sort"

	"github.com/DinethShakya23/kube-sre/internal/recorder"
)

// ── security outcome ─────────────────────────────────────────────────────────
//
// The gate that makes the write approaches shippable and not merely claimed: a fix
// pull request is only promotable if it introduces zero new policy violations and does
// not regress the security posture. Set arithmetic over violation identifiers taken
// before and after the change, with no model, reproducible by an auditor. A positive
// outcome is a promotion signal; any introduced violation is a hard blocker.

// SecurityOutcome is what a change introduced and resolved.
type SecurityOutcome struct {
	Introduced []string // violations the change added (bad)
	Resolved   []string // violations the change removed (good)
}

// NetDelta is the change in violation count; negative is a net improvement.
func (o SecurityOutcome) NetDelta() int { return len(o.Introduced) - len(o.Resolved) }

// Clean is the hard promotion precondition: no new violations.
func (o SecurityOutcome) Clean() bool { return len(o.Introduced) == 0 }

// ScoreSecurityOutcome diffs the violation sets.
func ScoreSecurityOutcome(before, after []string) SecurityOutcome {
	b, a := map[string]bool{}, map[string]bool{}
	for _, v := range before {
		b[v] = true
	}
	for _, v := range after {
		a[v] = true
	}
	out := SecurityOutcome{Introduced: []string{}, Resolved: []string{}}
	for v := range a {
		if !b[v] {
			out.Introduced = append(out.Introduced, v)
		}
	}
	for v := range b {
		if !a[v] {
			out.Resolved = append(out.Resolved, v)
		}
	}
	sort.Strings(out.Introduced)
	sort.Strings(out.Resolved)
	return out
}

// GatesPromotion says whether a change class may be promoted. It always requires zero
// introduced violations; with requireNetImprovement it also demands a net reduction.
func GatesPromotion(o SecurityOutcome, requireNetImprovement bool) bool {
	if !o.Clean() {
		return false
	}
	return !requireNetImprovement || o.NetDelta() < 0
}

// Aggregate folds a corpus of outcomes into one.
func Aggregate(outcomes []SecurityOutcome) SecurityOutcome {
	out := SecurityOutcome{Introduced: []string{}, Resolved: []string{}}
	for _, o := range outcomes {
		out.Introduced = append(out.Introduced, o.Introduced...)
		out.Resolved = append(out.Resolved, o.Resolved...)
	}
	sort.Strings(out.Introduced)
	sort.Strings(out.Resolved)
	return out
}

// ── the benchmark core ───────────────────────────────────────────────────────

const z95 = 1.6448536269514722

// HarnessSpec is the mandatory disclosure of the harness a number was produced under.
// Token and harness choices explain most of the variance between reported results, so a
// number without its harness is uninterpretable.
type HarnessSpec struct {
	Model           string   `json:"model"`
	MaxGatherRounds int      `json:"max_gather_rounds"`
	ToolSurface     string   `json:"tool_surface"` // aci-v0 | kubectl-flat
	MemoryFlags     []string `json:"memory_flags"`
	ReplayFidelity  string   `json:"replay_fidelity"` // full | environment | live
	Seed            int      `json:"seed"`
}

func (h HarnessSpec) IsComplete() bool {
	return h.Model != "" && h.ToolSurface != "" && h.ReplayFidelity != ""
}

// ScoreCard is one graded run's metrics.
type ScoreCard struct {
	M1Localized float64 `json:"m1_localized"` // correct root cause localisation rate
	M2Resolved  float64 `json:"m2_resolved"`  // postcondition verified resolution rate
	Tokens      int     `json:"tokens"`
}

// SnapshotManifestHash is a content addressed hash of a snapshot's layer digests,
// reusing the flight recorder's hash so the same tamper evidence applies: any changed
// layer byte changes the manifest hash and forces a replay mismatch.
func SnapshotManifestHash(layers map[string]string) string {
	canonical, _ := json.Marshal(layers) // map keys are sorted
	return recorder.ComputeHash("", "opsmembench", 0, "snapshot_manifest", map[string]any{"canonical": string(canonical)})
}

// Grade is a scorecard with its harness and snapshot identity.
type Grade struct {
	Scorecard    ScoreCard   `json:"scorecard"`
	Harness      HarnessSpec `json:"harness"`
	ManifestHash string      `json:"manifest_hash"`
	Notes        []string    `json:"notes"`
}

// Validate refuses a grade whose harness disclosure is incomplete: it is not shippable.
func (g Grade) Validate() error {
	if !g.Harness.IsComplete() {
		return errors.New("a grade requires a complete HarnessSpec (disclosure standard)")
	}
	return nil
}

// BuildGrade assembles a grade and validates it.
func BuildGrade(sc ScoreCard, h HarnessSpec, layers map[string]string, notes []string) (Grade, error) {
	if notes == nil {
		notes = []string{}
	}
	g := Grade{Scorecard: sc, Harness: h, ManifestHash: SnapshotManifestHash(layers), Notes: notes}
	return g, g.Validate()
}

// Separability is the outcome of the separability gate.
type Separability struct {
	Effect     float64 `json:"effect"`
	HarnessStd float64 `json:"harness_std"`
	Separable  bool    `json:"separable"`
	Verdict    string  `json:"verdict"` // proven | unproven
	Detail     string  `json:"detail"`
}

// Decompose is the separability gate: a claimed effect is proven only if it exceeds
// z times the harness standard deviation. Otherwise it cannot be told apart from
// harness noise and is unproven.
func Decompose(effect, harnessVariance float64) Separability {
	std := math.Sqrt(math.Max(harnessVariance, 0))
	threshold := z95 * std
	sep := math.Abs(effect) > threshold
	verdict, op := "unproven", "<="
	if sep {
		verdict, op = "proven", ">"
	}
	return Separability{effect, std, sep, verdict, fmtDetail(math.Abs(effect), op, threshold)}
}

func fmtDetail(effect float64, op string, threshold float64) string {
	return "|effect|=" + f4(effect) + " " + op + " z·σ_harness=" + f4(threshold)
}

func f4(v float64) string {
	b, _ := json.Marshal(math.Round(v*1e4) / 1e4)
	s := string(b)
	// four decimals, as the report format expects
	dot := -1
	for i, c := range s {
		if c == '.' {
			dot = i
		}
	}
	if dot < 0 {
		return s + ".0000"
	}
	for len(s)-dot-1 < 4 {
		s += "0"
	}
	return s
}
