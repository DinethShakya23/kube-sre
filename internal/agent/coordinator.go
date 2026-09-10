package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/change"
	"github.com/DinethShakya23/kube-sre/internal/events"
	"github.com/DinethShakya23/kube-sre/internal/kube"
	"github.com/DinethShakya23/kube-sre/internal/llm"
	"github.com/DinethShakya23/kube-sre/internal/nsguard"
)

var targetedRe = regexp.MustCompile(`(?i)TARGETED:\s*namespace\s*=\s*(\S+?),\s*pod\s*=\s*(\S+?),\s*issue\s*=\s*(.+)`)

// Every recursion unit is roughly one third of a step: a model call, a tool call
// and a tool result.
const unitsPerStep = 3

func (a *Agent) emit(session string, ev events.Event) {
	if a.Emitter != nil {
		a.Emitter.Emit(session, ev)
	}
}

func (a *Agent) callFor(st *State, bypass bool) kube.Call {
	return kube.Call{Role: st.UserRole, HitlBypass: bypass, SessionID: st.SessionID}
}

// hooks turns a tool batch into events.
func (a *Agent) hooks(session string) batchHooks {
	return batchHooks{
		onStart: func(c llm.ToolCall) { a.emit(session, events.NewToolCall(session, c.Name, callInfo(c))) },
		onEnd: func(c llm.ToolCall, out string) {
			a.emit(session, events.NewToolResult(session, c.Name, clip(out, 500)))
		},
	}
}

// directResult is the outcome of the coordinator's tool loop.
type directResult struct {
	Messages []llm.Message
	Paused   bool
	Plan     []PlanStep
}

// systemPrompt assembles the coordinator prompt for a turn.
func (a *Agent) systemPrompt(st *State, bypass bool, historySummary string) string {
	parts := []string{coordinatorSystem}
	if a.Cfg.InvestigationPlan {
		parts = append(parts, planPromptBlock)
	}
	if st.MemoryContext != "" {
		parts = append(parts, "\n\n## Cluster Context\n"+st.MemoryContext)
	}
	if st.ClusterSnapshot != "" {
		parts = append(parts, "\n\n"+st.ClusterSnapshot)
	}
	if b := snapshotSufficiencyBlock(st, a.Cfg.SnapshotMode, a.Cfg.SnapshotFreshness, a.Now()); b != "" {
		parts = append(parts, b)
	}
	if a.Cfg.Playbooks {
		if b := playbooksBlock(a.Playbooks, st.MatchedPlaybooks); b != "" {
			parts = append(parts, b)
		}
	}
	if a.Cfg.CortexV5 && a.Cfg.ChangeFirstRCA && a.Changes != nil {
		if prior := change.RenderPrior(a.Changes.Recent(st.ClusterID, ""), 5, 0); prior != "" {
			parts = append(parts, "\n\n"+prior)
		}
	}
	if bypass {
		parts = append(parts, proactiveFixBlock)
	}
	if historySummary != "" {
		parts = append(parts, "\n\n"+historySummary)
	}
	return strings.Join(parts, "\n")
}

// isSentinel reports whether a reply is a routing decision rather than an answer.
func isSentinel(content string) bool {
	c := strings.TrimSpace(content)
	return c == "RCA_REQUIRED" || targetedRe.MatchString(c)
}

