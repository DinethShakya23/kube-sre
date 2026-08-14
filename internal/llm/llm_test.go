package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/config"
)

func serve(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewOpenAI("sk-test", srv.URL+"/v1", "gpt-4o", 0)
	c.Backoff = time.Millisecond
	return c, srv
}

func reqBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestChatSendsTheOpenAIShapeAndParsesToolCalls(t *testing.T) {
	var got map[string]any
	var path, auth string
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		got = reqBody(t, r)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[
		  {"id":"call_1","type":"function","function":{"name":"run_kubectl","arguments":"{\"command\":\"get pods\"}"}},
		  {"id":"call_2","type":"function","function":{"name":"run_helm","arguments":"not json"}}]}}],
		  "usage":{"prompt_tokens":11,"completion_tokens":4}}`)
	})
	tools := []ToolSpec{{Name: "run_kubectl", Description: "run kubectl", Parameters: map[string]any{
		"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}}}}
	ctx, meter := WithMeter(context.Background())
	resp, err := c.Chat(ctx, []Message{
		{Role: System, Content: "be brief"},
		{Role: User, Content: "hi"},
		{Role: Assistant, ToolCalls: []ToolCall{{ID: "c0", Name: "run_kubectl", Args: map[string]any{"command": "get ns"}}}},
		{Role: Tool, ToolCallID: "c0", Name: "run_kubectl", Content: "default"},
	}, tools, Options{MaxTokens: 512})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/chat/completions" || auth != "Bearer sk-test" {
		t.Errorf("path=%s auth=%s", path, auth)
	}
	if got["model"] != "gpt-4o" || got["temperature"] != float64(0) || got["max_completion_tokens"] != float64(512) || got["stream"] != nil {
		t.Errorf("%v", got)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("%v", msgs)
	}
	asst := msgs[2].(map[string]any)
	if asst["content"] != nil {
		t.Errorf("an assistant turn that only calls tools carries null content: %v", asst)
	}
	fn := asst["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "run_kubectl" || fn["arguments"] != `{"command":"get ns"}` {
		t.Errorf("%v", fn)
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "c0" || tool["name"] != "run_kubectl" {
		t.Errorf("%v", tool)
	}
	spec := got["tools"].([]any)[0].(map[string]any)
	if spec["type"] != "function" || spec["function"].(map[string]any)["name"] != "run_kubectl" {
		t.Errorf("%v", spec)
	}

	if len(resp.Message.ToolCalls) != 2 || resp.Message.ToolCalls[0].Args["command"] != "get pods" {
		t.Fatalf("%+v", resp.Message)
	}
	if bad := resp.Message.ToolCalls[1]; bad.Args != nil || bad.RawArgs != "not json" {
		t.Errorf("arguments that are not an object must be reported, not guessed: %+v", bad)
	}
	if p, comp, calls := meter.Snapshot(); p != 11 || comp != 4 || calls != 1 || meter.Total() != 15 {
		t.Errorf("meter %d %d %d", p, comp, calls)
	}
}

func TestStreamingCollectsTextToolCallsAndUsage(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		b := reqBody(t, r)
		if b["stream"] != true {
			t.Errorf("stream flag: %v", b)
		}
		if so, _ := b["stream_options"].(map[string]any); so["include_usage"] != true {
			t.Errorf("usage is asked for where it is accepted: %v", b["stream_options"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []string{
			`{"choices":[{"delta":{"role":"assistant","content":"Hel"}}]}`,
			`{"choices":[{"delta":{"content":"lo"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"run_kubectl","arguments":"{\"comm"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"get pods\"}"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"run_helm","arguments":"{}"}}]}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":6}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", ev)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	c.StreamUsage = true
	var pieces []string
	ctx, meter := WithMeter(context.Background())
	resp, err := c.Chat(ctx, []Message{{Role: User, Content: "x"}}, nil, Options{OnToken: func(s string) { pieces = append(pieces, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pieces, "|") != "Hel|lo" || resp.Message.Content != "Hello" {
		t.Errorf("%v %q", pieces, resp.Message.Content)
	}
	if len(resp.Message.ToolCalls) != 2 || resp.Message.ToolCalls[0].Args["command"] != "get pods" || resp.Message.ToolCalls[1].Name != "run_helm" {
		t.Errorf("%+v", resp.Message.ToolCalls)
	}
	if p, comp, _ := meter.Snapshot(); p != 20 || comp != 6 {
		t.Errorf("usage from the last chunk: %d %d", p, comp)
	}
}

func TestCompatibleEndpointsDoNotGetStreamOptions(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := reqBody(t, r)["stream_options"]; ok {
			t.Error("stream_options is sent only where it is known to be accepted")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	if _, err := c.Chat(context.Background(), nil, nil, Options{OnToken: func(string) {}}); err != nil {
		t.Fatal(err)
	}
}

func TestRetriesRateLimitsAndServerErrorsThenGivesUp(t *testing.T) {
	var calls int32
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			http.Error(w, "slow down", 429)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	resp, err := c.Chat(context.Background(), nil, nil, Options{})
	if err != nil || resp.Message.Content != "ok" || calls != 3 {
		t.Fatalf("resp=%v err=%v calls=%d", resp, err, calls)
	}

	calls = 0
	c, _ = serve(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "boom", 503)
	})
	_, err = c.Chat(context.Background(), nil, nil, Options{})
	var le *Error
	if err == nil || !asErr(err, &le) || le.Status != 503 || calls != 3 {
		t.Errorf("two retries then the error: %v calls=%d", err, calls)
	}

	calls = 0
	c, _ = serve(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, `{"error":{"message":"bad key"}}`, 401)
	})
	_, err = c.Chat(context.Background(), nil, nil, Options{})
	if err == nil || calls != 1 || !strings.Contains(err.Error(), "bad key") {
		t.Errorf("an auth failure is not retried: %v calls=%d", err, calls)
	}
}

