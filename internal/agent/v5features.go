package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/cortex"
	"github.com/DinethShakya23/kube-sre/internal/events"
)

// The opt in features from the cortex package, wired at the points the agent already
// has: the coordinator prompt, the investigation fan out and the synthesis. All of them
// sit behind CORTEX_V5_ENABLED and their own flag, and with every flag off the agent is
// byte identical to before.

func (a *Agent) v5(flag bool) bool { return a.Cfg.CortexV5 && flag }

// skillsBlock is the matched runbooks as on demand skills for the coordinator prompt.
func (a *Agent) skillsBlock(st *State) string {
	if !a.v5(a.Cfg.V5RunbookSkills) || len(st.MatchedPlaybooks) == 0 {
		return ""
	}
	return cortex.RenderMatchedSkills(a.Playbooks, st.MatchedPlaybooks, 5)
}

// harnessFanout runs the plan step investigators, read only through the ACI verbs, in
// place of the four fixed specialists. It reports false when the feature is off or
// every investigator came back empty, so the caller falls back to the specialists and
// enabling the flag can never leave an investigation with no evidence.
func (a *Agent) harnessFanout(ctx context.Context, st *State, query string) bool {
	if !a.v5(a.Cfg.V5HarnessFanout) || a.ACI == nil {
		return false
	}
	var steps []string
	for _, s := range st.Plan {
		if s.Status == "pending" || s.Status == "in_progress" {
			steps = append(steps, s.Description)
		}
	}
	contracts := cortex.PlanSubagents(steps, query, a.Cfg.V5HarnessMaxSubagents)
	if len(contracts) == 0 {
		return false
	}
	model := a.Subagent
	if a.Cfg.V5HarnessLargeModel {
		model = a.Coordinator // a larger tier: small models mis-investigate
	}
	a.emit(st.SessionID, events.NewStatus(st.SessionID, "investigating", fmt.Sprintf("Dispatching %d read-only investigators…", len(contracts))))
	_, results, usable := cortex.Fanout(ctx, model, *a.ACI, contracts, st.ClusterSnapshot, a.Cfg.V5HarnessMaxRounds)
	if !usable {
		return false
	}
	findings := make([]Finding, 0, len(results))
	for _, r := range results {
		findings = append(findings, Finding{Domain: "investigation", Hypothesis: strings.TrimSpace(r.Summary), Confidence: 0.5,
			Evidence: []string{r.Objective}, ToolCallsMade: r.VerbsUsed})
	}
	st.Findings, st.RCARequired = findings, false
	return true
}

// heartbeat emits a progress status every V5HeartbeatSeconds while a slow phase runs,
// so the stream never goes quiet for longer than that. With the flag off it is a no op.
func (a *Agent) heartbeat(ctx context.Context, session, phase, message string) (stop func()) {
	if !a.v5(a.Cfg.V5Responsiveness) {
		return func() {}
	}
	return cortex.Heartbeat(ctx, time.Duration(a.Cfg.V5HeartbeatSeconds*float64(time.Second)), func() {
		a.emit(session, events.NewStatus(session, phase, message))
	})
}

func findingsEvidence(st *State) string {
	var parts []string
	for _, f := range st.Findings {
		parts = append(parts, fmt.Sprintf("[%s] %s\n%s", f.Domain, f.Hypothesis, strings.Join(f.Evidence, "\n")))
	}
	return strings.Join(parts, "\n\n")
}

// afterSynthesis appends the verification note, the responder brief and the latency
// budget warning to the answer, in that order. Each is a caveat or an aid and never a
// replacement: the answer itself is not changed, and none of them can fail the turn.
func (a *Agent) afterSynthesis(ctx context.Context, st *State, rca *RCAResult, summary string, budget *cortex.PhaseBudget) string {
	if budget != nil {
		budget.MarkFirstSignal()
	}
	evidence := findingsEvidence(st)
	if a.v5(a.Cfg.V5VerifyLadder) {
		summary += cortex.RenderReviewNote(cortex.ReviewRCA(ctx, a.Subagent, summary, evidence))
	}
	if a.v5(a.Cfg.V5EscalationBriefs) {
		summary += cortex.RenderBrief(cortex.BuildBrief(ctx, a.Subagent, rca.RootCause, evidence, a.Cfg.V5ResponderLevel))
	}
	if budget != nil {
		if w := budget.Warning(); w != "" {
			slog.Warn("investigation over its latency budget", "session", st.SessionID, "warning", w)
			summary += "\n\n_⏱ Latency budget: " + w + "._"
		}
	}
	return summary
}

// newBudget starts the latency tracker when responsiveness is on.
func (a *Agent) newBudget() *cortex.PhaseBudget {
	if !a.v5(a.Cfg.V5Responsiveness) {
		return nil
	}
	return cortex.NewPhaseBudget(time.Duration(a.Cfg.V5FirstSignalBudgetS*float64(time.Second)), time.Duration(a.Cfg.V5FullBudgetS*float64(time.Second)), a.Now)
}
