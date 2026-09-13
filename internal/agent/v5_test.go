package agent

import (
	"context"
	"github.com/DinethShakya23/kube-sre/internal/aci"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/change"
	"github.com/DinethShakya23/kube-sre/internal/llm"
)

var v5On = map[string]string{"CORTEX_V5_ENABLED": "true", "KI_V5_CHANGE_LEDGER": "true", "KI_V5_CHANGE_FIRST_RCA": "true", "KI_V5_INVESTIGATION_WRITEBACK": "true"}

func TestMutationsAreRecordedInTheChangeLedgerOnlyWhenEnabled(t *testing.T) {
	for _, on := range []bool{true, false} {
		env := map[string]string{}
		if on {
			env = v5On
		}
		r := newRig(t, okKubectl, env)
		r.a.Changes = change.NewLedger()
		r.coord.replies = []llm.Message{toolCall("c1", ToolKubectl, map[string]any{"command": "scale deployment web --replicas=2 -n shop"}), say("done")}
		r.turn(t, "scale web", func(q *TurnRequest) { q.AutoApprove = true })
		var got []change.Record
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && on && len(got) == 0 {
			got = r.a.Changes.Recent("test-cluster", "")
			time.Sleep(20 * time.Millisecond)
		}
		got = r.a.Changes.Recent("test-cluster", "")
		switch {
		case on && (len(got) != 1 || got[0].Kind != "scale" || got[0].Target != "deployment"):
			t.Errorf("enabled: %+v", got)
		case !on && len(got) != 0:
			t.Errorf("flag off must record nothing: %+v", got)
		}
	}
}

func TestChangeFirstPriorReachesTheCoordinatorPrompt(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want bool
	}{{v5On, true}, {map[string]string{"KI_V5_CHANGE_FIRST_RCA": "true"}, false}, {nil, false}} {
		r := newRig(t, okKubectl, tc.env)
		r.a.Changes = change.NewLedger()
		r.a.Changes.Add("test-cluster", change.Record{Kind: "image", Target: "deploy/api", TS: 1, Detail: "kubectl set image deploy/api api=v3"})
		r.coord.replies = []llm.Message{say("ok")}
		r.turn(t, "what changed?")
		got := strings.Contains(r.coord.seen[0][0].Content, "Recent changes (consider these FIRST")
		if got != tc.want {
			t.Errorf("env %v: prior present=%v", tc.env, got)
		}
		if tc.want && !strings.Contains(r.coord.seen[0][0].Content, "- image deploy/api") {
			t.Error("the change itself must be listed")
		}
	}
}

const crashKubectl = `case "$1 $2" in
"get pods") printf 'NAMESPACE  NAME  READY  STATUS  RESTARTS  AGE\nshop  web-1  0/1  CrashLoopBackOff  9  1d\n';;
"get events") echo "No resources found";;
*) echo done;;
esac`

func TestInvestigationWritebackFeedsTheMatchedPlaybooksBack(t *testing.T) {
	var mu sync.Mutex
	var gotCluster string
	var gotPlaybooks []string
	for _, on := range []bool{true, false} {
		env := map[string]string{}
		if on {
			env = v5On
		}
		r := newRig(t, crashKubectl, env)
		r.a.Writeback = func(_ context.Context, cluster string, playbooks []string) {
			mu.Lock()
			gotCluster, gotPlaybooks = cluster, playbooks
			mu.Unlock()
		}
		mu.Lock()
		gotCluster, gotPlaybooks = "", nil
		mu.Unlock()
		r.coord.replies = []llm.Message{
			say("RCA_REQUIRED"),
			say(`{"root_cause":"bad image","confidence":0.9,"supporting_evidence":["a"],"reasoning":"r","recommended_fix":"fix","affected_domain":["pod"]}`),
		}
		r.sub.next = func(msgs []llm.Message) *llm.Message {
			return &llm.Message{Content: "```json\n{\"domain\":\"pod\",\"signals\":[\"s\"],\"hypothesis\":\"h\",\"confidence\":0.8,\"evidence\":[\"e\"]}\n```"}
		}
		r.turn(t, "why is web crashing")
		time.Sleep(300 * time.Millisecond)
		mu.Lock()
		c, p := gotCluster, gotPlaybooks
		mu.Unlock()
		switch {
		case on && (c != "test-cluster" || len(p) == 0):
			t.Errorf("enabled: %q %v", c, p)
		case !on && (c != "" || p != nil):
			t.Errorf("flag off must not write back: %q %v", c, p)
		}
	}
}

