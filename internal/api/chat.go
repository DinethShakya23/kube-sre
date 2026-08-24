package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/agent"
	"github.com/DinethShakya23/kube-sre/internal/audit"
	"github.com/DinethShakya23/kube-sre/internal/events"
	"github.com/DinethShakya23/kube-sre/internal/llm"
)

// chat is POST /v1/chat/completions, an OpenAI compatible SSE stream:
//
//	data: {"id":"...","object":"chat.completion.chunk","choices":[{"delta":{"content":"..."},"index":0}]}
//	data: [DONE]
//
// The first frame is a stream.start handshake with the protocol version. SSE
// keepalive comments go out every 15s of silence. Side channel events ride in
// ki_event frames with empty choices. An approval request is embedded in a content
// chunk with hitl_required, action_id, risk_level and human_summary fields.
func (s *Server) chat(w http.ResponseWriter, r *http.Request, role string) {
	var body struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Stream      *bool  `json:"stream"`
		User        string `json:"user"`
		AutoApprove bool   `json:"auto_approve"` // skip approval for this request
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, &HTTPError{422, "invalid request body"})
		return
	}
	var message string
	found := false
	for _, m := range body.Messages {
		if m.Role == "user" {
			message, found = m.Content, true
		}
	}
	if !found {
		writeError(w, &HTTPError{422, "No user message provided"})
		return
	}
	// X-Session-ID ties the request to a conversation, which is what enables approval resume.
	session := r.Header.Get("X-Session-ID")
	if session == "" {
		session = newID()
	}
	user := body.User
	if user == "" {
		user = "default"
	}
	slog.Info("chat completions", "session", session, "user", user, "role", role, "msg", clipRunes(message, 80),
		"request_id", RequestID(r.Context()))
	if body.Stream != nil && !*body.Stream {
		writeError(w, &HTTPError{422, "Only stream=true is supported"})
		return
	}
	started := time.Now()
	s.stream(w, r, agent.TurnRequest{
		Message: message, SessionID: session, UserID: user, UserRole: role, AutoApprove: body.AutoApprove,
	}, started)
}

func sse(w http.ResponseWriter, payload any) {
	b, _ := json.Marshal(payload)
	fmt.Fprintf(w, "data: %s\n\n", b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func chunk(id, content string, finish *string, extra map[string]any) map[string]any {
	delta := map[string]any{}
	if content != "" {
		delta = map[string]any{"content": content, "role": "assistant"}
	}
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": finish}
	for k, v := range extra {
		choice[k] = v
	}
	return map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"model": "kube-sre", "choices": []any{choice},
	}
}

func kiEvent(id string, ev map[string]any) map[string]any {
	return map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"model": "kube-sre", "ki_event": ev, "choices": []any{},
	}
}

func strp(s string) *string { return &s }

// frame converts a typed event to an SSE payload, or nil to skip it. The final
// event is handled by the loop ending.
func frame(id string, ev map[string]any) map[string]any {
	typ, _ := ev["type"].(string)
	switch typ {
	case "status":
		return kiEvent(id, map[string]any{"type": "status", "phase": ev["phase"], "message": ev["message"]})
	case "tool_call":
		msg := fmt.Sprintf("Calling %v", ev["tool"])
		if c, ok := ev["command"].(string); ok && c != "" {
			msg = "Running: " + c
		}
		return kiEvent(id, map[string]any{"type": "tool_call", "tool": ev["tool"], "message": msg})
	case "tool_result":
		return kiEvent(id, map[string]any{"type": "tool_result", "tool": ev["tool"], "output": ev["output"]})
	case "token":
		c, _ := ev["content"].(string)
		return chunk(id, c, nil, nil)
	case "plan":
		return kiEvent(id, map[string]any{"type": "plan", "steps": ev["steps"]})
	case "hitl_request":
		risk, _ := ev["risk_level"].(string)
		command, _ := ev["command"].(string)
		actionID, _ := ev["action_id"].(string)
		marker := "🟡"
		if risk == "high" {
			marker = "🔴"
		}
		msg := fmt.Sprintf("\n\n---\n%s **Approval Required** - risk level: `%s`\n\n**Command:**\n```\n%s\n```\n", marker, strings.ToUpper(risk), command)
		if y, ok := ev["stdin_yaml"].(string); ok && y != "" {
			preview := y
			if r := []rune(y); len(r) > 3000 {
				preview = string(r[:3000]) + "\n... [truncated]"
			}
			msg += "\n**YAML to apply:**\n```yaml\n" + preview + "\n```\n"
		}
		msg += "\n**Type `yes` or `/approve` to proceed, or `no` / `/deny` to cancel.**"
		return chunk(id, msg, nil, map[string]any{
			"hitl_required": true, "action_id": actionID, "risk_level": risk, "human_summary": command,
		})
	case "error":
		return chunk(id, fmt.Sprintf("\n\n**Error:** %v", ev["error"]), strp("stop"), nil)
	}
	return nil
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request, req agent.TurnRequest, started time.Time) {
	completion := "chatcmpl-" + strings.ReplaceAll(newID(), "-", "")[:12]
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	// Reset the session queue for this turn, keeping accumulated history.
	s.Emitter.Prepare(req.SessionID)

	sse(w, map[string]any{"protocol_version": events.ProtocolVersion, "object": "stream.start", "session_id": req.SessionID})

	// The meter is bound before the turn starts, and only ever mutated, so tokens
	// from every branch of the turn land in the same total.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	ctx, meter := llm.WithMeter(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Agent.Run(ctx, req)
	}()

	for it := range s.Emitter.Stream(ctx, req.SessionID, 15*time.Second) {
		if it.Heartbeat {
			fmt.Fprint(w, ": heartbeat\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			continue
		}
		if p := frame(completion, it.Event); p != nil {
			sse(w, p)
		}
	}
	// A client that went away cancels the turn, and the turn is awaited so its
	// state is saved cleanly.
	cancel()
	<-done

	if r.Context().Err() == nil {
		// Usage goes out before the terminator so a client that stops at
		// finish_reason still sees it, and it is sent even when every count is zero:
		// "the model was called 40 times and reported no tokens" is an
		// instrumentation gap the caller must be able to tell from a cheap request.
		p, c, n := meter.Snapshot()
		sse(w, kiEvent(completion, events.Map(events.NewUsage(req.SessionID, p, c, n))))
		sse(w, chunk(completion, "", strp("stop"), nil))
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	if s.Audit != nil {
		s.Audit.Write(context.WithoutCancel(r.Context()), audit.Request{
			RequestID: RequestID(r.Context()), SessionID: req.SessionID, UserID: req.UserID, UserRole: req.UserRole,
			Path: r.URL.Path, Method: r.Method, StatusCode: 200,
			DurationMS: float64(time.Since(started).Milliseconds()),
		})
	}
}
