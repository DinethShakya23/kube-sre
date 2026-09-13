package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/DinethShakya23/kube-sre/internal/events"
	"github.com/DinethShakya23/kube-sre/internal/kube"
	"github.com/DinethShakya23/kube-sre/internal/llm"
)

var rcaDomains = []string{"pod", "metrics", "logs", "events"}

// A subagent step is about three recursion units, so 50 allows roughly 16 tool
// calls before it gives up.
const subagentRecursionLimit = 50

// runSubagent runs one specialist's tool loop and returns its structured finding.
// Subagents investigate; they do not act, so their tools run as a read only role
// and a write they try is refused instead of waiting for an approval nobody can
// give inside a parallel branch.
func (a *Agent) runSubagent(ctx context.Context, st *State, domain, query string) Finding {
	parts := []string{domainPrompts[domain]}
	if st.MemoryContext != "" {
		parts = append(parts, "\n\n## Cluster Context\n"+st.MemoryContext)
	}
	if st.ClusterSnapshot != "" {
		parts = append(parts, "\n\n## Shared Evidence Bundle\n"+st.ClusterSnapshot)
	}
	parts = append(parts, findingSchemaHint)
	system := llm.Message{Role: llm.System, Content: strings.Join(parts, "\n")}
	// Each subagent gets only the current question, not the session history: it
	// would bloat their context and make them answer in prose instead of JSON.
	loop := []llm.Message{{Role: llm.User, Content: query}}
	call := kube.Call{Role: "readonly", SessionID: st.SessionID}
	specs := a.Tools.Specs()
	hooks := a.hooks(st.SessionID)
	maxRounds := subagentRecursionLimit / unitsPerStep

	var content string
	for round := 0; ; round++ {
		if round >= maxRounds {
			content = ""
			break
		}
		resp, err := a.Subagent.Chat(ctx, append([]llm.Message{system}, loop...), specs, llm.Options{MaxTokens: llm.Subagent.MaxTokens()})
		if err != nil {
			slog.Warn("subagent model call failed", "domain", domain, "err", err)
			return Finding{Domain: domain, Signals: []string{"(model error)"}, Hypothesis: "Subagent could not run: " + err.Error(),
				Evidence: []string{clip(err.Error(), 500)}}
		}
		loop = append(loop, resp.Message)
		if len(resp.Message.ToolCalls) == 0 {
			content = resp.Message.Content
			break
		}
		msgs, held, _ := a.Tools.runBatch(ctx, resp.Message.ToolCalls, call, hooks)
		loop = append(loop, msgs...)
		if held != nil {
			// A read only role is refused before it gets here; this is defence in depth.
			loop = append(loop, llm.Message{Role: llm.Tool, ToolCallID: held.ID, Name: held.Name,
				Content: "Not allowed: subagents cannot make changes."})
		}
	}

	var f Finding
	if err := json.Unmarshal([]byte(stripFence(content)), &f); err != nil || f.Confidence < 0 || f.Confidence > 1 {
		if err == nil {
			err = fmt.Errorf("confidence %v is outside 0 to 1", f.Confidence)
		}
		slog.Warn("subagent finding did not parse", "domain", domain, "err", err)
		return Finding{Domain: domain, Signals: []string{"(parse error)"}, Hypothesis: "Could not parse subagent response",
			Confidence: 0, Evidence: []string{clip(content, 500)}}
	}
	if f.Domain == "" {
		f.Domain = domain
	}
	slog.Debug("subagent finished", "domain", domain, "confidence", f.Confidence)
	return f
}

// fanOut runs the four specialists in parallel and stores their findings.
func (a *Agent) fanOut(ctx context.Context, st *State) {
	query := ""
	for i := len(st.Messages) - 1; i >= 0; i-- {
		if st.Messages[i].Role == llm.User {
			query = st.Messages[i].Content
			break
		}
	}
	stopBeat := a.heartbeat(ctx, st.SessionID, "investigating", "Investigation still running…")
	defer stopBeat()
	if a.harnessFanout(ctx, st, query) {
		return
	}
	slog.Info("fanning out to specialist subagents", "count", len(rcaDomains), "session", st.SessionID)
	findings := make([]Finding, len(rcaDomains))
	var wg sync.WaitGroup
	for i, d := range rcaDomains {
		wg.Add(1)
		go func(i int, d string) {
			defer wg.Done()
			a.emit(st.SessionID, events.NewStatus(st.SessionID, "investigating", fmt.Sprintf("Running %s diagnostics…", d)))
			findings[i] = a.runSubagent(ctx, st, d, query)
		}(i, d)
	}
	wg.Wait()
	st.Findings, st.RCARequired = findings, false
}
