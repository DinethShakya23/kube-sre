package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/DinethShakya23/kube-sre/internal/events"
	"github.com/DinethShakya23/kube-sre/internal/llm"
)

// freshTurn resets the per turn fields and appends the user's message to the
// conversation. Findings are cleared so a stale RCA never bleeds into the next turn.
func freshTurn(prev *State, req TurnRequest) *State {
	st := &State{
		SessionID: req.SessionID, UserID: req.UserID, UserRole: req.UserRole,
		ClusterID: "unknown", SnapshotComplete: true,
		// Untrusted by default; only an in process caller sets it.
		TriggerSource: "user_query",
	}
	if req.TriggerSource != "" {
		st.TriggerSource = req.TriggerSource
	}
	if prev != nil {
		st.Messages = append(st.Messages, prev.Messages...)
	}
	st.Messages = append(st.Messages, llm.Message{Role: llm.User, Content: req.Message})
	return st
}

func (a *Agent) save(ctx context.Context, st *State) {
	if a.Checkpoints == nil {
		return
	}
	// Saved even when the turn's own context is already cancelled: the state is
	// the conversation.
	sctx := context.WithoutCancel(ctx)
	if err := a.Checkpoints.Save(sctx, st.SessionID, st); err != nil {
		slog.Error("saving conversation state failed", "session", st.SessionID, "err", err)
	}
}

// loadMemory is the memory loader node.
func (a *Agent) loadMemory(ctx context.Context, st *State) {
	sid := st.SessionID
	a.emit(sid, events.NewStatus(sid, "loading", "Loading conversation context…"))
	st.ClusterID = a.ClusterID(ctx)
	st.MemoryContext = a.Memory.Load(ctx, LoadRequest{
		UserID: st.UserID, SessionID: sid, ClusterID: st.ClusterID, Query: lastUserText(st),
	})
	st.Findings = nil
}

// fetchContext is the context fetcher node.
func (a *Agent) fetchContext(ctx context.Context, st *State) {
	sid := st.SessionID
	a.emit(sid, events.NewStatus(sid, "snapshot", "Fetching cluster snapshot…"))
	snap := a.Snapshot.Fetch(ctx)
	st.ClusterSnapshot = snap.Text
	st.SnapshotHasIssues, st.SnapshotHasWarnings, st.SnapshotPodCount = snap.HasIssues, snap.HasWarnings, snap.PodCount
	st.SnapshotReadFailed, st.SnapshotComplete = snap.ReadFailed, snap.Complete
	st.SnapshotBuiltAt = float64(a.Now().UnixNano()) / 1e9
	if a.Cfg.Playbooks && a.Playbooks != nil {
		// Matching against stderr would fire playbooks on the text of an error.
		pods, evs := "", ""
		if snap.PodsOK {
			pods = snap.PodsOut
		}
		if snap.EventsOK {
			evs = snap.EventsOut
		}
		st.MatchedPlaybooks = a.Playbooks.Match(pods, evs)
	}
	slog.Info("snapshot complete", "session", sid, "chars", len(snap.Text), "pods", snap.PodCount, "issues", snap.HasIssues,
		"warnings", snap.HasWarnings, "read_failed", snap.ReadFailed, "complete", snap.Complete, "playbooks", st.MatchedPlaybooks)
}

// runGraph drives a turn to its end or to an approval pause. resume is set when
// continuing a paused turn.
//
// A turn costs three steps and two more for each coordinator and investigation
// cycle, and AGENT_GRAPH_RECURSION_LIMIT bounds the total, so a turn that keeps
// re-opening cannot run forever.
func (a *Agent) runGraph(ctx context.Context, st *State, bypass bool, resume *bool) error {
	steps := 3
	if resume == nil {
		a.loadMemory(ctx, st)
		a.fetchContext(ctx, st)
		a.save(ctx, st)
	}
	for {
		if steps > a.Cfg.GraphRecursionLimit {
			slog.Error("graph budget exhausted", "session", st.SessionID, "limit", a.Cfg.GraphRecursionLimit)
			msg := fmt.Sprintf(graphBudgetExhaustedMessage, a.Cfg.GraphRecursionLimit)
			a.emit(st.SessionID, events.NewToken(st.SessionID, msg))
			st.Messages = append(st.Messages, llm.Message{Role: llm.Assistant, Content: msg})
			return nil
		}
		paused, err := a.coordinate(ctx, st, bypass, resume)
		resume = nil
		if err != nil {
			return err
		}
		if paused {
			return nil
		}
		switch {
		case st.Targeted != nil:
			a.targetedInvestigate(ctx, st)
			steps += 2
		case st.RCARequired:
			a.fanOut(ctx, st)
			steps += 2
		case st.RCAResult != nil:
			return nil
		case len(st.Findings) > 0:
			steps++ // subagents wrote findings the coordinator has not synthesized yet
		default:
			return nil
		}
	}
}

