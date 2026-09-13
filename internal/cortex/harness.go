// Package cortex holds the opt in investigation features layered on the agent:
// the read only subagent harness, the verification ladder, escalation briefs,
// runbooks as skills, responsiveness, model routing and the change watchdog.
// Every one of them is default off and fails safe.
package cortex

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/DinethShakya23/kube-sre/internal/aci"
	"github.com/DinethShakya23/kube-sre/internal/llm"
)

// The subagent contract: a gather subagent runs in an isolated context, may call only
// the four ACI read verbs, never mutates, and returns a structured summary bounded to
// about 2k tokens. These are pure guards applied around whatever runs the loop, so
// "subagents isolate reads, a single writer mutates" is structural and not a prompt
// exhortation.

const (
	SummaryMaxTokens = 2000
	charsPerToken    = 4
	SummaryMaxChars  = SummaryMaxTokens * charsPerToken
)

// ContractError is a subagent configured with a tool outside the read only allowlist.
type ContractError struct{ Msg string }

func (e *ContractError) Error() string { return e.Msg }

// Contract is what a dispatched investigation subagent is allowed to do.
type Contract struct {
	Objective        string
	AllowedVerbs     map[string]bool
	MaxSummaryTokens int
	MayMutate        bool // always false for investigation subagents
}

// NewContract builds a read only contract and validates it against the allowlist.
func NewContract(objective string) (Contract, error) {
	c := Contract{Objective: objective, AllowedVerbs: aci.ReadVerbAllowlist(), MaxSummaryTokens: SummaryMaxTokens}
	return c, c.Validate()
}

// Validate refuses a contract that could mutate or call a verb off the allowlist.
func (c Contract) Validate() error {
	if err := EnforceReadOnly(c.AllowedVerbs); err != nil {
		return err
	}
	if c.MayMutate {
		return &ContractError{"investigation subagents must not mutate"}
	}
	return nil
}

// EnforceReadOnly errors unless every requested verb is one of the four read verbs.
func EnforceReadOnly(verbs map[string]bool) error {
	allow := aci.ReadVerbAllowlist()
	var extra []string
	for v := range verbs {
		if !allow[v] {
			extra = append(extra, v)
		}
	}
	if len(extra) > 0 {
		return &ContractError{fmt.Sprintf("verbs %v are outside the read-only allowlist %v", extra, aci.ReadVerbs)}
	}
	return nil
}

// Result is the bounded summary a subagent returns to the lead.
type Result struct {
	Objective, Summary string
	Truncated          bool
	VerbsUsed          []string
}

// TruncationMarker is the one shape a truncation marker takes.
func TruncationMarker(omitted int, hint string) string {
	tail := ""
	if hint != "" {
		tail = " — " + hint
	}
	return fmt.Sprintf("[truncated: %d chars omitted%s]", omitted, tail)
}

// BoundSummary caps a summary to the token budget without splitting a line where it
// can be avoided, and appends an explicit marker so the lead is never silently handed
// a clipped summary.
func BoundSummary(text string, maxTokens int) (string, bool) {
	max := maxTokens * charsPerToken
	if len(text) <= max {
		return text, false
	}
	cut := text[:max]
	for len(cut) > 0 && cut[len(cut)-1]&0xC0 == 0x80 { // never end inside a rune
		cut = cut[:len(cut)-1]
	}
	if i := strings.LastIndex(cut, "\n"); i > max/2 { // only prefer a line break if it is not wastefully early
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " \t\r\n") + "\n…" + TruncationMarker(len(text)-len(cut), "summary cut to fit the 2k-token subagent budget"), true
}

// Finalize applies the contract's bounds to a raw summary.
func Finalize(c Contract, raw string, verbsUsed []string) (Result, error) {
	used := map[string]bool{}
	for _, v := range verbsUsed {
		used[v] = true
	}
	if err := EnforceReadOnly(used); err != nil {
		return Result{}, err
	}
	sum, trunc := BoundSummary(raw, c.MaxSummaryTokens)
	return Result{c.Objective, sum, trunc, verbsUsed}, nil
}

const subagentSystem = "You are an isolated read-only investigation subagent. Your single objective:\n  %s\n\n" +
	"You may call ONLY these read-only verbs: %s. You cannot mutate the cluster. Gather just enough evidence to address the objective, then STOP and reply " +
	"with a concise finding (root-cause hypothesis + the specific evidence lines that support it). Do not pad.\n\n" +
	"CRITICAL — never conclude a resource is missing from a `search` alone: `search` matches by LABEL/selector, so an empty result usually means no label match, " +
	"NOT that the object is absent. Before ever stating a named object 'does not exist', confirm with `inspect` using its exact kind and name. Only an explicit " +
	"by-name lookup that returns NotFound proves absence. If a cluster snapshot is provided below, consult it FIRST — any resource listed there exists, so " +
	"investigate its state (logs, events, status), not its existence. If the evidence is genuinely inconclusive, say so plainly."

