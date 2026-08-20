package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/events"
	"github.com/DinethShakya23/kube-sre/internal/helm"
	"github.com/DinethShakya23/kube-sre/internal/kube"
	"github.com/DinethShakya23/kube-sre/internal/llm"
	"github.com/DinethShakya23/kube-sre/internal/loki"
	"github.com/DinethShakya23/kube-sre/internal/nsguard"
	"github.com/DinethShakya23/kube-sre/internal/playbooks"
	"github.com/DinethShakya23/kube-sre/internal/prom"
)

// scripted is a model that answers from a list, and records what it was asked.
type scripted struct {
	mu      sync.Mutex
	replies []llm.Message
	seen    [][]llm.Message
	next    func(msgs []llm.Message) *llm.Message
}

func (s *scripted) Chat(ctx context.Context, msgs []llm.Message, tools []llm.ToolSpec, o llm.Options) (*llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, append([]llm.Message(nil), msgs...))
	var m llm.Message
	if s.next != nil {
		if r := s.next(msgs); r != nil {
			m = *r
		}
	} else if len(s.replies) > 0 {
		m, s.replies = s.replies[0], s.replies[1:]
	} else {
		m = llm.Message{Role: llm.Assistant, Content: "done"}
	}
	m.Role = llm.Assistant
	if o.OnToken != nil && m.Content != "" {
		for _, p := range strings.SplitAfter(m.Content, " ") {
			o.OnToken(p)
		}
	}
	llm.MeterFrom(ctx).Add(llm.Usage{PromptTokens: 10, CompletionTokens: 5})
	return &llm.Response{Message: m}, nil
}

func say(s string) llm.Message { return llm.Message{Content: s} }

func toolCall(id, name string, args map[string]any) llm.Message {
	return llm.Message{ToolCalls: []llm.ToolCall{{ID: id, Name: name, Args: args}}}
}

type memCheckpoints struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (c *memCheckpoints) Load(_ context.Context, s string) (*State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.m[s]
	if !ok {
		return nil, nil
	}
	var st State
	return &st, json.Unmarshal(b, &st)
}

func (c *memCheckpoints) Save(_ context.Context, s string, st *State) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string][]byte{}
	}
	b, err := json.Marshal(st)
	c.m[s] = b
	return err
}

type memMemory struct {
	mu   sync.Mutex
	got  []Outcome
	load string
}

func (m *memMemory) Load(context.Context, LoadRequest) string { return m.load }
func (m *memMemory) Record(_ context.Context, o Outcome) {
	m.mu.Lock()
	m.got = append(m.got, o)
	m.mu.Unlock()
}
func (m *memMemory) outcomes() []Outcome {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Outcome(nil), m.got...)
}

type rig struct {
	a       *Agent
	coord   *scripted
	sub     *scripted
	mem     *memMemory
	log     string
	emitter *events.Emitter
}

func newRig(t *testing.T, kubectlBody string, env map[string]string) *rig {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	bin := filepath.Join(dir, "kubectl")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n" + kubectlBody + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Load(func(k string) string { return env[k] })
	blocked := nsguard.Blocklist{"kube-system": true, "monitoring": true}
	tools := &Toolset{
		Kubectl: kube.NewTool(kube.Config{Bin: bin, Timeout: 5 * time.Second, BlockedNamespaces: blocked, BlockedResources: cfg.BlockedResources, ErrorHints: true}),
		Helm:    helm.New(blocked, cfg.BlockedResources),
		Prom:    prom.New("", blocked),
		Loki:    loki.New("", blocked),
	}
	r := &rig{coord: &scripted{}, sub: &scripted{}, mem: &memMemory{}, log: log, emitter: events.NewEmitter(nil)}
	r.a = New(Deps{
		Cfg: cfg, Tools: tools, Coordinator: r.coord, Subagent: r.sub, Emitter: r.emitter,
		Checkpoints: &memCheckpoints{}, Snapshot: &Snapshotter{Bin: bin, Timeout: 5 * time.Second, Blocked: blocked},
		Playbooks: playbooks.Load(), Memory: r.mem,
		ClusterID: func(context.Context) string { return "test-cluster" },
	})
	return r
}

const okKubectl = `case "$1 $2" in
"get pods") printf 'NAMESPACE  NAME  READY  STATUS  RESTARTS  AGE\nshop  web-1  1/1  Running  0  1d\n';;
"get events") echo "No resources found";;
*) echo done;;
esac`

// turn runs one turn and returns its events.
func (r *rig) turn(t *testing.T, msg string, opts ...func(*TurnRequest)) []map[string]any {
	t.Helper()
	req := TurnRequest{Message: msg, SessionID: "s1", UserID: "u", UserRole: "admin"}
	for _, o := range opts {
		o(&req)
	}
	r.emitter.Prepare(req.SessionID)
	ctx, _ := llm.WithMeter(context.Background())
	r.a.Run(ctx, req)
	var evs []map[string]any
	for it := range r.emitter.Stream(context.Background(), req.SessionID, time.Second) {
		if it.Event != nil {
			evs = append(evs, it.Event)
		}
	}
	return evs
}

