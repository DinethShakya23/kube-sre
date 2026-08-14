package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/config"
)

// Client talks the OpenAI chat completions protocol. Azure OpenAI uses the same
// body with a per deployment URL and an api-key header.
type Client struct {
	BaseURL     string
	APIKey      string
	Model       string
	Temperature float64
	// Azure switches to deployment URLs and the api-key header.
	Azure      bool
	APIVersion string
	HTTP       *http.Client
	// Retries is how many times a rate limit, a server error or a dropped
	// connection is retried. The default is 2.
	Retries int
	// Backoff is the wait before the first retry, doubled each time.
	Backoff time.Duration
	// StreamUsage asks the server to report usage on a streamed reply. Only the
	// endpoints known to accept it get it.
	StreamUsage bool
}

func newHTTP() *http.Client { return &http.Client{Timeout: 10 * time.Minute} }

// NewOpenAI builds a client for OpenAI or any compatible endpoint. baseURL empty
// means api.openai.com.
func NewOpenAI(apiKey, baseURL, model string, temperature float64) *Client {
	c := &Client{APIKey: apiKey, Model: model, Temperature: temperature, HTTP: newHTTP(), Retries: 2, Backoff: time.Second}
	if baseURL == "" {
		c.BaseURL = "https://api.openai.com/v1"
		c.StreamUsage = true
	} else {
		c.BaseURL = strings.TrimRight(baseURL, "/")
	}
	return c
}

// NewAzure builds a client for an Azure OpenAI deployment.
func NewAzure(endpoint, apiKey, apiVersion, deployment string, temperature float64) (*Client, error) {
	if endpoint == "" {
		return nil, errors.New("AZURE_OPENAI_ENDPOINT is not set. Run 'kube-sre init' or set it in ~/.kube-sre/.env")
	}
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "https://" + endpoint
	}
	return &Client{
		BaseURL: strings.TrimRight(endpoint, "/"), APIKey: apiKey, Model: deployment, Temperature: temperature,
		Azure: true, APIVersion: apiVersion, HTTP: newHTTP(), Retries: 2, Backoff: time.Second, StreamUsage: true,
	}, nil
}

// Tier picks a model for a job: the full capability model for the coordinator and
// synthesizer, or the faster and cheaper one for the parallel subagents.
type Tier int

const (
	Coordinator Tier = iota
	Subagent
)

// New builds the model for a tier from configuration.
//
// The anthropic provider has no backend in this build: like the V2 graph it
// stands in for, it sends the requests to the OpenAI compatible endpoint, and
// Config.LLMWarnings names the consequence at startup.
func New(cfg *config.Config, tier Tier) (*Client, error) {
	if cfg.LLMProvider == "azure" {
		dep := cfg.AzureCoord
		if tier == Subagent {
			dep = cfg.AzureSub
		}
		return NewAzure(cfg.AzureEnd, cfg.AzureKey, cfg.AzureVersion, dep, cfg.LLMTemperature)
	}
	model := cfg.CoordModel
	if tier == Subagent {
		model = cfg.SubModel
	}
	return NewOpenAI(cfg.OpenAIKey, cfg.OpenAIBase, model, cfg.LLMTemperature), nil
}

// MaxTokens is the completion cap for a tier: 4096 for the coordinator, 2048 for
// subagents, which only produce a short structured finding.
func (t Tier) MaxTokens() int {
	if t == Subagent {
		return 2048
	}
	return 4096
}

func (c *Client) endpoint() string {
	if c.Azure {
		return fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s",
			c.BaseURL, url.PathEscape(c.Model), url.QueryEscape(c.APIVersion))
	}
	return c.BaseURL + "/chat/completions"
}

type wireToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

func toWire(msgs []Message) []wireMessage {
	out := make([]wireMessage, 0, len(msgs))
	for _, m := range msgs {
		w := wireMessage{Role: string(m.Role), ToolCallID: m.ToolCallID}
		content := m.Content
		// An assistant turn that only calls tools carries null content.
		if !(m.Role == Assistant && len(m.ToolCalls) > 0 && content == "") {
			w.Content = &content
		}
		if m.Role == Tool {
			w.Name = m.Name
		}
		for _, tc := range m.ToolCalls {
			raw := tc.RawArgs
			if raw == "" {
				b, _ := json.Marshal(tc.Args)
				raw = string(b)
				if tc.Args == nil {
					raw = "{}"
				}
			}
			var wt wireToolCall
			wt.ID, wt.Type = tc.ID, "function"
			wt.Function.Name, wt.Function.Arguments = tc.Name, raw
			w.ToolCalls = append(w.ToolCalls, wt)
		}
		out = append(out, w)
	}
	return out
}

