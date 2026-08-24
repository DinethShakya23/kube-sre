package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/agent"
	"github.com/DinethShakya23/kube-sre/internal/audit"
	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/events"
	"github.com/DinethShakya23/kube-sre/internal/helm"
	"github.com/DinethShakya23/kube-sre/internal/kube"
	"github.com/DinethShakya23/kube-sre/internal/llm"
	"github.com/DinethShakya23/kube-sre/internal/loki"
	"github.com/DinethShakya23/kube-sre/internal/nsguard"
	"github.com/DinethShakya23/kube-sre/internal/playbooks"
	"github.com/DinethShakya23/kube-sre/internal/prom"
	"github.com/DinethShakya23/kube-sre/internal/store/storetest"
)

type scripted struct {
	mu      sync.Mutex
	replies []llm.Message
}

func (s *scripted) Chat(ctx context.Context, msgs []llm.Message, _ []llm.ToolSpec, o llm.Options) (*llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := llm.Message{Content: "ok"}
	if len(s.replies) > 0 {
		m, s.replies = s.replies[0], s.replies[1:]
	}
	m.Role = llm.Assistant
	if o.OnToken != nil && m.Content != "" {
		o.OnToken(m.Content)
	}
	llm.MeterFrom(ctx).Add(llm.Usage{PromptTokens: 7, CompletionTokens: 3})
	return &llm.Response{Message: m}, nil
}

type memCP struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (c *memCP) Load(_ context.Context, s string) (*agent.State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.m[s]
	if !ok {
		return nil, nil
	}
	var st agent.State
	return &st, json.Unmarshal(b, &st)
}
func (c *memCP) Save(_ context.Context, s string, st *agent.State) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string][]byte{}
	}
	b, err := json.Marshal(st)
	c.m[s] = b
	return err
}

type rig struct {
	srv   *Server
	model *scripted
	ts    *httptest.Server
	bin   string
	log   string
}

const kubectlBody = `case "$1 $2" in
"get pods") printf 'NAMESPACE NAME READY STATUS\nshop web 1/1 Running\n';;
"get events") echo "No resources found";;
"get namespaces") echo "default kube-system shop monitoring";;
*) echo done;;
esac`

func newRig(t *testing.T, env map[string]string) *rig {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	bin := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"$*\" >> "+log+"\n"+kubectlBody+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := config.Load(func(k string) string { return env[k] })
	blocked := nsguard.Blocklist(cfg.BlockedNamespaces)
	tools := &agent.Toolset{
		Kubectl: kube.NewTool(kube.Config{Bin: bin, Timeout: 5 * time.Second, BlockedNamespaces: blocked, BlockedResources: cfg.BlockedResources}),
		Helm:    helm.New(blocked, cfg.BlockedResources), Prom: prom.New("", blocked), Loki: loki.New("", blocked),
	}
	em := events.NewEmitter(nil)
	model := &scripted{}
	ag := agent.New(agent.Deps{
		Cfg: cfg, Tools: tools, Coordinator: model, Subagent: model, Emitter: em, Checkpoints: &memCP{},
		Snapshot:  &agent.Snapshotter{Bin: bin, Timeout: 5 * time.Second, Blocked: blocked},
		Playbooks: playbooks.Load(),
	})
	s := NewServer(cfg, ag, em, "0.1.0")
	s.SetReady(true)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &rig{srv: s, model: model, ts: ts, bin: bin, log: log}
}

