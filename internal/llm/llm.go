// Package llm is the chat model client: OpenAI compatible endpoints (OpenAI,
// Qwen/DashScope, LiteLLM and the like) and Azure OpenAI, with tool calling,
// streaming, retries, and per request token metering.
package llm

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/DinethShakya23/kube-sre/internal/metrics"
)

type Role string

const (
	System    Role = "system"
	User      Role = "user"
	Assistant Role = "assistant"
	Tool      Role = "tool"
)

// ToolCall is one tool invocation the model asked for.
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Args is the parsed argument object; nil when the model sent something that
	// is not a JSON object, in which case RawArgs holds what it sent.
	Args    map[string]any `json:"args,omitempty"`
	RawArgs string         `json:"raw_args,omitempty"`
}

// Message is one turn of a conversation.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // assistant messages
	ToolCallID string     `json:"tool_call_id,omitempty"` // tool messages
	Name       string     `json:"name,omitempty"`         // tool messages: the tool that produced it
	IsError    bool       `json:"is_error,omitempty"`     // tool messages: the call failed
}

// ToolSpec describes a tool to the model. Parameters is a JSON schema object.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// Usage is what one call cost.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

// Response is one completed model call.
type Response struct {
	Message Message
	Usage   Usage
}

// Options tunes one call.
type Options struct {
	// MaxTokens caps the completion. Zero leaves it to the provider.
	MaxTokens int
	// OnToken, when set, streams the reply and is called with each piece of text.
	OnToken func(string)
}

// Model is a chat model.
type Model interface {
	Chat(ctx context.Context, msgs []Message, tools []ToolSpec, opts Options) (*Response, error)
}

// Meter sums token usage across every model call made under one request.
//
// One request can make many calls: triage, the coordinator's tool loop, a
// four way subagent fan out, verification. The meter is bound once per request
// in the context and only ever mutated, never replaced, so a branch running in
// its own goroutine adds to the same total. Wrapping each call site would mean
// finding all of them and then finding each new one forever.
type Meter struct {
	mu         sync.Mutex
	prompt     int
	completion int
	calls      int
}

// Add records one call. A provider that reports no usage still counts a call,
// so "called 40 times, reported no tokens" stays visible as an instrumentation
// gap instead of reading as a free request.
func (m *Meter) Add(u Usage) {
	if m == nil {
		return
	}
	// Recorded outside the lock: a metrics backend is not something to hold a
	// request scoped lock across, and this is the one place every model call converges.
	metrics.LLMCalls.Inc()
	if u.PromptTokens > 0 {
		metrics.LLMTokens.Add(float64(u.PromptTokens), "input")
	}
	if u.CompletionTokens > 0 {
		metrics.LLMTokens.Add(float64(u.CompletionTokens), "output")
	}
	m.mu.Lock()
	if u.PromptTokens > 0 {
		m.prompt += u.PromptTokens
	}
	if u.CompletionTokens > 0 {
		m.completion += u.CompletionTokens
	}
	m.calls++
	m.mu.Unlock()
}

// Snapshot returns the totals so far.
func (m *Meter) Snapshot() (prompt, completion, calls int) {
	if m == nil {
		return 0, 0, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.prompt, m.completion, m.calls
}

func (m *Meter) Total() int {
	p, c, _ := m.Snapshot()
	return p + c
}

type meterKey struct{}

// WithMeter binds a fresh meter to the context and returns it.
func WithMeter(ctx context.Context) (context.Context, *Meter) {
	m := &Meter{}
	return context.WithValue(ctx, meterKey{}, m), m
}

// MeterFrom returns the request's meter, or nil.
func MeterFrom(ctx context.Context) *Meter {
	m, _ := ctx.Value(meterKey{}).(*Meter)
	return m
}

// ParseArgs decodes a tool call's arguments. It returns nil for anything that is
// not a JSON object.
func ParseArgs(raw string) map[string]any {
	if raw == "" {
		return map[string]any{}
	}
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return nil
	}
	return m
}
