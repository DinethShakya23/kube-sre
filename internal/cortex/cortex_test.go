package cortex

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/aci"
	"github.com/DinethShakya23/kube-sre/internal/change"
	"github.com/DinethShakya23/kube-sre/internal/detect"
	"github.com/DinethShakya23/kube-sre/internal/llm"
	"github.com/DinethShakya23/kube-sre/internal/playbooks"
)

var ctx = context.Background()

// script answers each Chat call in turn and remembers what it was asked.
type script struct {
	replies []llm.Message
	err     error
	seen    [][]llm.Message
	tools   [][]llm.ToolSpec
}

func (s *script) Chat(_ context.Context, msgs []llm.Message, tools []llm.ToolSpec, _ llm.Options) (*llm.Response, error) {
	s.seen = append(s.seen, append([]llm.Message(nil), msgs...))
	s.tools = append(s.tools, tools)
	if s.err != nil {
		return nil, s.err
	}
	m := llm.Message{Content: "done"}
	if len(s.replies) > 0 {
		m, s.replies = s.replies[0], s.replies[1:]
	}
	m.Role = llm.Assistant
	return &llm.Response{Message: m}, nil
}

func say(s string) llm.Message { return llm.Message{Content: s} }

func verbsOf(reply string) aci.Verbs {
	return aci.Verbs{Run: func(context.Context, string, string) string { return reply }, Limits: aci.Limits{MaxLines: 20, MaxChars: 2000}}
}

func TestContractsAreReadOnlyByConstruction(t *testing.T) {
	c, err := NewContract("why is web crashing")
	if err != nil || len(c.AllowedVerbs) != 4 || c.MaxSummaryTokens != 2000 {
		t.Fatalf("%+v %v", c, err)
	}
	bad := Contract{Objective: "x", AllowedVerbs: map[string]bool{"inspect": true, "delete": true}}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "[delete]") {
		t.Errorf("%v", err)
	}
	mut := Contract{Objective: "x", AllowedVerbs: aci.ReadVerbAllowlist(), MayMutate: true}
	if err := mut.Validate(); err == nil || !strings.Contains(err.Error(), "must not mutate") {
		t.Errorf("%v", err)
	}
	if _, err := Finalize(c, "s", []string{"inspect", "kubectl_apply"}); err == nil {
		t.Error("a used verb outside the allowlist is a contract violation")
	}
}

func TestBoundSummaryIsExplicitAboutWhatItCut(t *testing.T) {
	short, cut := BoundSummary("fine", 2000)
	if short != "fine" || cut {
		t.Error("under budget")
	}
	long := strings.Repeat("a line of evidence\n", 2000)
	got, cut := BoundSummary(long, 100)
	if !cut || len(got) > 400+200 || !strings.Contains(got, "[truncated: ") || !strings.Contains(got, "2k-token subagent budget") {
		t.Errorf("%d %q", len(got), got[len(got)-90:])
	}
	if body := got[:strings.Index(got, "\n…")]; !strings.HasSuffix(body, "evidence") {
		t.Errorf("cut on a line boundary: %q", body[len(body)-20:])
	}
	multi := strings.Repeat("✓", 500)
	if g, _ := BoundSummary(multi, 20); !strings.Contains(g, "[truncated") {
		t.Error("multibyte")
	}
}

func TestPlanSubagents(t *testing.T) {
	got := PlanSubagents([]string{"check pods", "check events", "check logs"}, "ignored", 2)
	if len(got) != 2 || got[0].Objective != "check pods" || got[1].Objective != "check events" {
		t.Errorf("%+v", got)
	}
	if one := PlanSubagents(nil, "  why is web crashing  ", 4); len(one) != 1 || one[0].Objective != "why is web crashing" {
		t.Errorf("no plan falls back to the user's question: %+v", one)
	}
	if one := PlanSubagents(nil, "", 4); one[0].Objective != "investigate the reported issue" {
		t.Errorf("%+v", one)
	}
	if PlanSubagents([]string{"x"}, "", 0) != nil {
		t.Error("a bound of zero plans nothing")
	}
	if long := PlanSubagents(nil, strings.Repeat("q", 900), 1); len([]rune(long[0].Objective)) != 300 {
		t.Error("objective is bounded")
	}
}