// PlanSubagents decomposes an investigation into read only contracts: one per
// actionable plan step, capped, or a single objective from the last user turn when
// there is no plan.
func PlanSubagents(steps []string, lastUser string, max int) []Contract {
	if max < 1 {
		return nil
	}
	if len(steps) == 0 {
		obj := strings.TrimSpace(lastUser)
		if r := []rune(obj); len(r) > 300 {
			obj = string(r[:300])
		}
		if obj == "" {
			obj = "investigate the reported issue"
		}
		steps = []string{obj}
	}
	var out []Contract
	for _, s := range steps {
		if len(out) == max {
			break
		}
		if c, err := NewContract(s); err == nil {
			out = append(out, c)
		}
	}
	return out
}

// RunSubagent runs one isolated read only investigator to a bounded summary. Any
// failure degrades to an error note result and never propagates, so one failed
// investigator never breaks the fan out.
func RunSubagent(ctx context.Context, model llm.Model, verbs aci.Verbs, c Contract, snapshot string, rounds int) Result {
	if rounds < 1 {
		rounds = 1
	}
	var used []string
	fail := func(err error) Result {
		slog.Warn("harness subagent failed", "objective", c.Objective, "err", err)
		r, _ := Finalize(c, fmt.Sprintf("[investigation error: %v]", err), used)
		return r
	}
	names := make([]string, 0, len(c.AllowedVerbs))
	for v := range c.AllowedVerbs {
		names = append(names, v)
	}
	sortStrings(names)
	msgs := []llm.Message{{Role: llm.System, Content: fmt.Sprintf(subagentSystem, c.Objective, strings.Join(names, ", "))}}
	if snapshot != "" {
		msgs = append(msgs, llm.Message{Role: llm.System, Content: "## Cluster snapshot\n" + snapshot})
	}
	msgs = append(msgs, llm.Message{Role: llm.User, Content: c.Objective})

	summary := ""
	for round := 0; round < rounds; round++ {
		resp, err := model.Chat(ctx, msgs, aci.Specs(), llm.Options{})
		if err != nil {
			return fail(err)
		}
		msgs = append(msgs, resp.Message)
		summary = resp.Message.Content
		if len(resp.Message.ToolCalls) == 0 {
			break
		}
		for _, tc := range resp.Message.ToolCalls {
			content := fmt.Sprintf("[blocked] %q is not a read-only verb", tc.Name)
			if c.AllowedVerbs[tc.Name] {
				content = verbs.Call(ctx, tc.Name, tc.Args)
				used = append(used, tc.Name)
			}
			msgs = append(msgs, llm.Message{Role: llm.Tool, ToolCallID: tc.ID, Name: tc.Name, Content: content})
		}
	}
	r, err := Finalize(c, summary, used)
	if err != nil {
		return fail(err)
	}
	return r
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Reconcile folds subagent findings into one evidence bundle for the lead.
func Reconcile(results []Result) string {
	blocks := make([]string, len(results))
	for i, r := range results {
		verbs := "none"
		if len(r.VerbsUsed) > 0 {
			verbs = strings.Join(r.VerbsUsed, ", ")
		}
		trunc := ""
		if r.Truncated {
			trunc = " (truncated)"
		}
		blocks[i] = fmt.Sprintf("### Finding %d: %s\n_read verbs used: %s%s_\n\n%s", i+1, r.Objective, verbs, trunc, strings.TrimSpace(r.Summary))
	}
	return strings.Join(blocks, "\n\n")
}

// Fanout runs the contracts in parallel and reconciles them. Usable is false when every
// investigator came back empty or failed, so the caller falls back to its flat gather
// and enabling the feature can never leave an investigation with no evidence.
// "Parallel diagnosis, serialised mutation": the investigators can only call the read
// verbs, so the fan out cannot mutate the cluster.
func Fanout(ctx context.Context, model llm.Model, verbs aci.Verbs, contracts []Contract, snapshot string, rounds int) (bundle string, results []Result, usable bool) {
	results = make([]Result, len(contracts))
	var wg sync.WaitGroup
	for i, c := range contracts {
		wg.Add(1)
		go func(i int, c Contract) {
			defer wg.Done()
			results[i] = RunSubagent(ctx, model, verbs, c, snapshot, rounds)
		}(i, c)
	}
	wg.Wait()
	for _, r := range results {
		s := strings.TrimSpace(r.Summary)
		if s != "" && !strings.HasPrefix(s, "[investigation error") {
			usable = true
		}
	}
	if !usable {
		slog.Info("harness fan-out produced no usable evidence; falling back to the flat gather")
		return "", results, false
	}
	return Reconcile(results), results, true
}