// Run executes one turn and streams its events, ending the session's stream. It
// never returns an error: a failure becomes an error event.
func (a *Agent) Run(ctx context.Context, req TurnRequest) {
	sid := req.SessionID
	defer a.Emitter.Close(sid)
	if _, err := a.turn(ctx, req); err != nil {
		slog.Error("turn failed", "session", sid, "err", err)
		a.emit(sid, events.NewError(sid, LLMErrorHint(err)))
	}
}

// Invoke executes one turn and returns the final state.
func (a *Agent) Invoke(ctx context.Context, req TurnRequest) (*State, error) {
	return a.turn(ctx, req)
}

func (a *Agent) turn(ctx context.Context, req TurnRequest) (*State, error) {
	sid := req.SessionID
	bypass := req.AutoApprove
	// "approve all" bypasses approval for THIS turn only. Nothing persists it:
	// bypass is rebuilt from the request on every call, so the next turn starts
	// gated again. The gap is in the safe direction.
	if IsAutoApproveRequest(req.Message) {
		bypass = true
		slog.Info("approval bypassed for this turn", "session", sid)
	}
	var prev *State
	if a.Checkpoints != nil {
		var err error
		if prev, err = a.Checkpoints.Load(ctx, sid); err != nil {
			return nil, fmt.Errorf("loading conversation state: %w", err)
		}
	}

	if prev != nil && prev.Pending != nil {
		// The reply is an answer to the pending action. An approval must be
		// recognised; anything else cancels it. "approve all" approves the pending
		// action as well as enabling bypass: cancelling the very action the user
		// just approved would be a new bug in the other direction.
		approved := IsApproval(req.Message) || IsAutoApproveRequest(req.Message)
		if !approved && !IsDenial(req.Message) {
			slog.Warn("approval reply not recognised, cancelling", "session", sid, "reply", clip(req.Message, 80))
		}
		slog.Info("resuming paused turn", "session", sid, "approved", approved)
		prev.UserRole = firstNonEmpty(req.UserRole, prev.UserRole)
		err := a.runGraph(ctx, prev, bypass, &approved)
		a.save(ctx, prev)
		return prev, err
	}

	st := freshTurn(prev, req)
	a.save(ctx, st)
	err := a.runGraph(ctx, st, bypass, nil)
	a.save(ctx, st)
	return st, err
}

func firstNonEmpty(items ...string) string {
	for _, s := range items {
		if s != "" {
			return s
		}
	}
	return ""
}

// LLMErrorHint words a model error for a person.
func LLMErrorHint(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "missing an 'http://' or 'https://'") || strings.Contains(msg, "unsupported protocol"):
		return "LLM connection failed: AZURE_OPENAI_ENDPOINT is missing the protocol. Set it to https://... in ~/.kube-sre/.env and restart."
	case strings.Contains(msg, "authentication") || strings.Contains(msg, "401") || strings.Contains(msg, "api key"):
		return "LLM authentication failed: check your API key in ~/.kube-sre/.env."
	case strings.Contains(msg, "connection error") || strings.Contains(msg, "connection refused"):
		return "LLM connection failed: check your endpoint URL and network connectivity."
	case strings.Contains(msg, "rate limit") || strings.Contains(msg, "429"):
		return "LLM rate limit hit - please try again in a moment."
	case strings.Contains(msg, "content_filter") || strings.Contains(msg, "content management policy") || strings.Contains(msg, "responsibleaipolicyviolation"):
		return "Azure content filter blocked this request. Try rephrasing - if the issue persists, start a new session (/new) to reset conversation history."
	}
	return "LLM error: " + err.Error()
}