func asErr(err error, target **Error) bool {
	for e := err; e != nil; {
		if le, ok := e.(*Error); ok {
			*target = le
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

func TestContextCancelStopsRetrying(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 500) })
	c.Backoff = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Chat(ctx, nil, nil, Options{}); err == nil || time.Since(start) > 2*time.Second {
		t.Errorf("err=%v after %v", err, time.Since(start))
	}
}

func TestAzureUsesDeploymentUrlAndApiKeyHeader(t *testing.T) {
	var path, query, key, auth string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, query, key, auth = r.URL.Path, r.URL.RawQuery, r.Header.Get("api-key"), r.Header.Get("Authorization")
		body = reqBody(t, r)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()
	c, err := NewAzure(srv.URL+"/", "azkey", "2024-10-01-preview", "gpt-4o-prod", 0.2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(context.Background(), []Message{{Role: User, Content: "x"}}, nil, Options{}); err != nil {
		t.Fatal(err)
	}
	if path != "/openai/deployments/gpt-4o-prod/chat/completions" || query != "api-version=2024-10-01-preview" {
		t.Errorf("%s ?%s", path, query)
	}
	if key != "azkey" || auth != "" {
		t.Errorf("api-key header, no bearer: %q %q", key, auth)
	}
	if _, ok := body["model"]; ok || body["temperature"] != 0.2 {
		t.Errorf("the deployment names the model on Azure: %v", body)
	}
	if _, err := NewAzure("", "k", "v", "d", 0); err == nil {
		t.Error("an Azure client with no endpoint must say so")
	}
	if az, _ := NewAzure("myres.openai.azure.com", "k", "v", "d", 0); !strings.HasPrefix(az.BaseURL, "https://") {
		t.Errorf("a missing protocol is added: %s", az.BaseURL)
	}
}

func TestNewPicksTheProviderAndTier(t *testing.T) {
	env := func(m map[string]string) *config.Config { return config.Load(func(k string) string { return m[k] }) }
	c, err := New(env(map[string]string{"OPENAI_API_KEY": "k"}), Coordinator)
	if err != nil || c.Model != "gpt-4o" || c.Azure || c.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("%+v %v", c, err)
	}
	c, _ = New(env(map[string]string{"OPENAI_API_KEY": "k", "OPENAI_BASE_URL": "http://llm.local/v1/"}), Subagent)
	if c.Model != "gpt-4o-mini" || c.BaseURL != "http://llm.local/v1" || c.StreamUsage {
		t.Errorf("%+v", c)
	}
	c, err = New(env(map[string]string{"LLM_PROVIDER": "azure", "AZURE_OPENAI_ENDPOINT": "https://x", "AZURE_OPENAI_API_KEY": "k"}), Subagent)
	if err != nil || !c.Azure || c.Model != "gpt-4o-mini" {
		t.Errorf("%+v %v", c, err)
	}
	if _, err := New(env(map[string]string{"LLM_PROVIDER": "azure"}), Coordinator); err == nil {
		t.Error("azure without an endpoint")
	}
	if Coordinator.MaxTokens() != 4096 || Subagent.MaxTokens() != 2048 {
		t.Error("completion caps")
	}
}

func TestContentBlocksAreFlattened(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":[{"type":"text","text":"Hel"},{"type":"text","text":"lo"}]}}]}`)
	})
	resp, err := c.Chat(context.Background(), nil, nil, Options{})
	if err != nil || resp.Message.Content != "Hello" {
		t.Errorf("%v %v", resp, err)
	}
}

func TestMeterIsSharedAcrossGoroutinesAndNilSafe(t *testing.T) {
	ctx, m := WithMeter(context.Background())
	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func() {
			MeterFrom(ctx).Add(Usage{1, 2})
			done <- struct{}{}
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}
	if p, c, n := m.Snapshot(); p != 10 || c != 20 || n != 10 {
		t.Errorf("%d %d %d", p, c, n)
	}
	m.Add(Usage{-5, 0})
	if p, _, n := m.Snapshot(); p != 10 || n != 11 {
		t.Error("a negative count is ignored but the call still counts")
	}
	var nilMeter *Meter
	nilMeter.Add(Usage{1, 1})
	if p, c, n := nilMeter.Snapshot(); p+c+n != 0 || MeterFrom(context.Background()) != nil {
		t.Error("no meter is a zero meter")
	}
}

func TestParseArgs(t *testing.T) {
	if m := ParseArgs(`{"a":1}`); m["a"] != float64(1) {
		t.Error(m)
	}
	if ParseArgs(`[1]`) != nil || ParseArgs(`nope`) != nil {
		t.Error("only objects")
	}
	if m := ParseArgs(""); m == nil || len(m) != 0 {
		t.Error("a call with no arguments has an empty object")
	}
}
