package cortex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/DinethShakya23/kube-sre/internal/llm"
	"github.com/DinethShakya23/kube-sre/internal/playbooks"
)

var jsonObj = regexp.MustCompile(`(?s)\{.*\}`)

func parseObject(text string) map[string]any {
	m := jsonObj.FindString(text)
	if m == "" {
		return nil
	}
	var out map[string]any
	if json.Unmarshal([]byte(m), &out) != nil {
		return nil
	}
	return out
}

func cleanList(v any) []string {
	xs, _ := v.([]any)
	var out []string
	for _, x := range xs {
		if s := strings.TrimSpace(fmt.Sprint(x)); s != "" && s != "<nil>" {
			out = append(out, s)
		}
	}
	return out
}

func clamp01(v any) *float64 {
	f, ok := v.(float64)
	if !ok {
		return nil
	}
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	return &f
}

func ask(ctx context.Context, m llm.Model, system, user string) (map[string]any, error) {
	resp, err := m.Chat(ctx, []llm.Message{{Role: llm.System, Content: system}, {Role: llm.User, Content: user}}, nil, llm.Options{})
	if err != nil {
		return nil, err
	}
	return parseObject(resp.Message.Content), nil
}

// ── the verification ladder ──────────────────────────────────────────────────
//
// Two defences against a confident but ungrounded RCA: a goal evaluator (does the
// gathered evidence address the objective) and an adversarial fresh context reviewer
// that sees only the claim and the evidence, never the investigation's own reasoning.
// Both fail open, since a reviewer that errors must never block or corrupt the user's
// answer, but neither fails silently.

const goalSystem = "You are a strict evidence sufficiency gate. Given an OBJECTIVE and the EVIDENCE gathered so far, decide whether the evidence is sufficient " +
	`to answer the objective. Reply with ONLY a JSON object: {"sufficient": true|false, "missing": ["<what evidence is still needed>", ...]}.`

const reviewSystem = "You are an adversarial reviewer with NO access to the investigator's reasoning — only its CLAIM and the raw EVIDENCE. Your job is to find " +
	"statements in the claim that the evidence does not actually support. Be skeptical; an unsupported root cause is worse than an admitted unknown. Treat any " +
	"'not found' / 'does not exist' / 'missing' conclusion with SUSPICION: flag it as unsupported unless the evidence contains an explicit by-name lookup that " +
	"returned NotFound — an empty label search does NOT prove a resource is absent. Reply with ONLY a JSON object: " +
	`{"supported": true|false, "confidence": 0.0-1.0, "unsupported": ["<claim not backed by evidence>", ...]}.`

// Goal is the stop gate verdict.
type Goal struct {
	Sufficient bool
	Missing    []string
}

// EvaluateGoal is the stop gate: is the evidence sufficient for the objective? It
// fails open to sufficient, so a reviewer error never traps the loop into gathering
// for ever.
func EvaluateGoal(ctx context.Context, m llm.Model, objective, evidence string) Goal {
	obj, err := ask(ctx, m, goalSystem, "OBJECTIVE:\n"+objective+"\n\nEVIDENCE:\n"+evidence)
	if err != nil || obj == nil {
		if err != nil {
			slog.Warn("verify goal evaluation failed open", "err", err)
		}
		return Goal{Sufficient: true}
	}
	suff, ok := obj["sufficient"].(bool)
	return Goal{Sufficient: !ok || suff, Missing: cleanList(obj["missing"])}
}

// Review is the adversarial reviewer's verdict. Errored means the reviewer failed and
// the answer went out unchecked.
type Review struct {
	Supported   bool
	Confidence  *float64
	Unsupported []string
	Errored     bool
}