func TestSubagentUsesOnlyReadVerbsAndBoundsItsRounds(t *testing.T) {
	m := &script{replies: []llm.Message{
		{ToolCalls: []llm.ToolCall{{ID: "1", Name: "inspect", Args: map[string]any{"kind": "pod", "name": "web-1", "namespace": "shop"}}}},
		{ToolCalls: []llm.ToolCall{{ID: "2", Name: "delete_pod", Args: map[string]any{}}}},
		say("the pod is OOMKilled: limit 128Mi"),
	}}
	c, _ := NewContract("why is web-1 down")
	r := RunSubagent(ctx, m, verbsOf("web-1 CrashLoopBackOff"), c, "## snapshot: web-1 exists", 5)
	if r.Summary != "the pod is OOMKilled: limit 128Mi" || len(r.VerbsUsed) != 1 || r.VerbsUsed[0] != "inspect" {
		t.Errorf("%+v", r)
	}
	// It never sees the lead's history: a system prompt, the snapshot and the objective only.
	first := m.seen[0]
	if len(first) != 3 || !strings.Contains(first[0].Content, "why is web-1 down") || !strings.Contains(first[1].Content, "web-1 exists") || first[2].Content != "why is web-1 down" {
		t.Errorf("%+v", first)
	}
	if len(m.tools[0]) != 4 {
		t.Errorf("only the four read verbs are offered: %d", len(m.tools[0]))
	}
	// The blocked verb came back to the model as a refusal.
	last := m.seen[2][len(m.seen[2])-1]
	if last.Role != llm.Tool || !strings.HasPrefix(last.Content, "[blocked]") {
		t.Errorf("%+v", last)
	}

	loop := &script{replies: []llm.Message{
		{ToolCalls: []llm.ToolCall{{ID: "1", Name: "search", Args: map[string]any{"kinds": []any{"pods"}}}}},
		{ToolCalls: []llm.ToolCall{{ID: "2", Name: "search", Args: map[string]any{"kinds": []any{"pods"}}}}},
		{ToolCalls: []llm.ToolCall{{ID: "3", Name: "search", Args: map[string]any{"kinds": []any{"pods"}}}}},
	}}
	if r = RunSubagent(ctx, loop, verbsOf("x"), c, "", 2); len(loop.seen) != 2 || len(r.VerbsUsed) != 2 {
		t.Errorf("bounded to two rounds: %d calls %+v", len(loop.seen), r)
	}
}

func TestASubagentThatFailsIsANoteNotACrash(t *testing.T) {
	c, _ := NewContract("x")
	r := RunSubagent(ctx, &script{err: errors.New("model down")}, verbsOf(""), c, "", 3)
	if !strings.HasPrefix(r.Summary, "[investigation error: model down") {
		t.Errorf("%+v", r)
	}
}

func TestFanoutRunsInParallelAndFallsBackWhenNothingCameBack(t *testing.T) {
	m := &script{}
	contracts := PlanSubagents([]string{"a", "b", "c"}, "", 4)
	bundle, results, usable := Fanout(ctx, m, verbsOf("x"), contracts, "", 2)
	if !usable || len(results) != 3 || !strings.Contains(bundle, "### Finding 1: a") || !strings.Contains(bundle, "### Finding 3: c") || !strings.Contains(bundle, "_read verbs used: none_") {
		t.Errorf("%v\n%s", usable, bundle)
	}
	if _, _, usable = Fanout(ctx, &script{err: errors.New("down")}, verbsOf("x"), contracts, "", 2); usable {
		t.Error("every investigator failed: the caller must fall back to its flat gather, not proceed with no evidence")
	}
	if _, _, usable = Fanout(ctx, &script{replies: []llm.Message{say("  ")}}, verbsOf("x"), contracts[:1], "", 1); usable {
		t.Error("an empty summary is not evidence")
	}
	rec := Reconcile([]Result{{Objective: "o", Summary: "s", Truncated: true, VerbsUsed: []string{"logs", "inspect"}}})
	if !strings.Contains(rec, "_read verbs used: logs, inspect (truncated)_") {
		t.Errorf("%s", rec)
	}
}