// directAnswer runs the coordinator's model and tool loop. With res set it resumes
// a loop that paused for approval instead of starting one.
//
// The loop is bounded explicitly. Without a bound it would inherit an effectively
// unlimited one, and this is the loop that holds the write capable toolset. On
// exhaustion it halts and escalates with what was found; it never truncates silently.
func (a *Agent) directAnswer(ctx context.Context, st *State, bypass bool, res *bool) (*directResult, error) {
	history, summary := trimSession(st.Messages)
	system := llm.Message{Role: llm.System, Content: a.systemPrompt(st, bypass, summary)}
	call := a.callFor(st, bypass)
	specs := a.Tools.Specs()
	maxRounds := a.Cfg.CoordinatorRecursions / unitsPerStep
	hooks := a.hooks(st.SessionID)

	var loop []llm.Message
	rounds := 0
	if res != nil && st.Pending != nil {
		p := st.Pending
		loop, rounds = append([]llm.Message(nil), p.Loop...), p.Rounds
		loop = append(loop, a.Tools.resolvePending(ctx, p.Call, *res, call, hooks))
		st.Pending = nil
	}

	for {
		if rounds >= maxRounds {
			slog.Error("coordinator tool call budget exhausted", "session", st.SessionID, "limit", a.Cfg.CoordinatorRecursions)
			msg := fmt.Sprintf(budgetExhaustedMessage, a.Cfg.CoordinatorRecursions)
			a.emit(st.SessionID, events.NewToken(st.SessionID, msg))
			return &directResult{Messages: []llm.Message{{Role: llm.Assistant, Content: msg}}}, nil
		}
		rounds++

		// Tokens are buffered per call. A call that ends in tool calls was a planning
		// step and its text is dropped; only the final answer is shown.
		var tokens []string
		input := append(append([]llm.Message{system}, history...), loop...)
		resp, err := a.Coordinator.Chat(ctx, input, specs, llm.Options{
			MaxTokens: llm.Coordinator.MaxTokens(),
			OnToken:   func(s string) { tokens = append(tokens, s) },
		})
		if err != nil {
			return nil, err
		}
		loop = append(loop, resp.Message)
		if len(resp.Message.ToolCalls) == 0 {
			final := strings.TrimSpace(resp.Message.Content)
			switch {
			case final == "":
				// The model returned nothing: the context is probably too large, or it
				// was rate limited without saying so.
				slog.Warn("coordinator model returned no text, likely context overflow", "session", st.SessionID)
				msg := "I was unable to generate a response - the session context may have grown too large. Please start a new session to continue."
				a.emit(st.SessionID, events.NewToken(st.SessionID, msg))
				return &directResult{Messages: []llm.Message{{Role: llm.Assistant, Content: msg}}}, nil
			case isSentinel(final):
				// A routing decision, not an answer to show.
			default:
				a.flushAnswer(st.SessionID, tokens, resp.Message.Content)
			}
			break
		}
		msgs, held, approval := a.Tools.runBatch(ctx, resp.Message.ToolCalls, call, hooks)
		loop = append(loop, msgs...)
		if held != nil {
			st.Pending = &Pending{Approval: *approval, Call: *held, Loop: loop, Rounds: rounds}
			var stdin *string
			if approval.Stdin != "" {
				stdin = &approval.Stdin
			}
			a.emit(st.SessionID, events.NewHitlRequest(st.SessionID, approval.RiskLevel, approval.Command, stdin))
			return &directResult{Paused: true}, nil
		}
	}

	out := &directResult{}
	newMsgs := trimToolMessages(loop)
	newMsgs = fillOrphanToolCalls(newMsgs)
	if a.Cfg.InvestigationPlan {
		plan, cleaned := extractPlan(newMsgs)
		newMsgs = cleaned
		if len(plan) > 0 {
			annotatePlan(plan, newMsgs)
			out.Plan = plan
			steps := make([]map[string]any, len(plan))
			for i, s := range plan {
				steps[i] = map[string]any{"description": s.Description, "status": s.Status}
			}
			a.emit(st.SessionID, events.NewPlan(st.SessionID, steps))
			slog.Info("investigation plan emitted", "session", st.SessionID, "steps", len(plan))
		}
	}
	out.Messages = newMsgs
	a.maybeRecordDirectOutcome(ctx, st, newMsgs)
	return out, nil
}

// flushAnswer shows the final answer. The streamed pieces are used as they came,
// unless a plan block was stripped from the answer, in which case the cleaned
// text goes out whole.
func (a *Agent) flushAnswer(session string, tokens []string, content string) {
	if planBlockRe.MatchString(content) {
		if _, cleaned := extractPlan([]llm.Message{{Role: llm.Assistant, Content: content}}); len(cleaned) == 1 &&
			strings.TrimSpace(cleaned[0].Content) != "" {
			a.emit(session, events.NewToken(session, cleaned[0].Content))
			return
		}
	}
	if len(tokens) == 0 && content != "" {
		tokens = []string{content}
	}
	for _, t := range tokens {
		a.emit(session, events.NewToken(session, t))
	}
}