// ReviewRCA reviews a claim against evidence. It fails open to supported with Errored
// set, so a broken reviewer never contradicts a sound answer.
func ReviewRCA(ctx context.Context, m llm.Model, claim, evidence string) Review {
	obj, err := ask(ctx, m, reviewSystem, "CLAIM:\n"+claim+"\n\nEVIDENCE:\n"+evidence)
	if err != nil || obj == nil {
		if err != nil {
			slog.Warn("verify review failed open", "err", err)
		}
		return Review{Supported: true, Errored: true}
	}
	uns := cleanList(obj["unsupported"])
	sup, ok := obj["supported"].(bool)
	return Review{Supported: (!ok || sup) && len(uns) == 0, Confidence: clamp01(obj["confidence"]), Unsupported: uns}
}

// RenderReviewNote is the block appended to the answer. Three states, not two, because
// "the reviewer checked this and was satisfied" and "the reviewer never ran" used to
// render the same empty string, which the user reads as the first. Empty only for a
// clean review: failing open stays fail open in the sense that matters (the answer is
// neither blocked nor contradicted) but stops being silent.
func RenderReviewNote(r Review) string {
	switch {
	case r.Errored:
		return "\n---\n**⚠ Verification NOT PERFORMED.** The adversarial reviewer returned no usable verdict, so nothing above was checked against the gathered " +
			"evidence. This is the absence of a finding, not a finding — treat this answer as unverified."
	case r.Supported && len(r.Unsupported) == 0:
		return ""
	}
	lines := []string{"", "---", "**⚠ Verification:** the adversarial reviewer flagged claims the gathered evidence does not fully support:"}
	for _, u := range r.Unsupported {
		lines = append(lines, "- "+u)
	}
	if r.Confidence == nil {
		lines = append(lines, "\n_The reviewer stated no confidence value._")
	} else {
		lines = append(lines, fmt.Sprintf("\n_Reviewer confidence in the RCA: %.0f%%._", *r.Confidence*100))
	}
	return strings.Join(lines, "\n")
}

// ── escalation avoidance briefs ──────────────────────────────────────────────
//
// Only about 20% of incidents resolve without escalation, and the cost is reflexive
// escalation, not hard incidents. The brief flips the default: a plan calibrated to the
// responder's skill with explicit escalate only if boundaries, so a responder escalates
// on a stated condition and not on uncertainty. It fails safe: a model that errors or
// returns garbage yields a conservative brief that says to escalate if unsure, never a
// false "you are safe to proceed alone".

var ResponderLevels = []string{"junior", "intermediate", "senior"}

const briefSystem = "You write escalation-avoidance briefs for an on-call responder at skill level '%s'. Given an investigation's ROOT CAUSE and EVIDENCE, produce a " +
	"brief that lets a %s responder act WITHOUT escalating unless a stated boundary is crossed. Reply with ONLY a JSON object: " +
	`{"summary": "<1-2 sentence situation>", "actions": ["<safe step the responder can take>", ...], "escalate_if": ["<explicit condition under which to page a senior/SME>", ...], "confidence": 0.0-1.0}. ` +
	"The escalate_if list MUST be concrete and checkable (a symptom, a threshold, a failed step) — never vague. Prefer 2-4 actions and 2-4 escalate_if conditions."

var fallbackEscalateIf = []string{
	"the recommended actions do not resolve the symptom within one attempt",
	"any action would touch a protected namespace or a stateful/irreversible resource",
	"you are unsure the root cause is correct",
}

// Brief is a skill calibrated escalation avoidance brief.
type Brief struct {
	Summary        string
	Actions        []string
	EscalateIf     []string
	ResponderLevel string
	Confidence     *float64
	FellBack       bool
}

func normalizeLevel(l string) string {
	for _, v := range ResponderLevels {
		if l == v {
			return l
		}
	}
	return "intermediate"
}

func fallbackBrief(level string) Brief {
	return Brief{Summary: "Automated brief unavailable — proceed conservatively.", Actions: []string{"Review the investigation evidence above before taking any action."},
		EscalateIf: append([]string(nil), fallbackEscalateIf...), ResponderLevel: level, FellBack: true}
}