func cortexOn(extra map[string]string) map[string]string {
	env := map[string]string{"CORTEX_V5_ENABLED": "true"}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func rcaRig(t *testing.T, env map[string]string) *rig {
	r := newRig(t, crashKubectl, env)
	r.coord.replies = []llm.Message{
		say("RCA_REQUIRED"),
		say(`{"root_cause":"bad image tag","confidence":0.9,"supporting_evidence":["a"],"reasoning":"because","recommended_fix":"fix the tag","affected_domain":["pod"]}`),
	}
	r.sub.next = func(msgs []llm.Message) *llm.Message {
		return &llm.Message{Content: "```json\n{\"domain\":\"pod\",\"signals\":[\"s\"],\"hypothesis\":\"h\",\"confidence\":0.8,\"evidence\":[\"e\"]}\n```"}
	}
	return r
}

func TestRunbookSkillsReachThePromptOnlyWhenMatchedAndEnabled(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want bool
	}{{cortexOn(map[string]string{"KI_V5_RUNBOOK_SKILLS": "true"}), true}, {cortexOn(nil), false}, {map[string]string{"KI_V5_RUNBOOK_SKILLS": "true"}, false}} {
		r := newRig(t, crashKubectl, tc.env)
		r.coord.replies = []llm.Message{say("ok")}
		r.turn(t, "what is wrong with web?")
		got := strings.Contains(r.coord.seen[0][0].Content, "Runbook skills (loaded on demand")
		if got != tc.want {
			t.Errorf("%v: skills present=%v", tc.env, got)
		}
	}
	healthy := newRig(t, okKubectl, cortexOn(map[string]string{"KI_V5_RUNBOOK_SKILLS": "true"}))
	healthy.coord.replies = []llm.Message{say("ok")}
	healthy.turn(t, "status?")
	if strings.Contains(healthy.coord.seen[0][0].Content, "Runbook skills") {
		t.Error("nothing matched, nothing injected")
	}
}