func TestReviewFailsOpenButNeverSilently(t *testing.T) {
	ok := ReviewRCA(ctx, &script{replies: []llm.Message{say(`{"supported": true, "confidence": 0.9, "unsupported": []}`)}}, "claim", "ev")
	if !ok.Supported || ok.Errored || ok.Confidence == nil || *ok.Confidence != 0.9 || RenderReviewNote(ok) != "" {
		t.Errorf("only a clean review renders nothing: %+v", ok)
	}
	flagged := ReviewRCA(ctx, &script{replies: []llm.Message{say("Sure:\n```json\n{\"supported\": true, \"confidence\": 7, \"unsupported\": [\"the node is missing\"]}\n```")}}, "c", "e")
	if flagged.Supported || len(flagged.Unsupported) != 1 || *flagged.Confidence != 1 {
		t.Errorf("an unsupported claim overrides supported=true and confidence is clamped: %+v", flagged)
	}
	note := RenderReviewNote(flagged)
	if !strings.Contains(note, "- the node is missing") || !strings.Contains(note, "Reviewer confidence in the RCA: 100%") {
		t.Errorf("%s", note)
	}
	if n := RenderReviewNote(Review{Unsupported: []string{"x"}}); !strings.Contains(n, "stated no confidence value") {
		t.Errorf("%s", n)
	}
	for _, m := range []llm.Model{&script{err: errors.New("down")}, &script{replies: []llm.Message{say("no json here")}}} {
		r := ReviewRCA(ctx, m, "c", "e")
		if !r.Supported || !r.Errored || !strings.Contains(RenderReviewNote(r), "Verification NOT PERFORMED") {
			t.Errorf("a reviewer that never ran must say so: %+v", r)
		}
	}
	if seen := (&script{replies: []llm.Message{say("{}")}}); true {
		ReviewRCA(ctx, seen, "the claim", "the evidence")
		if got := seen.seen[0]; len(got) != 2 || !strings.Contains(got[1].Content, "CLAIM:\nthe claim") || strings.Contains(got[1].Content, "reasoning") {
			t.Errorf("the reviewer sees only the claim and the evidence: %+v", got)
		}
	}
}

func TestGoalGateFailsOpenToSufficient(t *testing.T) {
	if g := EvaluateGoal(ctx, &script{replies: []llm.Message{say(`{"sufficient": false, "missing": ["pod logs", " "]}`)}}, "o", "e"); g.Sufficient || len(g.Missing) != 1 || g.Missing[0] != "pod logs" {
		t.Errorf("%+v", g)
	}
	for _, m := range []llm.Model{&script{err: errors.New("x")}, &script{replies: []llm.Message{say("garbage")}}, &script{replies: []llm.Message{say(`{"missing": []}`)}}} {
		if !EvaluateGoal(ctx, m, "o", "e").Sufficient {
			t.Error("never trap the loop into gathering for ever on a reviewer error")
		}
	}
}

func TestBriefsFailSafeAndAlwaysStateConfidence(t *testing.T) {
	good := `{"summary": "web is OOMKilled", "actions": ["raise the limit", "watch restarts"], "escalate_if": ["restarts continue after the change"], "confidence": 0.0}`
	b := BuildBrief(ctx, &script{replies: []llm.Message{say(good)}}, "OOM", "ev", "senior")
	if b.FellBack || b.ResponderLevel != "senior" || len(b.Actions) != 2 || b.Confidence == nil || *b.Confidence != 0 {
		t.Fatalf("%+v", b)
	}
	if len(b.EscalateIf) != 4 || b.EscalateIf[0] != "restarts continue after the change" {
		t.Errorf("the conservative escalation boundaries are always appended: %v", b.EscalateIf)
	}
	md := RenderBrief(b)
	if !strings.Contains(md, "### Responder brief (senior)") || !strings.Contains(md, "1. raise the limit") || !strings.Contains(md, "Brief confidence: 0%") || strings.Contains(md, "FALLBACK") {
		t.Errorf("a confidence of zero must still be stated:\n%s", md)
	}
	noConf := BuildBrief(ctx, &script{replies: []llm.Message{say(`{"summary": "s", "actions": ["a"], "escalate_if": ["e"]}`)}}, "r", "e", "bogus-level")
	if noConf.ResponderLevel != "intermediate" || !strings.Contains(RenderBrief(noConf), "reported no confidence in itself") {
		t.Errorf("%+v", noConf)
	}
	for _, m := range []llm.Model{&script{err: errors.New("x")}, &script{replies: []llm.Message{say("prose")}},
		&script{replies: []llm.Message{say(`{"summary": "s", "actions": [], "escalate_if": ["e"]}`)}},
		&script{replies: []llm.Message{say(`{"summary": "", "actions": ["a"], "escalate_if": ["e"]}`)}}} {
		fb := BuildBrief(ctx, m, "r", "e", "junior")
		if !fb.FellBack || len(fb.EscalateIf) != 3 || !strings.Contains(RenderBrief(fb), "— FALLBACK") || !strings.Contains(fb.Summary, "proceed conservatively") {
			t.Errorf("%+v", fb)
		}
	}
}

