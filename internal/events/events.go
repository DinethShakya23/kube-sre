// Package events is the typed event protocol for streaming: the flat wire shape
// a client reads off the SSE channel, and the per-session emitter behind it.
//
// Every event is a flat JSON object with a type, a session id and a timestamp.
// Wire format changes must be made here and in the client together.
package events

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

const ProtocolVersion = "1.0"

// Event is anything the emitter can carry.
type Event interface{ Kind() string }

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Status says which phase the server is in: loading, snapshot, analyzing,
// investigating, dispatching, synthesizing.
type Status struct {
	Type      string  `json:"type"`
	Phase     string  `json:"phase"`
	Message   string  `json:"message"`
	SessionID string  `json:"session_id"`
	TS        float64 `json:"ts"`
}

func NewStatus(session, phase, message string) Status {
	return Status{"status", phase, message, session, now()}
}
func (Status) Kind() string { return "status" }

type ToolCall struct {
	Type      string  `json:"type"`
	Tool      string  `json:"tool"`
	Command   *string `json:"command"` // set for run_kubectl
	SessionID string  `json:"session_id"`
	TS        float64 `json:"ts"`
}

func NewToolCall(session, tool string, command *string) ToolCall {
	return ToolCall{"tool_call", tool, command, session, now()}
}
func (ToolCall) Kind() string { return "tool_call" }

type ToolResult struct {
	Type      string  `json:"type"`
	Tool      string  `json:"tool"`
	Output    string  `json:"output"` // first 500 chars
	SessionID string  `json:"session_id"`
	TS        float64 `json:"ts"`
}

func NewToolResult(session, tool, output string) ToolResult {
	return ToolResult{"tool_result", tool, output, session, now()}
}
func (ToolResult) Kind() string { return "tool_result" }

type Token struct {
	Type      string  `json:"type"`
	Content   string  `json:"content"`
	SessionID string  `json:"session_id"`
	TS        float64 `json:"ts"`
}

func NewToken(session, content string) Token { return Token{"token", content, session, now()} }
func (Token) Kind() string                   { return "token" }

type Final struct {
	Type      string  `json:"type"`
	SessionID string  `json:"session_id"`
	TS        float64 `json:"ts"`
}

func NewFinal(session string) Final { return Final{"final", session, now()} }
func (Final) Kind() string          { return "final" }

type HitlRequest struct {
	Type      string  `json:"type"`
	ActionID  string  `json:"action_id"`
	RiskLevel string  `json:"risk_level"`
	Command   string  `json:"command"`
	StdinYAML *string `json:"stdin_yaml"`
	SessionID string  `json:"session_id"`
	TS        float64 `json:"ts"`
}

func NewHitlRequest(session, risk, command string, stdin *string) HitlRequest {
	return HitlRequest{"hitl_request", uuid4(), risk, command, stdin, session, now()}
}
func (HitlRequest) Kind() string { return "hitl_request" }

// Plan is emitted when the coordinator produces an investigation plan.
type Plan struct {
	Type      string           `json:"type"`
	Steps     []map[string]any `json:"steps"` // {description, status}
	SessionID string           `json:"session_id"`
	TS        float64          `json:"ts"`
}

func NewPlan(session string, steps []map[string]any) Plan {
	return Plan{"plan", steps, session, now()}
}
func (Plan) Kind() string { return "plan" }

type Error struct {
	Type      string  `json:"type"`
	Error     string  `json:"error"`
	SessionID string  `json:"session_id"`
	TS        float64 `json:"ts"`
}

func NewError(session, msg string) Error { return Error{"error", msg, session, now()} }
func (Error) Kind() string               { return "error" }

// Usage carries token and call counts for one request, emitted once before the
// terminator. llm_calls is kept so "called 40 times, reported no tokens", an
// instrumentation gap, stays distinguishable from a genuinely cheap request.
type Usage struct {
	Type             string  `json:"type"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	LLMCalls         int     `json:"llm_calls"`
	SessionID        string  `json:"session_id"`
	TS               float64 `json:"ts"`
}

func NewUsage(session string, prompt, completion, calls int) Usage {
	return Usage{"usage", prompt, completion, prompt + completion, calls, session, now()}
}
func (Usage) Kind() string { return "usage" }

// Map returns an event as the flat map that goes on the wire and into history.
func Map(e Event) map[string]any {
	b, _ := json.Marshal(e)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func uuid4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