func TestVerificationNoteAndBriefAreAppendedToTheAnswer(t *testing.T) {
	r := rcaRig(t, cortexOn(map[string]string{"KI_V5_VERIFY_LADDER": "true", "KI_V5_ESCALATION_BRIEFS": "true", "KI_V5_RESPONDER_LEVEL": "junior"}))
	calls := 0
	inner := r.sub.next
	r.sub.next = func(msgs []llm.Message) *llm.Message {
		calls++
		switch {
		case strings.Contains(msgs[0].Content, "adversarial reviewer"):
			return &llm.Message{Content: `{"supported": false, "confidence": 0.3, "unsupported": ["the tag was never checked"]}`}
		case strings.Contains(msgs[0].Content, "escalation-avoidance briefs"):
			return &llm.Message{Content: `{"summary": "bad tag", "actions": ["fix the tag"], "escalate_if": ["still crashing"], "confidence": 0.8}`}
		}
		return inner(msgs)
	}
	out := text(r.turn(t, "why is web crashing"))
	for _, want := range []string{"**Root Cause**: bad image tag", "⚠ Verification:", "- the tag was never checked", "Reviewer confidence in the RCA: 30%", "### Responder brief (junior)", "1. fix the tag", "Escalate only if:"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Index(out, "Verification") > strings.Index(out, "Responder brief") {
		t.Error("verification note comes before the brief")
	}
}

func TestAReviewerThatCannotRunIsAnnouncedNotSilent(t *testing.T) {
	r := rcaRig(t, cortexOn(map[string]string{"KI_V5_VERIFY_LADDER": "true"}))
	inner := r.sub.next
	r.sub.next = func(msgs []llm.Message) *llm.Message {
		if strings.Contains(msgs[0].Content, "adversarial reviewer") {
			return &llm.Message{Content: "I refuse to output JSON"}
		}
		return inner(msgs)
	}
	out := text(r.turn(t, "why is web crashing"))
	if !strings.Contains(out, "Verification NOT PERFORMED") || !strings.Contains(out, "**Root Cause**: bad image tag") {
		t.Errorf("the answer still goes out, and says it was not checked:\n%s", out)
	}
}

func TestNoFeatureFlagsMeansNoExtraModelCallsAndNoExtraText(t *testing.T) {
	r := rcaRig(t, nil)
	out := text(r.turn(t, "why is web crashing"))
	if strings.Contains(out, "Verification") || strings.Contains(out, "Responder brief") || strings.Contains(out, "Latency budget") {
		t.Errorf("%s", out)
	}
	if len(r.sub.seen) != 4 { // the four specialists, one call each: no reviewer, no brief
		t.Errorf("subagent calls %d", len(r.sub.seen))
	}
}

func TestHarnessFanoutReplacesTheSpecialistsAndFallsBackWhenEmpty(t *testing.T) {
	env := cortexOn(map[string]string{"KI_V5_HARNESS_FANOUT": "true", "KI_V5_HARNESS_MAX_SUBAGENTS": "2"})
	r := rcaRig(t, env)
	verbs := aci.Verbs{Run: func(context.Context, string, string) string { return "web-1 CrashLoopBackOff" }, Limits: aci.Limits{MaxLines: 20, MaxChars: 2000}}
	r.a.ACI = &verbs
	r.sub.next = func(msgs []llm.Message) *llm.Message {
		return &llm.Message{Content: "web-1 is crash looping because of the image tag"}
	}
	out := text(r.turn(t, "why is web crashing"))
	if !strings.Contains(out, "**Root Cause**: bad image tag") {
		t.Errorf("%s", out)
	}
	if len(r.sub.seen) != 1 {
		t.Errorf("no plan means one investigator, not four specialists: %d", len(r.sub.seen))
	}
	st, _ := r.a.Checkpoints.Load(context.Background(), "s1")
	if st == nil || len(st.Findings) != 1 || st.Findings[0].Domain != "investigation" || !strings.Contains(st.Findings[0].Hypothesis, "image tag") {
		t.Errorf("%+v", st)
	}

	// Every investigator failed: the four specialists take over, so there is always evidence.
	fb := rcaRig(t, env)
	fb.a.ACI = &verbs
	n := 0
	fb.sub.next = func(msgs []llm.Message) *llm.Message {
		n++
		if strings.Contains(msgs[0].Content, "isolated read-only investigation subagent") {
			return &llm.Message{Content: "  "}
		}
		return &llm.Message{Content: "```json\n{\"domain\":\"pod\",\"signals\":[\"s\"],\"hypothesis\":\"h\",\"confidence\":0.8,\"evidence\":[\"e\"]}\n```"}
	}
	fb.turn(t, "why is web crashing")
	st, _ = fb.a.Checkpoints.Load(context.Background(), "s1")
	if st == nil || len(st.Findings) != 4 {
		t.Errorf("fallback to the specialists: %+v", st)
	}
}

func TestResponsivenessBeatsDuringASlowPhaseAndReportsABreach(t *testing.T) {
	env := cortexOn(map[string]string{"KI_V5_RESPONSIVENESS": "true", "KI_V5_HEARTBEAT_SECONDS": "0.05", "KI_V5_FULL_BUDGET_S": "0.001"})
	r := rcaRig(t, env)
	inner := r.sub.next
	r.sub.next = func(msgs []llm.Message) *llm.Message {
		time.Sleep(150 * time.Millisecond)
		return inner(msgs)
	}
	evs := r.turn(t, "why is web crashing")
	beats := 0
	for _, s := range types(evs, "status") {
		if s["message"] == "Investigation still running…" {
			beats++
		}
	}
	if beats < 1 {
		t.Errorf("a slow phase must not be silent: %d heartbeats", beats)
	}
	if out := text(evs); !strings.Contains(out, "Latency budget: full investigation") {
		t.Errorf("%s", out)
	}
}