func (r *rig) do(t *testing.T, method, path, body string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, r.ts.URL+path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func bearer(k string) map[string]string { return map[string]string{"Authorization": "Bearer " + k} }

// frames reads the data lines of an SSE body.
func frames(body string) []map[string]any {
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			out = append(out, map[string]any{"__done__": true})
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(data), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func content(fs []map[string]any) string {
	var b strings.Builder
	for _, f := range fs {
		if ch, ok := f["choices"].([]any); ok && len(ch) > 0 {
			if d, ok := ch[0].(map[string]any)["delta"].(map[string]any); ok {
				if c, ok := d["content"].(string); ok {
					b.WriteString(c)
				}
			}
		}
	}
	return b.String()
}

func chatBody(msg string) string {
	b, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": msg}}})
	return string(b)
}

func TestOpenAccessIsAdminAndRequireAuthRefuses(t *testing.T) {
	r := newRig(t, nil)
	if _, b := r.do(t, "GET", "/v1/auth/whoami", "", nil); !strings.Contains(b, `"role":"admin"`) {
		t.Errorf("%s", b)
	}
	r = newRig(t, map[string]string{"REQUIRE_AUTH": "true"})
	resp, b := r.do(t, "GET", "/v1/auth/whoami", "", nil)
	if resp.StatusCode != 401 || !strings.Contains(b, "REQUIRE_AUTH") {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
}

func TestKeysMapToRoles(t *testing.T) {
	r := newRig(t, map[string]string{
		"KUBESRE_SUPERADMIN_KEYS": "sk", "KUBESRE_ADMIN_KEYS": "ak", "KUBESRE_OPERATOR_KEYS": "ok", "KUBESRE_READONLY_KEYS": "rk",
	})
	for key, role := range map[string]string{"sk": "superadmin", "ak": "admin", "ok": "operator", "rk": "readonly"} {
		if _, b := r.do(t, "GET", "/v1/auth/whoami", "", bearer(key)); !strings.Contains(b, `"role":"`+role+`"`) {
			t.Errorf("%s: %s", key, b)
		}
	}
	for _, h := range []map[string]string{nil, {"Authorization": "Basic x"}, bearer("wrong"), bearer(""), bearer("sk-extra"), bearer("s")} {
		if resp, _ := r.do(t, "GET", "/v1/auth/whoami", "", h); resp.StatusCode != 401 {
			t.Errorf("%v should be refused", h)
		}
	}
	// every authenticated route inherits the gate, and the probes stay public
	for _, p := range []string{"/v1/namespaces", "/v1/events/replay/x"} {
		if resp, _ := r.do(t, "GET", p, "", nil); resp.StatusCode != 401 {
			t.Errorf("%s answered without a key: %d", p, resp.StatusCode)
		}
	}
	if resp, _ := r.do(t, "POST", "/v1/chat/completions", chatBody("hi"), nil); resp.StatusCode != 401 {
		t.Error("chat needs a key")
	}
	for _, p := range []string{"/healthz", "/readyz", "/v1/healthz", "/v1/readyz", "/metrics"} {
		if resp, _ := r.do(t, "GET", p, "", nil); resp.StatusCode != 200 {
			t.Errorf("%s must be public: %d", p, resp.StatusCode)
		}
	}
}

func TestHMACDemoKeys(t *testing.T) {
	now := time.Now()
	key, exp, err := MintDemoKey("secret", "a@b.co", time.Hour, now)
	if err != nil || !strings.HasPrefix(key, "ks-ro-") || exp != now.Add(time.Hour).Unix() {
		t.Fatalf("%v %v", key, err)
	}
	if !verifyDemoKey("secret", key, now) {
		t.Error("a fresh key verifies")
	}
	if verifyDemoKey("secret", key, now.Add(2*time.Hour)) {
		t.Error("an expired key is refused")
	}
	if verifyDemoKey("other", key, now) || verifyDemoKey("", key, now) {
		t.Error("the wrong secret")
	}
	i := strings.LastIndex(key, ".")
	if verifyDemoKey("secret", key[:i+1]+strings.Repeat("0", 32), now) || verifyDemoKey("secret", "ks-ro-nodot", now) || verifyDemoKey("secret", "other-"+key, now) {
		t.Error("tampered and malformed keys")
	}
	if _, _, err := MintDemoKey("", "a@b.co", time.Hour, now); err == nil {
		t.Error("no secret")
	}

	r := newRig(t, map[string]string{"AUTH_BACKEND": "hmac", "DEMO_KEY_HMAC_SECRET": "secret", "KUBESRE_ADMIN_KEYS": "ak"})
	if _, b := r.do(t, "GET", "/v1/auth/whoami", "", bearer(key)); !strings.Contains(b, `"role":"readonly"`) {
		t.Errorf("a demo key is readonly: %s", b)
	}
	r = newRig(t, map[string]string{"AUTH_BACKEND": "static", "DEMO_KEY_HMAC_SECRET": "secret", "KUBESRE_ADMIN_KEYS": "ak"})
	if resp, _ := r.do(t, "GET", "/v1/auth/whoami", "", bearer(key)); resp.StatusCode != 401 {
		t.Error("static backend does not honour demo keys")
	}
}

func TestMintEndpoint(t *testing.T) {
	r := newRig(t, map[string]string{"KUBESRE_ADMIN_KEYS": "ak", "KUBESRE_READONLY_KEYS": "rk", "DEMO_KEY_HMAC_SECRET": "secret", "DEMO_KEY_MAX_TTL_HOURS": "48"})
	post := func(key, body string) (*http.Response, string) {
		return r.do(t, "POST", "/v1/auth/demo-keys", body, bearer(key))
	}
	if resp, _ := post("rk", `{"email":"a@b.co"}`); resp.StatusCode != 403 {
		t.Error("readonly cannot mint")
	}
	resp, b := post("ak", `{"email":"a@b.co","ttl_hours":2}`)
	var out map[string]any
	json.Unmarshal([]byte(b), &out)
	if resp.StatusCode != 200 || out["expires_in_seconds"] != float64(7200) || !strings.HasPrefix(out["api_key"].(string), "ks-ro-") || out["email"] != "a@b.co" {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
	if resp, _ := post("ak", `{"email":"nope"}`); resp.StatusCode != 422 {
		t.Error("invalid email")
	}
	if resp, _ := post("ak", `{"email":"a@b.co","ttl_hours":0}`); resp.StatusCode != 422 {
		t.Error("ttl below 1")
	}
	if resp, b := post("ak", `{"email":"a@b.co","ttl_hours":49}`); resp.StatusCode != 400 || !strings.Contains(b, "exceeds max (48)") {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
	r2 := newRig(t, map[string]string{"KUBESRE_ADMIN_KEYS": "ak"})
	if resp, _ := r2.do(t, "POST", "/v1/auth/demo-keys", `{"email":"a@b.co"}`, bearer("ak")); resp.StatusCode != 503 {
		t.Error("no secret configured")
	}
}

func TestRateLimiter(t *testing.T) {
	cfg := config.Load(func(k string) string {
		return map[string]string{"RATE_LIMIT_BURST": "3", "RATE_LIMIT_PER_MIN": "60", "RATE_LIMIT_MAX_TRACKED": "2"}[k]
	})
	l := NewLimiter(cfg)
	now := time.Now()
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("a", now); !ok {
			t.Fatalf("burst %d", i)
		}
	}
	ok, retry := l.Allow("a", now)
	if ok || retry < time.Second {
		t.Errorf("over the burst: %v %v", ok, retry)
	}
	if ok, _ := l.Allow("a", now.Add(1100*time.Millisecond)); !ok {
		t.Error("a token refills each second at 60 per minute")
	}
	if ok, _ := l.Allow("b", now); !ok {
		t.Error("callers have separate buckets")
	}
	l.Allow("c", now) // evicts the least recently seen
	if len(l.idx) != 2 || l.idx["a"] != nil && l.idx["b"] != nil && l.idx["c"] != nil {
		t.Errorf("the table is bounded: %d", len(l.idx))
	}
}

func TestRateLimitMiddlewareAndKeys(t *testing.T) {
	r := newRig(t, map[string]string{"RATE_LIMIT_BURST": "2", "RATE_LIMIT_PER_MIN": "1", "KUBESRE_ADMIN_KEYS": "ak,bk"})
	get := func(key string) *http.Response {
		resp, _ := r.do(t, "GET", "/v1/auth/whoami", "", bearer(key))
		return resp
	}
	get("ak")
	get("ak")
	resp := get("ak")
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" || resp.Header.Get("X-RateLimit-Limit") != "1" ||
		resp.Header.Get("X-RateLimit-Remaining") != "0" {
		t.Errorf("%d %v", resp.StatusCode, resp.Header)
	}
	if get("bk").StatusCode != 200 {
		t.Error("another key has its own bucket")
	}
	for i := 0; i < 5; i++ {
		for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
			if resp, _ := r.do(t, "GET", p, "", nil); resp.StatusCode != 200 {
				t.Fatalf("%s was throttled: probes must never be", p)
			}
		}
	}
	// a 429 still carries the CORS header, so a browser can see the status
	req, _ := http.NewRequest("GET", r.ts.URL+"/v1/auth/whoami", nil)
	req.Header.Set("Authorization", "Bearer ak")
	req.Header.Set("Origin", "http://localhost:3080")
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != 429 || res.Header.Get("Access-Control-Allow-Origin") != "http://localhost:3080" {
		t.Errorf("%d %v", res.StatusCode, res.Header)
	}
	if k := r.srv.Limiter.CallerKey(httptest.NewRequest("GET", "/", nil)); !strings.HasPrefix(k, "ip:") {
		t.Errorf("%s", k)
	}
	rq := httptest.NewRequest("GET", "/", nil)
	rq.Header.Set("Authorization", "Bearer super-secret-key")
	if k := r.srv.Limiter.CallerKey(rq); !strings.HasPrefix(k, "k:") || strings.Contains(k, "super-secret") {
		t.Errorf("the raw key must never be stored: %s", k)
	}
}

func TestForwardedForOnlyReadFromTheRight(t *testing.T) {
	mk := func(hops string) *Limiter {
		return NewLimiter(config.Load(func(k string) string { return map[string]string{"RATE_LIMIT_TRUSTED_PROXY_HOPS": hops}[k] }))
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.9:5555"
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 1.2.3.4, 10.1.1.1")
	if got := mk("0").clientAddress(req); got != "10.0.0.9" {
		t.Errorf("ignored by default: %s", got)
	}
	if got := mk("1").clientAddress(req); got != "10.1.1.1" {
		t.Errorf("%s", got)
	}
	if got := mk("2").clientAddress(req); got != "1.2.3.4" {
		t.Errorf("%s", got)
	}
	if got := mk("5").clientAddress(req); got != "10.0.0.9" {
		t.Errorf("a header shorter than the declared chain did not come from it: %s", got)
	}
}

func TestCORS(t *testing.T) {
	r := newRig(t, map[string]string{"ALLOWED_ORIGINS": "http://a.example, http://b.example"})
	req, _ := http.NewRequest("OPTIONS", r.ts.URL+"/v1/chat/completions", nil)
	req.Header.Set("Origin", "http://b.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != 200 || res.Header.Get("Access-Control-Allow-Origin") != "http://b.example" ||
		res.Header.Get("Access-Control-Allow-Credentials") != "true" || res.Header.Get("Access-Control-Allow-Headers") != "authorization,content-type" {
		t.Errorf("preflight: %d %v", res.StatusCode, res.Header)
	}
	req.Header.Set("Origin", "http://evil.example")
	if res, _ = http.DefaultClient.Do(req); res.StatusCode != 400 || res.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("an unlisted origin: %d", res.StatusCode)
	}
	resp, _ := r.do(t, "GET", "/healthz", "", map[string]string{"Origin": "http://a.example"})
	if resp.Header.Get("Access-Control-Allow-Origin") != "http://a.example" {
		t.Errorf("%v", resp.Header)
	}
	resp, _ = r.do(t, "GET", "/healthz", "", map[string]string{"Origin": "http://evil.example"})
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("no header for an origin the operator did not list")
	}
}

func TestRequestIDIsEchoedAndGenerated(t *testing.T) {
	r := newRig(t, nil)
	resp, _ := r.do(t, "GET", "/healthz", "", map[string]string{"X-Request-ID": "abc-123"})
	if resp.Header.Get("X-Request-ID") != "abc-123" {
		t.Error("echoed")
	}
	resp, _ = r.do(t, "GET", "/healthz", "", nil)
	if id := resp.Header.Get("X-Request-ID"); len(id) != 36 {
		t.Errorf("generated: %q", id)
	}
}

func TestChatStreamShape(t *testing.T) {
	r := newRig(t, nil)
	r.model.replies = []llm.Message{{Content: "All pods are Running."}}
	resp, body := r.do(t, "POST", "/v1/chat/completions", chatBody("how is the cluster?"), map[string]string{"X-Session-ID": "sess-1"})
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	fs := frames(body)
	if fs[0]["object"] != "stream.start" || fs[0]["protocol_version"] != "1.0" || fs[0]["session_id"] != "sess-1" {
		t.Errorf("handshake first: %v", fs[0])
	}
	if !fs[len(fs)-1]["__done__"].(bool) {
		t.Error("ends with [DONE]")
	}
	if content(fs) != "All pods are Running." {
		t.Errorf("%q", content(fs))
	}
	var phases []string
	var usage, finish map[string]any
	for _, f := range fs {
		if ev, ok := f["ki_event"].(map[string]any); ok {
			switch ev["type"] {
			case "status":
				phases = append(phases, ev["phase"].(string))
			case "usage":
				usage = ev
			}
		}
		if ch, ok := f["choices"].([]any); ok && len(ch) > 0 {
			if fr, ok := ch[0].(map[string]any)["finish_reason"].(string); ok {
				finish = f
				_ = fr
			}
		}
	}
	if strings.Join(phases, ",") != "loading,snapshot,analyzing" {
		t.Errorf("%v", phases)
	}
	if usage == nil || usage["prompt_tokens"] != float64(7) || usage["completion_tokens"] != float64(3) || usage["llm_calls"] != float64(1) || usage["total_tokens"] != float64(10) {
		t.Errorf("usage before the terminator: %v", usage)
	}
	if finish == nil {
		t.Error("a stop chunk before [DONE]")
	}
	for i, f := range fs {
		if _, ok := f["ki_event"]; ok {
			if ch := f["choices"].([]any); len(ch) != 0 {
				t.Errorf("frame %d: ki_event frames carry empty choices", i)
			}
		}
	}
}

func TestChatValidation(t *testing.T) {
	r := newRig(t, nil)
	resp, _ := r.do(t, "POST", "/v1/chat/completions", `{"messages":[{"role":"system","content":"x"}]}`, nil)
	if resp.StatusCode != 422 {
		t.Error("no user message")
	}
	resp, b := r.do(t, "POST", "/v1/chat/completions", `{"stream":false,"messages":[{"role":"user","content":"x"}]}`, nil)
	if resp.StatusCode != 422 || !strings.Contains(b, "Only stream=true") {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
	if resp, _ = r.do(t, "POST", "/v1/chat/completions", `not json`, nil); resp.StatusCode != 422 {
		t.Error("bad json")
	}
	r.model.replies = []llm.Message{{Content: "a"}}
	resp, b = r.do(t, "POST", "/v1/chat/completions", `{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"x"},{"role":"user","content":"last"}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatal(b)
	}
	st, _ := r.srv.Agent.Checkpoints.Load(context.Background(), frames(b)[0]["session_id"].(string))
	if st.Messages[0].Content != "last" {
		t.Errorf("the last user message is the question: %+v", st.Messages[0])
	}
}

func TestApprovalOverTwoRequests(t *testing.T) {
	r := newRig(t, nil)
	r.model.replies = []llm.Message{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "run_kubectl", Args: map[string]any{"command": "delete pod web -n shop"}}}},
		{Content: "deleted"},
	}
	h := map[string]string{"X-Session-ID": "s-apr"}
	_, body := r.do(t, "POST", "/v1/chat/completions", chatBody("delete web"), h)
	fs := frames(body)
	var hitl map[string]any
	for _, f := range fs {
		if ch, ok := f["choices"].([]any); ok && len(ch) > 0 {
			if c := ch[0].(map[string]any); c["hitl_required"] == true {
				hitl = c
			}
		}
	}
	if hitl == nil || hitl["risk_level"] != "high" || hitl["human_summary"] != "kubectl delete pod web -n shop" || len(hitl["action_id"].(string)) != 36 {
		t.Fatalf("hitl fields: %v", hitl)
	}
	msg := content(fs)
	if !strings.Contains(msg, "Approval Required") || !strings.Contains(msg, "risk level: `HIGH`") || !strings.Contains(msg, "kubectl delete pod web -n shop") ||
		!strings.Contains(msg, "`yes` or `/approve`") {
		t.Errorf("%q", msg)
	}
	if b, _ := os.ReadFile(r.log); strings.Contains(string(b), "delete pod") {
		t.Fatal("must not run before approval")
	}
	_, body = r.do(t, "POST", "/v1/chat/completions", chatBody("yes"), h)
	if content(frames(body)) != "deleted" {
		t.Errorf("%q", content(frames(body)))
	}
	if b, _ := os.ReadFile(r.log); !strings.Contains(string(b), "delete pod web") {
		t.Error("runs once approved")
	}
}

func TestErrorFrame(t *testing.T) {
	p := frame("id", map[string]any{"type": "error", "error": "boom"})
	if !strings.Contains(content([]map[string]any{p}), "**Error:** boom") {
		t.Errorf("%v", p)
	}
	if fr, _ := p["choices"].([]any)[0].(map[string]any)["finish_reason"].(*string); fr == nil || *fr != "stop" {
		t.Error("an error frame stops the stream")
	}
	if frame("id", map[string]any{"type": "final"}) != nil {
		t.Error("final is the loop ending, not a frame")
	}
	tool := frame("id", map[string]any{"type": "tool_call", "tool": "query_loki"})
	if tool["ki_event"].(map[string]any)["message"] != "Calling query_loki" {
		t.Errorf("%v", tool)
	}
	tool = frame("id", map[string]any{"type": "tool_call", "tool": "run_kubectl", "command": "get pods"})
	if tool["ki_event"].(map[string]any)["message"] != "Running: get pods" {
		t.Errorf("%v", tool)
	}
}

func TestReplay(t *testing.T) {
	r := newRig(t, nil)
	if resp, b := r.do(t, "GET", "/v1/events/replay/none", "", nil); resp.StatusCode != 404 || !strings.Contains(b, "NOT evidence") || !strings.Contains(b, "/v1/episodes/none/replay") {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
	r.srv.Emitter.Prepare("s9")
	r.srv.Emitter.Emit("s9", events.NewStatus("s9", "loading", "x"))
	r.srv.Emitter.Close("s9")
	resp, b := r.do(t, "GET", "/v1/events/replay/s9", "", nil)
	fs := frames(b)
	if resp.StatusCode != 200 || fs[0]["type"] != "replay_meta" || fs[0]["records"] != float64(2) || fs[0]["durable"] != false ||
		fs[1]["type"] != "status" || fs[2]["type"] != "final" || !fs[3]["__done__"].(bool) {
		t.Errorf("%d %v", resp.StatusCode, fs)
	}
	r.srv.Emitter.Prepare("empty")
	_, b = r.do(t, "GET", "/v1/events/replay/empty", "", nil)
	if fs := frames(b); fs[0]["records"] != float64(0) {
		t.Errorf("an empty session is present and says so: %v", fs)
	}
}

func TestNamespacesFiltersAndSaysSo(t *testing.T) {
	r := newRig(t, nil)
	resp, b := r.do(t, "GET", "/v1/namespaces", "", nil)
	var out struct {
		Namespaces []string `json:"namespaces"`
		Withheld   string   `json:"withheldByPolicy"`
	}
	json.Unmarshal([]byte(b), &out)
	if resp.StatusCode != 200 || strings.Join(out.Namespaces, ",") != "default,shop" || !strings.Contains(out.Withheld, "2 namespace(s) withheld") ||
		!strings.Contains(out.Withheld, "NOT the complete set") {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
}

func TestNamespacesFailureIsNotAnEmptyList(t *testing.T) {
	r := newRig(t, nil)
	os.WriteFile(r.bin, []byte("#!/bin/sh\necho 'Unable to connect to the server: dial tcp: connection refused' >&2\necho second >&2\nexit 1\n"), 0o755)
	resp, b := r.do(t, "GET", "/v1/namespaces", "", nil)
	if resp.StatusCode != 503 || !strings.Contains(b, "Cannot list namespaces: Unable to connect") || strings.Contains(b, "second") {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
	os.WriteFile(r.bin, []byte("#!/bin/sh\necho\n"), 0o755)
	_, b = r.do(t, "GET", "/v1/namespaces", "", nil)
	if !strings.Contains(b, `"namespaces":[]`) || !strings.Contains(b, `"withheldByPolicy":""`) {
		t.Errorf("a cluster with none is an empty list: %s", b)
	}
	t.Setenv("PATH", "/nonexistent")
	resp, b = r.do(t, "GET", "/v1/namespaces", "", nil)
	if resp.StatusCode != 503 || !strings.Contains(b, "kubectl is not installed") {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
}

func TestHealthAndReadiness(t *testing.T) {
	r := newRig(t, nil)
	r.srv.Health["audit"] = func() map[string]any { return map[string]any{"enabled": true} }
	_, b := r.do(t, "GET", "/healthz", "", nil)
	var h map[string]any
	json.Unmarshal([]byte(b), &h)
	if h["status"] != "ok" || h["version"] != "0.1.0" || h["arm"] == "" || h["audit"].(map[string]any)["enabled"] != true ||
		h["leader"].(map[string]any)["is_leader"] != true {
		t.Errorf("%v", h)
	}
	if resp, _ := r.do(t, "GET", "/readyz", "", nil); resp.StatusCode != 200 {
		t.Error("ready")
	}
	r.srv.SetReady(false)
	resp, b := r.do(t, "GET", "/readyz", "", nil)
	if resp.StatusCode != 503 || !strings.Contains(b, "draining") {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
	if resp, _ := r.do(t, "GET", "/healthz", "", nil); resp.StatusCode != 200 {
		t.Error("liveness stays up while draining")
	}
}

func TestExtraRoutesInheritAuth(t *testing.T) {
	r := newRig(t, map[string]string{"KUBESRE_ADMIN_KEYS": "ak"})
	r.srv.Handle("GET /v1/thing", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]any{"ok": true}) })
	ts := httptest.NewServer(r.srv.Handler())
	defer ts.Close()
	res, _ := http.Get(ts.URL + "/v1/thing")
	if res.StatusCode != 401 {
		t.Error("a route added later inherits the gate")
	}
	req, _ := http.NewRequest("GET", ts.URL+"/v1/thing", nil)
	req.Header.Set("Authorization", "Bearer ak")
	if res, _ = http.DefaultClient.Do(req); res.StatusCode != 200 {
		t.Errorf("%d", res.StatusCode)
	}
}

func TestAuditRowIsWrittenAfterAChat(t *testing.T) {
	db := storetest.New(t, audit.Migrations)
	r := newRig(t, nil)
	l := audit.New(db)
	l.Start(context.Background())
	r.srv.Audit = l
	r.do(t, "POST", "/v1/chat/completions", chatBody("hi"), map[string]string{"X-Session-ID": "aud-1"})
	var role, path string
	if err := db.QueryRow(db.Q(`SELECT user_role, path FROM request_log WHERE session_id = ?`), "aud-1").Scan(&role, &path); err != nil {
		t.Fatal(err)
	}
	if role != "admin" || path != "/v1/chat/completions" {
		t.Errorf("%s %s", role, path)
	}
}

func TestMetricsEndpointCountsRequests(t *testing.T) {
	r := newRig(t, nil)
	r.do(t, "GET", "/v1/auth/whoami", "", nil)
	_, b := r.do(t, "GET", "/metrics", "", nil)
	if !strings.Contains(b, `http_requests_total{method="GET",handler="/v1/auth/whoami",status="200"}`) {
		t.Errorf("%s", b)
	}
	if strings.Contains(b, `handler="/metrics"`) || strings.Contains(b, `handler="/healthz"`) {
		t.Error("probes and the scrape itself are not counted")
	}
}