// coordinate is the coordinator node. It returns after either answering, pausing
// for approval, or setting a routing flag for the next node.
func (a *Agent) coordinate(ctx context.Context, st *State, bypass bool, resume *bool) (paused bool, err error) {
	sid := st.SessionID

	// Synthesis mode: subagent findings are ready.
	if len(st.Findings) > 0 {
		a.emit(sid, events.NewStatus(sid, "synthesizing", "Synthesizing specialist findings…"))
		return false, a.synthesize(ctx, st)
	}

	a.emit(sid, events.NewStatus(sid, "analyzing", "Analyzing your request…"))
	started := time.Now()
	res, err := a.directAnswer(ctx, st, bypass, resume)
	if err != nil {
		slog.Error("coordinator model call failed", "session", sid, "err", err)
		return false, err
	}
	if res.Paused {
		return true, nil
	}
	if len(res.Messages) == 0 {
		return false, nil
	}
	last := strings.TrimSpace(res.Messages[len(res.Messages)-1].Content)
	isRCA := last == "RCA_REQUIRED"
	var m []string
	if !isRCA {
		m = targetedRe.FindStringSubmatch(last)
	}
	route := "direct"
	switch {
	case isRCA:
		route = "RCA"
	case m != nil:
		route = "TARGETED"
	}
	slog.Info("coordinator routing decision", "route", route, "elapsed_ms", time.Since(started).Milliseconds(), "session", sid)

	if m != nil {
		t := &Targeted{Namespace: strings.TrimRight(m[1], ","), Pod: strings.TrimRight(m[2], ","), Issue: strings.TrimSpace(m[3])}
		slog.Info("coordinator targeted investigation", "namespace", t.Namespace, "pod", t.Pod, "issue", t.Issue, "session", sid)
		a.emit(sid, events.NewStatus(sid, "investigating", fmt.Sprintf("Targeting %s in %s…", t.Pod, t.Namespace)))
		st.Targeted, st.RCARequired = t, false
		return false, nil
	}
	if isRCA {
		slog.Info("coordinator requested RCA, fanning out to specialists", "session", sid)
		a.emit(sid, events.NewStatus(sid, "dispatching", "Dispatching specialist subagents (pod · metrics · logs · events)…"))
		// The sentinel is not added to history: routing reads the flag.
		st.RCARequired = true
		return false, nil
	}
	st.Messages = append(st.Messages, res.Messages...)
	if res.Plan != nil {
		st.Plan = res.Plan
	}
	return false, nil
}

// targetedInvestigate runs three parallel reads for a single resource issue and
// adds them to the snapshot, then returns to the coordinator.
func (a *Agent) targetedInvestigate(ctx context.Context, st *State) {
	sid := st.SessionID
	t := st.Targeted
	st.Targeted = nil
	if t == nil {
		return
	}
	// ns and pod are captures out of a line the model wrote, and they are spliced
	// into an argv. The snapshot reader refuses a blocked namespace itself, but a
	// refusal rendered inside a fence headed "Pod Description" reads as a
	// description, so say plainly that the read did not happen.
	if a.Snapshot.Blocked[strings.ToLower(strings.TrimSpace(t.Namespace))] {
		slog.Warn("targeted investigation refused a protected namespace", "namespace", t.Namespace, "session", sid)
		refusal := fmt.Sprintf("## Targeted Investigation: %s in %s\n%s\n\nNo pod description, events or deployments were read. "+
			"Tell the user this namespace is protected; do not infer its contents from anything above.",
			t.Pod, t.Namespace, nsguard.ProtectedMessage(t.Namespace))
		st.ClusterSnapshot = joinBlocks(st.ClusterSnapshot, refusal)
		return
	}
	a.emit(sid, events.NewStatus(sid, "investigating", fmt.Sprintf("Investigating %s in %s…", t.Pod, t.Namespace)))
	var describe, evs, deploys string
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		describe = a.Snapshot.ReadText(ctx, []string{"describe", "pod", t.Pod, "-n", t.Namespace})
	}()
	go func() {
		defer wg.Done()
		evs = a.Snapshot.ReadText(ctx, []string{"get", "events", "-n", t.Namespace, "--sort-by=.lastTimestamp"})
	}()
	go func() {
		defer wg.Done()
		deploys = a.Snapshot.ReadText(ctx, []string{"get", "deployments", "-n", t.Namespace})
	}()
	wg.Wait()
	detail := fmt.Sprintf("## Targeted Investigation: %s in %s\n**Issue**: %s\n\n### Pod Description\n```\n%s\n```\n\n"+
		"### Namespace Events\n```\n%s\n```\n\n### Deployments\n```\n%s\n```", t.Pod, t.Namespace, t.Issue, describe, evs, deploys)
	st.ClusterSnapshot = joinBlocks(st.ClusterSnapshot, detail)
	slog.Debug("targeted investigation complete", "chars", len(detail), "pod", t.Pod, "session", sid)
}

func joinBlocks(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "\n\n" + add
}