func text(evs []map[string]any) string {
	var b strings.Builder
	for _, e := range evs {
		if e["type"] == "token" {
			b.WriteString(e["content"].(string))
		}
	}
	return b.String()
}

func types(evs []map[string]any, typ string) []map[string]any {
	var out []map[string]any
	for _, e := range evs {
		if e["type"] == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestDirectAnswerWithToolCall(t *testing.T) {
	r := newRig(t, okKubectl, nil)
	r.coord.replies = []llm.Message{
		toolCall("c1", ToolKubectl, map[string]any{"command": "get pods -n shop"}),
		say("web-1 is Running."),
	}
	evs := r.turn(t, "how is shop?")
	if text(evs) != "web-1 is Running." {
		t.Errorf("only the final answer is shown: %q", text(evs))
	}
	tc, tr := types(evs, "tool_call"), types(evs, "tool_result")
	if len(tc) != 1 || tc[0]["command"] != "get pods -n shop" || len(tr) != 1 {
		t.Errorf("tool events: %v %v", tc, tr)
	}
	var phases []string
	for _, s := range types(evs, "status") {
		phases = append(phases, s["phase"].(string))
	}
	if strings.Join(phases, ",") != "loading,snapshot,analyzing" {
		t.Errorf("phases %v", phases)
	}
	st, _ := r.a.Checkpoints.Load(context.Background(), "s1")
	if st == nil || len(st.Messages) != 4 || st.Messages[0].Role != llm.User || st.Messages[3].Content != "web-1 is Running." {
		t.Fatalf("history: %+v", st)
	}
	if st.ClusterID != "test-cluster" || !strings.Contains(st.ClusterSnapshot, "web-1") || st.SnapshotPodCount != 1 || st.SnapshotHasIssues {
		t.Errorf("snapshot state: %+v", st)
	}
	last := r.coord.seen[len(r.coord.seen)-1]
	if !strings.Contains(last[0].Content, "## Cluster Snapshot") || !strings.Contains(last[0].Content, "Snapshot sufficiency") {
		t.Error("system prompt lacks the snapshot")
	}
}

func TestSecondTurnKeepsHistoryAndReloadsSnapshot(t *testing.T) {
	r := newRig(t, okKubectl, nil)
	r.coord.replies = []llm.Message{say("first"), say("second")}
	r.turn(t, "one")
	r.turn(t, "two")
	st, _ := r.a.Checkpoints.Load(context.Background(), "s1")
	if len(st.Messages) != 4 || st.Messages[2].Content != "two" {
		t.Errorf("%+v", st.Messages)
	}
	if h := r.coord.seen[1]; len(h) != 4 || h[1].Content != "one" {
		t.Errorf("the second call sees the conversation: %+v", h)
	}
}

func TestWriteWaitsForApprovalThenRuns(t *testing.T) {
	r := newRig(t, okKubectl, nil)
	r.coord.replies = []llm.Message{
		toolCall("c1", ToolKubectl, map[string]any{"command": "scale deployment web --replicas=3 -n shop"}),
		say("Scaled and verified."),
	}
	evs := r.turn(t, "scale web to 3")
	h := types(evs, "hitl_request")
	if len(h) != 1 || h[0]["risk_level"] != "medium" || h[0]["command"] != "kubectl scale deployment web --replicas=3 -n shop" {
		t.Fatalf("hitl: %v", h)
	}
	if text(evs) != "" {
		t.Errorf("nothing is answered while waiting: %q", text(evs))
	}
	if b, _ := os.ReadFile(r.log); strings.Contains(string(b), "scale") {
		t.Fatal("the write must not run before approval")
	}
	st, _ := r.a.Checkpoints.Load(context.Background(), "s1")
	if st.Pending == nil || st.Pending.Call.ID != "c1" {
		t.Fatalf("pending: %+v", st.Pending)
	}

	evs = r.turn(t, "yes")
	if text(evs) != "Scaled and verified." || len(types(evs, "tool_result")) != 1 {
		t.Errorf("after approval: %q %v", text(evs), evs)
	}
	if b, _ := os.ReadFile(r.log); !strings.Contains(string(b), "scale deployment web") {
		t.Error("the approved write must run")
	}
	st, _ = r.a.Checkpoints.Load(context.Background(), "s1")
	if st.Pending != nil {
		t.Error("pending is cleared")
	}
	if len(r.coord.seen) != 2 {
		t.Errorf("the loop continues rather than starting over: %d model calls", len(r.coord.seen))
	}
}

func TestAnythingButAnApprovalCancels(t *testing.T) {
	for _, reply := range []string{"no", "No.", "NO!", "no thanks", "wait", "why?", "", "yes please do", "cancel it", "  "} {
		r := newRig(t, okKubectl, nil)
		r.coord.replies = []llm.Message{toolCall("c1", ToolKubectl, map[string]any{"command": "delete pod web-1 -n shop"}), say("ok, not deleting")}
		r.turn(t, "delete web-1")
		r.turn(t, reply)
		if b, _ := os.ReadFile(r.log); strings.Contains(string(b), "delete pod") {
			t.Errorf("reply %q ran a destructive command", reply)
		}
		last := r.coord.seen[len(r.coord.seen)-1]
		if tm := last[len(last)-1]; tm.Role != llm.Tool || tm.Content != "Action cancelled by user." {
			t.Errorf("reply %q: the model must be told it was cancelled: %+v", reply, tm)
		}
	}
	for _, reply := range []string{"yes", "Yes.", "approve", "go ahead", "approve all", "  OK  "} {
		r := newRig(t, okKubectl, nil)
		r.coord.replies = []llm.Message{toolCall("c1", ToolKubectl, map[string]any{"command": "delete pod web-1 -n shop"}), say("deleted")}
		r.turn(t, "delete web-1")
		r.turn(t, reply)
		if b, _ := os.ReadFile(r.log); !strings.Contains(string(b), "delete pod") {
			t.Errorf("reply %q should approve", reply)
		}
	}
}

func TestAutoApproveSkipsPromptExceptAlwaysConfirm(t *testing.T) {
	r := newRig(t, okKubectl, nil)
	r.coord.replies = []llm.Message{toolCall("c1", ToolKubectl, map[string]any{"command": "scale deployment web --replicas=2 -n shop"}), say("done")}
	evs := r.turn(t, "scale web", func(q *TurnRequest) { q.AutoApprove = true })
	if len(types(evs, "hitl_request")) != 0 || text(evs) != "done" {
		t.Errorf("%v", evs)
	}
	if !strings.Contains(r.coord.seen[0][0].Content, "Proactive fix mode") {
		t.Error("auto approve adds the proactive fix block")
	}

	r = newRig(t, okKubectl, nil)
	r.coord.replies = []llm.Message{toolCall("c1", ToolKubectl, map[string]any{"command": "delete namespace shop"}), say("x")}
	evs = r.turn(t, "delete it", func(q *TurnRequest) { q.AutoApprove = true })
	h := types(evs, "hitl_request")
	if len(h) != 1 || h[0]["risk_level"] != "high" {
		t.Errorf("a namespace delete always asks: %v", evs)
	}

	r = newRig(t, okKubectl, nil)
	r.coord.replies = []llm.Message{toolCall("c1", ToolKubectl, map[string]any{"command": "scale deployment web --replicas=2 -n shop"}), say("done")}
	evs = r.turn(t, "approve all")
	if len(types(evs, "hitl_request")) != 0 {
		t.Error("the phrase 'approve all' bypasses approval for its own turn")
	}
	r.coord.replies = []llm.Message{toolCall("c2", ToolKubectl, map[string]any{"command": "scale deployment web --replicas=5 -n shop"}), say("x")}
	if evs = r.turn(t, "scale to 5"); len(types(evs, "hitl_request")) != 1 {
		t.Error("the bypass does not outlive its turn")
	}
}

func TestOnlyOneApprovalPerBatchAndOthersAreSkipped(t *testing.T) {
	r := newRig(t, okKubectl, nil)
	r.coord.replies = []llm.Message{
		{ToolCalls: []llm.ToolCall{
			{ID: "r", Name: ToolKubectl, Args: map[string]any{"command": "get pods -n shop"}},
			{ID: "w1", Name: ToolKubectl, Args: map[string]any{"command": "scale deployment a --replicas=1 -n shop"}},
			{ID: "w2", Name: ToolKubectl, Args: map[string]any{"command": "scale deployment b --replicas=1 -n shop"}},
		}},
		say("finished"),
	}
	evs := r.turn(t, "scale both")
	if len(types(evs, "hitl_request")) != 1 {
		t.Fatalf("one prompt per batch: %v", evs)
	}
	r.turn(t, "yes")
	last := r.coord.seen[len(r.coord.seen)-1]
	results := map[string]string{}
	for _, m := range last {
		if m.Role == llm.Tool {
			results[m.ToolCallID] = m.Content
		}
	}
	if len(results) != 3 || !strings.Contains(results["r"], "web-1") || !strings.Contains(results["w1"], "done") ||
		!strings.Contains(results["w2"], "Skipped - pending approval") {
		t.Errorf("every tool call needs a result: %v", results)
	}
}

func TestToolFailuresAreIsolatedAndWorded(t *testing.T) {
	r := newRig(t, okKubectl, nil)
	r.coord.replies = []llm.Message{
		{ToolCalls: []llm.ToolCall{
			{ID: "a", Name: ToolKubectl, Args: map[string]any{"command": "get pods; rm -rf /"}},
			{ID: "b", Name: "nonsense", Args: map[string]any{}},
			{ID: "c", Name: ToolKubectl, RawArgs: "not json"},
			{ID: "d", Name: ToolKubectl, Args: map[string]any{"stdin": "x"}},
			{ID: "e", Name: ToolKubectl, Args: map[string]any{"command": "get secrets -n shop"}},
			{ID: "f", Name: ToolPrometheus, Args: map[string]any{"promql": "up"}},
		}},
		say("handled"),
	}
	evs := r.turn(t, "go")
	if text(evs) != "handled" {
		t.Fatalf("%v", evs)
	}
	got := map[string]llm.Message{}
	for _, m := range r.coord.seen[1] {
		if m.Role == llm.Tool {
			got[m.ToolCallID] = m
		}
	}
	checks := map[string]string{
		"a": "Tool error: run_kubectl command='get pods; rm -rf /': Command contains disallowed shell characters",
		"b": "nonsense is not a valid tool, try one of [",
		"c": "invalid arguments for run_kubectl: they must be a JSON object",
		"d": `missing required argument "command"`,
		"e": "[Protected]",
		"f": "not configured",
	}
	for id, want := range checks {
		if !strings.Contains(got[id].Content, want) {
			t.Errorf("%s: %q does not contain %q", id, got[id].Content, want)
		}
	}
	if !got["a"].IsError || got["e"].IsError {
		t.Error("errors are flagged, refusals are answers")
	}
}

func TestSecretsInAFailedCommandAreRedacted(t *testing.T) {
	c := toolFailure(llm.ToolCall{ID: "x", Name: ToolKubectl, Args: map[string]any{"command": "get pods --token=abcdefghij1234567890abcdefghij1234567890"}}, os.ErrInvalid)
	if strings.Contains(c.content, "abcdefghij1234") {
		t.Errorf("%s", c.content)
	}
}

func TestBudgetExhaustionEscalates(t *testing.T) {
	r := newRig(t, okKubectl, map[string]string{"AGENT_COORDINATOR_RECURSION_LIMIT": "6"})
	r.coord.next = func([]llm.Message) *llm.Message {
		m := toolCall("c", ToolKubectl, map[string]any{"command": "get pods -n shop"})
		return &m
	}
	evs := r.turn(t, "loop forever")
	if !strings.Contains(text(evs), "tool-call budget") || !strings.Contains(text(evs), "(6 recursion units)") {
		t.Errorf("%q", text(evs))
	}
	if n := len(r.coord.seen); n != 2 {
		t.Errorf("6 units is 2 rounds, got %d", n)
	}
}

func TestEmptyModelReplyIsSaid(t *testing.T) {
	r := newRig(t, okKubectl, nil)
	r.coord.replies = []llm.Message{say("   ")}
	if evs := r.turn(t, "hi"); !strings.Contains(text(evs), "unable to generate a response") {
		t.Errorf("%q", text(evs))
	}
}

func TestPlanIsExtractedEmittedAndStripped(t *testing.T) {
	r := newRig(t, okKubectl, nil)
	r.coord.replies = []llm.Message{
		{Content: "INVESTIGATION_PLAN:\n- read pods\n- read events\n- read endpoints\n",
			ToolCalls: []llm.ToolCall{{ID: "c", Name: ToolKubectl, Args: map[string]any{"command": "get pods -n shop"}}}},
		say("all good"),
	}
	evs := r.turn(t, "check shop")
	p := types(evs, "plan")
	if len(p) != 1 || len(p[0]["steps"].([]any)) != 3 {
		t.Fatalf("%v", p)
	}
	steps := p[0]["steps"].([]any)
	if steps[0].(map[string]any)["status"] != "done" || steps[1].(map[string]any)["status"] != "skipped" {
		t.Errorf("one call ran: %v", steps)
	}
	st, _ := r.a.Checkpoints.Load(context.Background(), "s1")
	if strings.Contains(st.Messages[1].Content, "INVESTIGATION_PLAN") || len(st.Plan) != 3 {
		t.Errorf("stored: %+v", st.Messages[1])
	}
}

func TestRCAFanOutAndSynthesis(t *testing.T) {
	r := newRig(t, okKubectl, nil)
	r.coord.replies = []llm.Message{
		say("RCA_REQUIRED"),
		say(`{"root_cause":"db is down","confidence":0.9,"supporting_evidence":["a"],"reasoning":"because","recommended_fix":"restart db","affected_domain":["pod"]}`),
	}
	r.sub.next = func(msgs []llm.Message) *llm.Message {
		for _, m := range msgs {
			if m.Role == llm.Tool {
				return &llm.Message{Content: "```json\n{\"domain\":\"pod\",\"signals\":[\"s\"],\"hypothesis\":\"h\",\"confidence\":0.8,\"evidence\":[\"e\"]}\n```"}
			}
		}
		m := toolCall("t", ToolKubectl, map[string]any{"command": "get pods -n shop"})
		return &m
	}
	evs := r.turn(t, "everything is broken")
	out := text(evs)
	if strings.Contains(out, "RCA_REQUIRED") || strings.Contains(out, "{") {
		t.Errorf("no sentinel and no raw JSON: %q", out)
	}
	if !strings.Contains(out, "**Root Cause**: db is down") || !strings.Contains(out, "**Confidence**: 90%") || !strings.Contains(out, "restart db") {
		t.Errorf("%q", out)
	}
	var msgs []string
	for _, s := range types(evs, "status") {
		msgs = append(msgs, s["message"].(string))
	}
	joined := strings.Join(msgs, "|")
	for _, want := range []string{"Dispatching specialist subagents", "Running pod diagnostics", "Running metrics diagnostics", "Running logs diagnostics", "Running events diagnostics", "Synthesizing"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing status %q in %s", want, joined)
		}
	}
	if len(r.sub.seen) != 8 {
		t.Errorf("4 subagents, 2 model calls each: %d", len(r.sub.seen))
	}
	if first := r.sub.seen[0]; len(first) != 2 || first[1].Content != "everything is broken" || !strings.Contains(first[0].Content, "Shared Evidence Bundle") {
		t.Errorf("subagents only see the current question: %+v", first)
	}
	st, _ := r.a.Checkpoints.Load(context.Background(), "s1")
	if st.RCAResult == nil || st.RCAResult.RootCause != "db is down" || len(st.Findings) != 4 {
		t.Errorf("%+v", st.RCAResult)
	}
	for _, m := range st.Messages {
		if strings.Contains(m.Content, "RCA_REQUIRED") {
			t.Error("the sentinel is not kept in history")
		}
	}
	outs := r.mem.outcomes()
	if len(outs) != 1 || outs[0].EpisodeOutcome != "report_only" || outs[0].Confidence != 0.9 {
		time.Sleep(200 * time.Millisecond)
		outs = r.mem.outcomes()
	}
	if len(outs) != 1 || outs[0].EpisodeOutcome != "report_only" || outs[0].Confidence != 0.9 {
		t.Errorf("reflexion write: %+v", outs)
	}
}

func TestSubagentsCannotWrite(t *testing.T) {
	r := newRig(t, okKubectl, nil)
	r.coord.replies = []llm.Message{say("RCA_REQUIRED"), say(`{"root_cause":"x","confidence":0.1,"reasoning":"r","recommended_fix":"f"}`)}
	r.sub.next = func(msgs []llm.Message) *llm.Message {
		for _, m := range msgs {
			if m.Role == llm.Tool {
				return &llm.Message{Content: `{"domain":"pod","hypothesis":"h","confidence":0.5}`}
			}
		}
		m := toolCall("w", ToolKubectl, map[string]any{"command": "delete pod web-1 -n shop"})
		return &m
	}
	evs := r.turn(t, "broken")
	if len(types(evs, "hitl_request")) != 0 {
		t.Error("a subagent must not open an approval")
	}
	if b, _ := os.ReadFile(r.log); strings.Contains(string(b), "delete pod") {
		t.Error("a subagent write must be refused")
	}
	var refused bool
	for _, s := range r.sub.seen {
		for _, m := range s {
			if m.Role == llm.Tool && strings.Contains(m.Content, "read-only access") {
				refused = true
			}
		}
	}
	if !refused {
		t.Error("the refusal names the role")
	}
}

func TestUnparseableFindingAndRCADegrade(t *testing.T) {
	r := newRig(t, okKubectl, nil)
	r.coord.replies = []llm.Message{say("RCA_REQUIRED"), say("not json at all")}
	r.sub.replies = []llm.Message{say("prose"), say("prose"), say("prose"), say("prose")}
	evs := r.turn(t, "broken")
	if !strings.Contains(text(evs), "Synthesis failed - see individual findings") || !strings.Contains(text(evs), "**Confidence**: 0%") {
		t.Errorf("%q", text(evs))
	}
	st, _ := r.a.Checkpoints.Load(context.Background(), "s1")
	if len(st.Findings) != 4 || st.Findings[0].Hypothesis != "Could not parse subagent response" || st.Findings[0].Signals[0] != "(parse error)" {
		t.Errorf("%+v", st.Findings)
	}
	time.Sleep(200 * time.Millisecond)
	if len(r.mem.outcomes()) != 0 {
		t.Error("a zero confidence synthesis is not remembered")
	}
}

func TestTargetedInvestigationReturnsToTheCoordinator(t *testing.T) {
	r := newRig(t, `case "$1" in
describe) echo "Name: web-1 Last State: Terminated exit 1";;
*) printf 'NAMESPACE  NAME  READY  STATUS\nshop  web-1  0/1  CrashLoopBackOff\n';;
esac`, nil)
	r.coord.replies = []llm.Message{
		say("TARGETED: namespace=shop, pod=web-1, issue=exits with code 1"),
		say("The command exits 1."),
	}
	evs := r.turn(t, "why is web-1 failing")
	if text(evs) != "The command exits 1." {
		t.Errorf("%q", text(evs))
	}
	second := r.coord.seen[1][0].Content
	if !strings.Contains(second, "## Targeted Investigation: web-1 in shop") || !strings.Contains(second, "Last State: Terminated exit 1") {
		t.Errorf("the second pass sees the reads")
	}
	st, _ := r.a.Checkpoints.Load(context.Background(), "s1")
	if st.Targeted != nil {
		t.Error("cleared")
	}
	if !contains(st.MatchedPlaybooks, "CrashLoopBackOff") {
		t.Errorf("playbooks matched from the snapshot: %v", st.MatchedPlaybooks)
	}
	if !strings.Contains(second, "Recognized failure patterns") || !strings.Contains(second, "### CrashLoopBackOff") {
		t.Error("matched playbooks are rendered into the prompt")
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestTargetedInvestigationRefusesProtectedNamespace(t *testing.T) {
	r := newRig(t, okKubectl, nil)
	r.coord.replies = []llm.Message{say("TARGETED: namespace=kube-system, pod=coredns-1, issue=crashing"), say("protected")}
	r.turn(t, "why is coredns failing")
	second := r.coord.seen[1][0].Content
	if !strings.Contains(second, "No pod description, events or deployments were read") || !strings.Contains(second, "[Protected]") {
		t.Errorf("%s", second[len(second)-400:])
	}
	if b, _ := os.ReadFile(r.log); strings.Contains(string(b), "coredns") {
		t.Error("nothing may be read")
	}
}

func TestGraphBudgetEscalates(t *testing.T) {
	r := newRig(t, okKubectl, map[string]string{"AGENT_GRAPH_RECURSION_LIMIT": "6"})
	r.coord.next = func([]llm.Message) *llm.Message {
		m := say("TARGETED: namespace=shop, pod=web-1, issue=x")
		return &m
	}
	evs := r.turn(t, "loop")
	if !strings.Contains(text(evs), "overall step budget (6 recursion units)") {
		t.Errorf("%q", text(evs))
	}
}

func TestSnapshotFailureIsNotAHealthyCluster(t *testing.T) {
	r := newRig(t, `echo "error: You must be logged in to the server (Unauthorized)" >&2; exit 1`, nil)
	r.coord.replies = []llm.Message{say("cannot tell")}
	r.turn(t, "is the cluster healthy?")
	st, _ := r.a.Checkpoints.Load(context.Background(), "s1")
	if !st.SnapshotReadFailed || st.SnapshotPodCount != 0 || st.SnapshotHasIssues || len(st.MatchedPlaybooks) != 0 {
		t.Errorf("%+v", st)
	}
	sys := r.coord.seen[0][0].Content
	if !strings.Contains(sys, "UNAVAILABLE") || !strings.Contains(sys, "Unauthorized") || !strings.Contains(sys, "not an empty or healthy cluster") ||
		!strings.Contains(sys, "This is not zero pods; it is unknown") {
		t.Error("the prompt must say the snapshot is unavailable")
	}
	if strings.Contains(sys, "contains 0 pods") {
		t.Error("no pod count is asserted")
	}
}

func TestMemoryContextIsPinned(t *testing.T) {
	r := newRig(t, okKubectl, nil)
	r.mem.load = "## Preferences\nlikes terse answers"
	r.coord.replies = []llm.Message{say("ok")}
	r.turn(t, "hi")
	if !strings.Contains(r.coord.seen[0][0].Content, "## Cluster Context\n## Preferences\nlikes terse answers") {
		t.Error("memory context missing from the prompt")
	}
}

func TestDirectFixIsRecordedWithVerification(t *testing.T) {
	r := newRig(t, `case "$1 $2" in
"get pods") printf 'NAME  READY  STATUS\nweb-1  1/1  Running\n';;
"get events") echo "No resources found";;
"get deployment") echo "kind: Deployment";;
*) echo patched;;
esac`, nil)
	r.coord.replies = []llm.Message{
		toolCall("c1", ToolKubectl, map[string]any{"command": "patch deployment web -n shop -p {}"}),
		say("patched"),
	}
	r.turn(t, "fix web", func(q *TurnRequest) { q.AutoApprove = true })
	deadline := time.Now().Add(5 * time.Second)
	for len(r.mem.outcomes()) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	outs := r.mem.outcomes()
	if len(outs) != 1 {
		t.Fatalf("outcomes: %+v", outs)
	}
	o := outs[0]
	if o.Namespace != "shop" || o.Verified == nil || !*o.Verified || derefStr(o.Feedback) != "resolved" || o.Confidence != 0.7 ||
		!strings.HasPrefix(o.RootCause, "query=fix web | cluster=test-cluster") || o.SessionID != "s1" {
		t.Errorf("%+v", o)
	}
}

func TestReadOnlyTurnRecordsNothing(t *testing.T) {
	r := newRig(t, okKubectl, nil)
	r.coord.replies = []llm.Message{toolCall("c", ToolKubectl, map[string]any{"command": "get pods -n shop"}), say("fine")}
	r.turn(t, "look")
	time.Sleep(200 * time.Millisecond)
	if len(r.mem.outcomes()) != 0 {
		t.Error("reads teach nothing")
	}
}

func TestUsageIsMeteredAcrossTheTurn(t *testing.T) {
	r := newRig(t, okKubectl, nil)
	r.coord.replies = []llm.Message{toolCall("c", ToolKubectl, map[string]any{"command": "get pods -n shop"}), say("fine")}
	r.emitter.Prepare("s1")
	ctx, meter := llm.WithMeter(context.Background())
	r.a.Run(ctx, TurnRequest{Message: "x", SessionID: "s1", UserID: "u", UserRole: "admin"})
	if p, c, calls := meter.Snapshot(); calls != 2 || p != 20 || c != 10 {
		t.Errorf("%d %d %d", p, c, calls)
	}
}

type failing struct{ err error }

func (f failing) Chat(context.Context, []llm.Message, []llm.ToolSpec, llm.Options) (*llm.Response, error) {
	return nil, f.err
}

func TestErrorBecomesAnErrorEvent(t *testing.T) {
	r := newRig(t, okKubectl, nil)
	r.a.Coordinator = failing{&llm.Error{Status: 401, Body: "bad key"}}
	evs := r.turn(t, "hi")
	e := types(evs, "error")
	if len(e) != 1 || !strings.Contains(e[0]["error"].(string), "LLM authentication failed") {
		t.Errorf("%v", evs)
	}
	if h := r.emitter.History("s1"); h[len(h)-1]["type"] != "final" {
		t.Error("the stream always ends")
	}
	st, _ := r.a.Checkpoints.Load(context.Background(), "s1")
	if st == nil || len(st.Messages) != 1 {
		t.Error("the question is kept even when the turn fails")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestLLMErrorHints(t *testing.T) {
	cases := map[string]string{
		"LLM request failed: HTTP 429: rate limit": "rate limit",
		"boom: connection refused":                 "connection failed",
		"content_filter triggered":                 "content filter",
		"weird":                                    "LLM error: weird",
	}
	for in, want := range cases {
		if got := LLMErrorHint(errString(in)); !strings.Contains(got, want) {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestHitlPhrases(t *testing.T) {
	yes := []string{"yes", "Yes.", "  APPROVE!  ", "go ahead", `"ok"`, "run it"}
	no := []string{"no", "No.", "cancel", "don't", "dont"}
	other := []string{"", "maybe", "yes please", "not yet", "why?"}
	for _, s := range yes {
		if !IsApproval(s) {
			t.Errorf("%q approves", s)
		}
	}
	for _, s := range no {
		if IsApproval(s) || !IsDenial(s) {
			t.Errorf("%q denies", s)
		}
	}
	for _, s := range other {
		if IsApproval(s) || IsDenial(s) {
			t.Errorf("%q is neither, and neither means cancel", s)
		}
	}
	if !IsAutoApproveRequest("Approve ALL.") || !IsAutoApproveRequest("/auto-approve") || IsAutoApproveRequest("approve") {
		t.Error("auto approve phrases")
	}
}

func TestScanSnapshotIgnoresErrorsAndPolicyLines(t *testing.T) {
	pods := "NAMESPACE NAME READY STATUS RESTARTS AGE\nshop a 1/1 Running 0 1d\nshop b 0/1 CrashLoopBackOff 5 1d\n[Protected] 2 row(s) withheld: they belong to a namespace in KUBECTL_BLOCKED_NAMESPACES. This listing is NOT the complete set.\n[truncated: 9 chars omitted]\n"
	issues, warnings, n := ScanSnapshot(pods, "", true, true)
	if !issues || warnings || n != 2 {
		t.Errorf("%v %v %d", issues, warnings, n)
	}
	if i, w, n := ScanSnapshot("error: You must be logged in", "", false, true); i || w || n != 0 {
		t.Error("a failed read is unknown, not empty")
	}
	if i, _, n := ScanSnapshot("error: Unauthorized", "", true, true); i || n != 0 {
		t.Error("a one line error is consumed as the header")
	}
	if _, w, _ := ScanSnapshot(pods, "NAMESPACE LAST TYPE\n[Protected] 3 row(s) withheld: x\n", true, true); w {
		t.Error("a header and a notice are not a warning")
	}
	if _, w, _ := ScanSnapshot(pods, "NAMESPACE LAST TYPE REASON\nshop 1m Warning BackOff\n", true, true); !w {
		t.Error("a real warning row")
	}
	if _, w, _ := ScanSnapshot(pods, "No resources found", true, true); w {
		t.Error("no resources")
	}
	if _, _, n := ScanSnapshot("NAME READY STATUS\nweb 1/1 Running\n", "", true, true); n != 1 {
		t.Error("the status column is found by header")
	}
}

func TestCapWithNoticesKeepsNoticesAndCutsOnLines(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		b.WriteString("shop app-1 1/1 Running 0 1d\n")
	}
	out := capWithNotices(b.String(), 3)
	if !strings.Contains(out, "3 row(s) withheld") || !strings.Contains(out, "[truncated:") {
		t.Errorf("both notices survive the cap: %s", out[len(out)-300:])
	}
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "shop") && ln != "shop app-1 1/1 Running 0 1d" {
			t.Errorf("a severed row: %q", ln)
		}
	}
	if issues, _, _ := ScanSnapshot("NAMESPACE NAME READY STATUS\n"+out, "", true, true); issues {
		t.Error("a cut listing of healthy pods must not invent an issue")
	}
	short := capWithNotices("NAME\nweb\n", 0)
	if short != "NAME\nweb\n" {
		t.Errorf("%q", short)
	}
}

func TestTrimSessionKeepsExchangesWhole(t *testing.T) {
	var msgs []llm.Message
	for i := 0; i < 8; i++ {
		msgs = append(msgs,
			llm.Message{Role: llm.User, Content: "q"},
			llm.Message{Role: llm.Assistant, ToolCalls: []llm.ToolCall{{ID: "x", Name: ToolKubectl, Args: map[string]any{"command": "get pods"}}}},
			llm.Message{Role: llm.Tool, ToolCallID: "x", Content: "line one\nline two"},
			llm.Message{Role: llm.Assistant, Content: "answer"},
		)
	}
	kept, summary := trimSession(msgs)
	if len(kept) > maxSessionMessages || kept[0].Role != llm.User {
		t.Errorf("must start at a user message: %d %s", len(kept), kept[0].Role)
	}
	if !strings.Contains(summary, "Earlier Session Context") || !strings.Contains(summary, "- Ran: get pods") || !strings.Contains(summary, "-> line one") {
		t.Errorf("%s", summary)
	}
	if k, s := trimSession(msgs[:5]); len(k) != 5 || s != "" {
		t.Error("short history is untouched")
	}
}

func TestTrimToolOutputSaysWhatItRemoved(t *testing.T) {
	var b strings.Builder
	b.WriteString("NAMESPACE NAME READY STATUS\n")
	for i := 0; i < 200; i++ {
		b.WriteString("shop pod-x 1/1 Running\n")
	}
	b.WriteString("shop bad 0/1 CrashLoopBackOff\n")
	b.WriteString("[Protected] 4 row(s) withheld: they belong to a namespace in KUBECTL_BLOCKED_NAMESPACES. This listing is NOT the complete set.\n")
	out := trimToolOutput(b.String())
	if !strings.Contains(out, "CrashLoopBackOff") || !strings.Contains(out, "170 row(s) omitted from LLM context") || !strings.Contains(out, "NOT the complete set") {
		t.Errorf("%s", out)
	}
	if !strings.HasSuffix(out, "complete set.") {
		t.Error("the tool's own notice goes last")
	}
	if got := trimToolOutput("short"); got != "short" {
		t.Error("short output is untouched")
	}
	var logs strings.Builder
	for i := 0; i < 300; i++ {
		logs.WriteString("log line\n")
	}
	if out := trimToolOutput(logs.String()); !strings.Contains(out, "240 line(s) omitted") {
		t.Errorf("%s", out[len(out)-120:])
	}
}

func TestFillOrphansAndPlanParsing(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.Assistant, ToolCalls: []llm.ToolCall{{ID: "a"}, {ID: "b"}}},
		{Role: llm.Tool, ToolCallID: "a", Content: "ok"},
	}
	out := fillOrphanToolCalls(msgs)
	if len(out) != 3 || out[2].ToolCallID != "b" || !strings.Contains(out[2].Content, "Skipped") {
		t.Errorf("%+v", out)
	}
	plan, cleaned := extractPlan([]llm.Message{{Role: llm.Assistant, Content: "INVESTIGATION_PLAN:\n1. a\n2. b\n"}})
	if len(plan) != 0 || cleaned[0].Content == "" {
		t.Error("fewer than three steps is not a plan")
	}
	plan, cleaned = extractPlan([]llm.Message{{Role: llm.Assistant, Content: "INVESTIGATION_PLAN:\n- a\n* b\n1. c\n\nThen the answer"}})
	if len(plan) != 3 || plan[2].Description != "c" || !strings.Contains(cleaned[0].Content, "Then the answer") || strings.Contains(cleaned[0].Content, "INVESTIGATION") {
		t.Errorf("%+v %+v", plan, cleaned)
	}
}

func TestOutcomeHelpers(t *testing.T) {
	cmds := []string{"kubectl patch deploy web -n shop", "apply -f -", "kubectl get pods"}
	var msgs []llm.Message
	for _, c := range cmds {
		msgs = append(msgs, llm.Message{Role: llm.Assistant, ToolCalls: []llm.ToolCall{{Name: ToolKubectl, Args: map[string]any{
			"command": c, "stdin": "metadata:\n  namespace: prod\n  namespace: prod\n"}}}})
	}
	ran := ranMutation(msgs)
	if len(ran) != 2 {
		t.Fatalf("%v", ran)
	}
	a := newRig(t, okKubectl, nil).a
	pairs := a.mutationPairs(msgs)
	if ns := inferNamespace(ran, pairs); ns != "prod" {
		t.Errorf("the most cited namespace across args and yaml: %q", ns)
	}
	st := &State{ClusterID: "c1", MatchedPlaybooks: []string{"Evicted", "CrashLoopBackOff", "Evicted"}}
	if k := outcomeKey(st, "shop"); k != "playbook=CrashLoopBackOff+Evicted | ns=shop | cluster=c1" {
		t.Errorf("%s", k)
	}
	st = &State{Messages: []llm.Message{{Role: llm.User, Content: "fix\nthe thing"}}}
	if k := outcomeKey(st, ""); k != "query=fix the thing | cluster=unknown" {
		t.Errorf("%s", k)
	}
	tr := true
	if resolveConfidence(true, &tr) != 0.9 || resolveConfidence(false, &tr) != 0.7 || resolveConfidence(true, nil) != 0.7 || resolveConfidence(false, nil) != 0.5 {
		t.Error("confidence table")
	}
}