func TestSkillsRenderOnlyMatchedPlaybooks(t *testing.T) {
	reg := playbooks.Load()
	block := RenderMatchedSkills(reg, []string{"CrashLoopBackOff", "NoSuchPlaybook", "OOMKilled"}, 5)
	if !strings.HasPrefix(block, "## Runbook skills (loaded on demand") || !strings.Contains(block, "### SKILL: CrashLoopBackOff") || strings.Contains(block, "NoSuchPlaybook") {
		t.Errorf("%s", block[:200])
	}
	if !strings.Contains(block, "**Diagnostic steps:**\n1. ") {
		t.Errorf("numbered steps expected")
	}
	if RenderMatchedSkills(reg, nil, 5) != "" || RenderMatchedSkills(reg, []string{"nope"}, 5) != "" {
		t.Error("nothing matched leaves the prompt unchanged")
	}
	if strings.Count(RenderMatchedSkills(reg, []string{"CrashLoopBackOff", "OOMKilled", "ImagePullBackOff"}, 2), "### SKILL:") != 2 {
		t.Error("max skills bounds the prompt cost")
	}
}

func TestHeartbeatIsSilentForAFastPhaseAndStopsCleanly(t *testing.T) {
	var beats atomic.Int32
	stop := Heartbeat(ctx, 20*time.Millisecond, func() { beats.Add(1) })
	stop()
	if beats.Load() != 0 {
		t.Errorf("a fast phase emits nothing: %d", beats.Load())
	}
	stop = Heartbeat(ctx, 10*time.Millisecond, func() { beats.Add(1) })
	time.Sleep(60 * time.Millisecond)
	stop()
	n := beats.Load()
	time.Sleep(40 * time.Millisecond)
	if n < 2 || beats.Load() != n {
		t.Errorf("beats %d then %d: it must beat while running and stop when told", n, beats.Load())
	}
	Heartbeat(ctx, 5*time.Millisecond, func() { panic("broken beat") })()
	time.Sleep(20 * time.Millisecond)
	if Heartbeat(ctx, 0, nil) == nil {
		t.Error("a zero interval is a no-op stop")
	}
}

func TestPhaseBudget(t *testing.T) {
	now := time.Unix(1000, 0)
	b := NewPhaseBudget(30*time.Second, 120*time.Second, func() time.Time { return now })
	if b.Warning() != "" || b.FirstSignalBreached() {
		t.Error("fresh")
	}
	now = now.Add(45 * time.Second)
	if el := b.MarkFirstSignal(); el != 45*time.Second || !b.FirstSignalBreached() {
		t.Errorf("%v", el)
	}
	now = now.Add(time.Hour)
	if b.MarkFirstSignal() != 45*time.Second {
		t.Error("the first signal is recorded once")
	}
	w := b.Warning()
	if !strings.Contains(w, "first signal 45s > 30s target") || !strings.Contains(w, "full investigation 3645s > 120s target") {
		t.Errorf("%s", w)
	}
}

func TestRoutingDegradesToTheSmallFloorAndDiscloses(t *testing.T) {
	no := false
	for task, want := range map[string]string{TaskTriage: TierSmall, TaskToolFormat: TierSmall, TaskRCASynthesis: TierFrontier, "unknown": TierFrontier} {
		if got := RouteTier(task, true, nil); got != want {
			t.Errorf("%s: %s", task, got)
		}
	}
	if RouteTier(TaskRCASynthesis, false, nil) != TierSmall || RouteTier(TaskRCASynthesis, true, &no) != TierSmall {
		t.Error("frontier unreachable degrades instead of failing")
	}
	if RouteTier(TaskTriage, false, nil) != TierSmall {
		t.Error("small stays small")
	}
	if !Degraded(TaskRCASynthesis, false, nil) || Degraded(TaskTriage, false, nil) || Degraded(TaskRCASynthesis, true, nil) {
		t.Error("only a task running below its preferred tier is degraded")
	}
	if EdgeWriteAllowed(false) || !EdgeWriteAllowed(true) {
		t.Error("disconnected means no unsupervised writes")
	}
}