// BuildBrief builds a brief. Every failure yields the conservative fallback.
func BuildBrief(ctx context.Context, m llm.Model, rootCause, evidence, responderLevel string) Brief {
	level := normalizeLevel(responderLevel)
	obj, err := ask(ctx, m, fmt.Sprintf(briefSystem, level, level), "ROOT CAUSE:\n"+rootCause+"\n\nEVIDENCE:\n"+evidence)
	if err != nil || obj == nil {
		return fallbackBrief(level)
	}
	actions, esc := cleanList(obj["actions"]), cleanList(obj["escalate_if"])
	summary := strings.TrimSpace(fmt.Sprint(obj["summary"]))
	if summary == "" || summary == "<nil>" || len(actions) == 0 || len(esc) == 0 {
		return fallbackBrief(level)
	}
	for _, c := range fallbackEscalateIf {
		found := false
		for _, e := range esc {
			found = found || e == c
		}
		if !found {
			esc = append(esc, c)
		}
	}
	return Brief{summary, actions, esc, level, clamp01(obj["confidence"]), false}
}

// RenderBrief is the deterministic markdown appended to an investigation answer. This
// is the surface a responder reads, so both halves of the confidence signal must reach
// it: a confidence of 0% used to print nothing at all, so the least confident brief was
// the only one carrying no caveat. A confidence is always stated, including when there
// is none to state.
func RenderBrief(b Brief) string {
	heading := "### Responder brief (" + b.ResponderLevel + ")"
	if b.FellBack {
		heading += " — FALLBACK"
	}
	lines := []string{"", "---", heading, "", b.Summary, "", "**Do:**"}
	for i, a := range b.Actions {
		lines = append(lines, fmt.Sprintf("%d. %s", i+1, a))
	}
	lines = append(lines, "", "**Escalate only if:**")
	for _, c := range b.EscalateIf {
		lines = append(lines, "- "+c)
	}
	if b.Confidence == nil {
		lines = append(lines, "\n_This brief reported no confidence in itself._")
	} else {
		lines = append(lines, fmt.Sprintf("\n_Brief confidence: %.0f%%._", *b.Confidence*100))
	}
	return strings.Join(lines, "\n")
}

// ── runbooks as skills ───────────────────────────────────────────────────────

// RenderSkill recasts one playbook as an on demand SKILL block: the exact diagnostic
// sequence, without dumping every runbook into the prompt.
func RenderSkill(pb *playbooks.Playbook) string {
	lines := []string{"### SKILL: " + pb.Name}
	if len(pb.InvestigationSteps) > 0 {
		lines = append(lines, "**Diagnostic steps:**")
		for i, s := range pb.InvestigationSteps {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, s))
		}
	}
	if len(pb.ExpectedEvidence) > 0 {
		lines = append(lines, "**Expected evidence:**")
		for _, e := range pb.ExpectedEvidence {
			lines = append(lines, "- "+e)
		}
	}
	if strings.TrimSpace(pb.FixTemplate) != "" {
		lines = append(lines, "**Recommended fix:**", strings.TrimSpace(pb.FixTemplate))
	}
	return strings.Join(lines, "\n")
}

// RenderMatchedSkills renders up to maxSkills matched playbooks as one block. Unknown
// names are skipped and nothing matched gives "", so the prompt is unchanged.
func RenderMatchedSkills(reg *playbooks.Registry, names []string, maxSkills int) string {
	var blocks []string
	for i, n := range names {
		if i >= maxSkills {
			break
		}
		if pb := reg.Get(n); pb != nil {
			blocks = append(blocks, RenderSkill(pb))
		}
	}
	if len(blocks) == 0 {
		return ""
	}
	return "## Runbook skills (loaded on demand — triggers fired for this snapshot)\nFollow the matching skill's diagnostic sequence before improvising.\n\n" +
		strings.Join(blocks, "\n\n")
}