// synthesize turns the subagent findings into a final root cause analysis.
func (a *Agent) synthesize(ctx context.Context, st *State) error {
	var xml []string
	for _, f := range st.Findings {
		ev := f.Evidence
		if len(ev) > 3 {
			ev = ev[:3]
		}
		xml = append(xml, fmt.Sprintf("<finding domain='%s' confidence='%v'>\n  hypothesis: %s\n  signals: %s\n  evidence: %s\n</finding>",
			f.Domain, f.Confidence, f.Hypothesis, strings.Join(f.Signals, ", "), strings.Join(ev, "\n")))
	}
	resp, err := a.Coordinator.Chat(ctx, []llm.Message{
		{Role: llm.System, Content: coordinatorSystem},
		{Role: llm.User, Content: synthesisPrompt(strings.Join(xml, "\n"))},
	}, nil, llm.Options{MaxTokens: llm.Coordinator.MaxTokens()})
	if err != nil {
		return err
	}
	rca, perr := parseRCA(resp.Message.Content)
	if perr != nil {
		slog.Warn("coordinator failed to parse the RCA JSON", "err", perr)
		var hyps []string
		for _, f := range st.Findings {
			hyps = append(hyps, f.Hypothesis)
		}
		rca = &RCAResult{
			RootCause: "Synthesis failed - see individual findings", Confidence: 0,
			SupportingEvidence: hyps, Reasoning: "Parse error: " + perr.Error(), RecommendedFix: "Review findings manually",
		}
	}
	summary := fmt.Sprintf("**Root Cause**: %s\n\n**Confidence**: %.0f%%\n\n**Recommended Fix**: %s\n\n**Reasoning**: %s",
		rca.RootCause, rca.Confidence*100, rca.RecommendedFix, rca.Reasoning)
	a.emit(st.SessionID, events.NewToken(st.SessionID, summary))

	// Reflexion: persist confident outcomes so future sessions benefit. A failure to
	// write must never break the answer.
	if a.Cfg.Reflexion && rca.Confidence >= a.Cfg.ReflexionMinConf {
		// Copied first: the turn keeps changing the state after this returns.
		out := Outcome{
			SessionID: st.SessionID, UserID: st.UserID, ClusterID: st.ClusterID,
			RootCause: rca.RootCause, Confidence: rca.Confidence, RecommendedFix: rca.RecommendedFix,
			Playbooks: append([]string(nil), st.MatchedPlaybooks...), Role: st.UserRole, RequestID: st.SessionID,
			TriggerSource: st.TriggerSource, TriggerDetail: clip(lastUserText(st), 300),
			Summary: clip(summary, 1200), EpisodeOutcome: "report_only",
		}
		a.recordAsync(func(ctx context.Context) { a.Memory.Record(ctx, out) })
		if a.Cfg.CortexV5 && a.Cfg.Writeback && a.Writeback != nil && len(out.Playbooks) > 0 {
			cluster, pbs := st.ClusterID, out.Playbooks
			a.recordAsync(func(ctx context.Context) { a.Writeback(ctx, cluster, pbs) })
		}
		slog.Info("rca outcome scheduled", "session", st.SessionID, "confidence", rca.Confidence)
	}
	st.RCAResult, st.RCARequired = rca, false
	st.Messages = append(st.Messages, llm.Message{Role: llm.Assistant, Content: summary})
	return nil
}

// parseRCA reads the synthesis JSON, tolerating a markdown fence around it.
func parseRCA(raw string) (*RCAResult, error) {
	var r RCAResult
	if err := json.Unmarshal([]byte(stripFence(raw)), &r); err != nil {
		return nil, err
	}
	if r.Confidence < 0 || r.Confidence > 1 {
		return nil, fmt.Errorf("confidence %v is outside 0 to 1", r.Confidence)
	}
	if r.RootCause == "" {
		return nil, fmt.Errorf("root_cause is missing")
	}
	return &r, nil
}

// stripFence takes the JSON out of a ```json fence, when there is one.
func stripFence(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		parts := strings.SplitN(raw, "```", 3)
		if len(parts) >= 2 {
			raw = strings.TrimPrefix(parts[1], "json")
		}
	}
	return strings.TrimSpace(raw)
}

func lastUserText(st *State) string {
	for i := len(st.Messages) - 1; i >= 0; i-- {
		if st.Messages[i].Role == llm.User && st.Messages[i].Content != "" {
			return st.Messages[i].Content
		}
	}
	return ""
}

// recordAsync runs a memory write off the request path, detached from the
// request's cancellation so an answer already delivered is not cancelled with it.
func (a *Agent) recordAsync(f func(context.Context)) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				slog.Warn("memory write panicked", "err", r)
			}
		}()
		f(ctx)
	}()
}