func TestPlanWatchdogsIsBoundedDedupedAndOrdered(t *testing.T) {
	changes := []change.Record{
		{Kind: "scale", Target: "deploy/web", TS: 100, Namespace: "shop"},
		{Kind: "config", Target: "cm/a", TS: 300},
		{Kind: "image", Target: "deploy/api", TS: 200, Namespace: "shop"},
		{Kind: "scale", Target: "deploy/old", TS: 10},
	}
	got := PlanWatchdogs(changes, 1000, 300, 2, 50, nil)
	if len(got) != 2 || got[0].Target != "cm/a" || got[1].Target != "deploy/api" || got[0].TTLSeconds != 300 || got[0].CreatedEpoch != 1000 {
		t.Fatalf("most recent first, bounded, and nothing older than the last sweep: %+v", got)
	}
	if !strings.Contains(got[1].Objective, "A image to deploy/api in shop just happened") || strings.Contains(got[0].Objective, " in ") {
		t.Errorf("%s / %s", got[1].Objective, got[0].Objective)
	}
	seen := map[string]bool{"shop/image/deploy/api": true}
	if again := PlanWatchdogs(changes, 1000, 300, 5, 0, seen); len(again) != 3 {
		t.Errorf("already-armed changes are skipped: %d", len(again))
	}
	if got[0].Expired(1200) || !got[0].Expired(1301) {
		t.Error("ttl")
	}
}

func TestSweepWatchesEachChangeOnce(t *testing.T) {
	w := NewWatchdogs()
	var fired []string
	w.SetDispatch(func(t WatchdogTask) {
		fired = append(fired, t.Target)
		if t.Target == "boom" {
			panic("dispatch failed")
		}
	})
	changes := []change.Record{{Kind: "scale", Target: "a", TS: 100}, {Kind: "scale", Target: "boom", TS: 101}}
	if n := w.Sweep(changes, 200, 300, 5); n != 1 || len(fired) != 2 {
		t.Errorf("one dispatch panicked and must not stop the others: %d %v", n, fired)
	}
	fired = nil
	if n := w.Sweep(changes, 300, 300, 5); n != 0 || len(fired) != 0 {
		t.Errorf("a change is watched once: %d %v", n, fired)
	}
	changes = append(changes, change.Record{Kind: "scale", Target: "b", TS: 350})
	if n := w.Sweep(changes, 400, 300, 5); n != 1 || fired[0] != "b" {
		t.Errorf("%d %v", n, fired)
	}
}

func TestPredictionsBecomeReadOnlyInvestigationsAndPreCaptureIsTargeted(t *testing.T) {
	eta := 12.0
	pred := detect.Finding{Playbook: "OOMKilled", Namespace: "shop", Object: "web-1", Severity: "predicted", ETAMinutes: &eta}
	if !IsPrediction(pred) || IsPrediction(detect.Finding{Severity: "warning"}) {
		t.Error("is prediction")
	}
	task := PredictionTask(pred, 300)
	if task.Kind != "predicted" || task.DedupKey != "predicted/shop/web-1" || task.TTLSeconds != 300 {
		t.Errorf("%+v", task)
	}
	for _, want := range []string{"PREDICTED OOMKilled for web-1 in shop (ETA ~12m)", "(read-only)", "a prediction never drives a fix"} {
		if !strings.Contains(task.Objective, want) {
			t.Errorf("missing %q in %s", want, task.Objective)
		}
	}
	plan := PlanPreCapture(pred, 15, nil)
	if plan == nil || len(plan.Actions) != 2 || plan.Actions[0] != LogVerbosity || plan.Target != "web-1" || !strings.Contains(plan.Reason, "in ~12m") {
		t.Errorf("%+v", plan)
	}
	far, past, zero := 40.0, 0.0, -1.0
	for name, f := range map[string]detect.Finding{
		"far off":  {Severity: "predicted", ETAMinutes: &far},
		"no eta":   {Severity: "predicted"},
		"zero":     {Severity: "predicted", ETAMinutes: &past},
		"negative": {Severity: "predicted", ETAMinutes: &zero},
		"realized": {Severity: "warning", ETAMinutes: &eta},
	} {
		if PlanPreCapture(f, 15, nil) != nil {
			t.Errorf("%s must not arm capture", name)
		}
	}
	if p := PlanPreCapture(pred, 15, []string{HeapDump}); len(p.Actions) != 1 || p.Actions[0] != HeapDump {
		t.Errorf("%+v", p)
	}
}