func (c *Client) body(msgs []Message, tools []ToolSpec, opts Options, stream bool) ([]byte, error) {
	body := map[string]any{"messages": toWire(msgs), "temperature": c.Temperature}
	if !c.Azure {
		body["model"] = c.Model
	}
	if opts.MaxTokens > 0 {
		body["max_completion_tokens"] = opts.MaxTokens
	}
	if len(tools) > 0 {
		var specs []map[string]any
		for _, t := range tools {
			params := t.Parameters
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			specs = append(specs, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": params,
			}})
		}
		body["tools"] = specs
	}
	if stream {
		body["stream"] = true
		if c.StreamUsage {
			body["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	return json.Marshal(body)
}

// Error is a failed call, kept structured so callers can word it for a person.
type Error struct {
	Status int
	Body   string
	Cause  error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return "LLM request failed: " + e.Cause.Error()
	}
	return fmt.Sprintf("LLM request failed: HTTP %d: %s", e.Status, e.Body)
}

func (e *Error) Unwrap() error { return e.Cause }

func retryable(err *Error) bool {
	if err.Cause != nil {
		return !errors.Is(err.Cause, context.Canceled) && !errors.Is(err.Cause, context.DeadlineExceeded)
	}
	return err.Status == 429 || err.Status >= 500
}

// Chat runs one completion. With opts.OnToken set the reply is streamed and the
// callback sees each piece of text as it arrives.
func (c *Client) Chat(ctx context.Context, msgs []Message, tools []ToolSpec, opts Options) (*Response, error) {
	stream := opts.OnToken != nil
	payload, err := c.body(msgs, tools, opts, stream)
	if err != nil {
		return nil, err
	}
	var resp *Response
	wait := c.Backoff
	for attempt := 0; ; attempt++ {
		resp, err = c.once(ctx, payload, opts, stream)
		if err == nil {
			MeterFrom(ctx).Add(resp.Usage)
			return resp, nil
		}
		var le *Error
		if !errors.As(err, &le) || !retryable(le) || attempt >= c.Retries {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
	}
}

func (c *Client) once(ctx context.Context, payload []byte, opts Options, stream bool) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Azure {
		req.Header.Set("api-key", c.APIKey)
	} else if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &Error{Cause: err}
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return nil, &Error{Status: res.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if stream {
		return readStream(res.Body, opts.OnToken)
	}
	return readOnce(res.Body)
}

type wireResponse struct {
	Choices []struct {
		Message struct {
			Role      string         `json:"role"`
			Content   any            `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// text flattens content, which a few providers send as a list of blocks.
func text(v any) string {
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, part := range c {
			if m, ok := part.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

func readOnce(r io.Reader) (*Response, error) {
	var w wireResponse
	if err := json.NewDecoder(r).Decode(&w); err != nil {
		return nil, &Error{Cause: fmt.Errorf("bad response: %w", err)}
	}
	if len(w.Choices) == 0 {
		return nil, &Error{Cause: errors.New("the model returned no choices")}
	}
	m := w.Choices[0].Message
	out := &Response{Message: Message{Role: Assistant, Content: text(m.Content)}}
	for _, tc := range m.ToolCalls {
		out.Message.ToolCalls = append(out.Message.ToolCalls, ToolCall{
			ID: tc.ID, Name: tc.Function.Name, RawArgs: tc.Function.Arguments, Args: ParseArgs(tc.Function.Arguments),
		})
	}
	if w.Usage != nil {
		out.Usage = Usage{w.Usage.PromptTokens, w.Usage.CompletionTokens}
	}
	return out, nil
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   any            `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func readStream(r io.Reader, onToken func(string)) (*Response, error) {
	var content strings.Builder
	calls := map[int]*ToolCall{}
	var order []int
	var usage Usage
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var ch streamChunk
		if json.Unmarshal([]byte(data), &ch) != nil {
			continue
		}
		if ch.Usage != nil {
			usage = Usage{ch.Usage.PromptTokens, ch.Usage.CompletionTokens}
		}
		if len(ch.Choices) == 0 {
			continue
		}
		d := ch.Choices[0].Delta
		if s := text(d.Content); s != "" {
			content.WriteString(s)
			onToken(s)
		}
		for _, tc := range d.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			cur, ok := calls[idx]
			if !ok {
				cur = &ToolCall{}
				calls[idx] = cur
				order = append(order, idx)
			}
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			if tc.Function.Name != "" {
				cur.Name = tc.Function.Name
			}
			cur.RawArgs += tc.Function.Arguments
		}
	}
	if err := sc.Err(); err != nil {
		return nil, &Error{Cause: err}
	}
	out := &Response{Message: Message{Role: Assistant, Content: content.String()}, Usage: usage}
	for _, idx := range order {
		tc := calls[idx]
		tc.Args = ParseArgs(tc.RawArgs)
		out.Message.ToolCalls = append(out.Message.ToolCalls, *tc)
	}
	return out, nil
}
