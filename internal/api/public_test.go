package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/llm"
)

func publicEnv(extra map[string]string) map[string]string {
	env := map[string]string{"KUBESRE_ADMIN_KEYS": "ak", "KUBESRE_READONLY_KEYS": "rk", "PUBLIC_DEMO": "true", "RATE_LIMIT_ENABLED": "false"}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func decode(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return m
}

func TestPublicCallerReadsButCannotManage(t *testing.T) {
	r := newRig(t, publicEnv(nil))
	_, body := r.do(t, "GET", "/v1/auth/whoami", "", nil)
	if m := decode(t, body); m["role"] != "public" || m["session"] != "none" {
		t.Fatalf("whoami: %s", body)
	}
	if resp, _ := r.do(t, "GET", "/v1/findings", "", nil); resp.StatusCode != 200 {
		t.Fatalf("findings: %d", resp.StatusCode)
	}
	for _, c := range [][2]string{{"GET", "/v1/preferences"}, {"PUT", "/v1/preferences"}, {"GET", "/v1/detectors"},
		{"POST", "/v1/detectors"}, {"GET", "/v1/v5/status"}, {"POST", "/v1/auth/demo-keys"}, {"DELETE", "/v1/preferences/x"}} {
		resp, body := r.do(t, c[0], c[1], "{}", nil)
		if resp.StatusCode != 403 || !strings.Contains(body, "Not available in the public demo.") {
			t.Fatalf("%s %s: %d %s", c[0], c[1], resp.StatusCode, body)
		}
	}
	// A key is never quietly downgraded: a bad one is a 401, a good one keeps its role.
	if resp, _ := r.do(t, "GET", "/v1/auth/whoami", "", bearer("nope")); resp.StatusCode != 401 {
		t.Fatalf("bad key: %d", resp.StatusCode)
	}
	if _, body := r.do(t, "GET", "/v1/auth/whoami", "", bearer("rk")); decode(t, body)["session"] != "key" {
		t.Fatalf("readonly key: %s", body)
	}
}

func TestPublicDemoWithoutKeysIsNotOpenAdmin(t *testing.T) {
	r := newRig(t, map[string]string{"PUBLIC_DEMO": "true"})
	if _, body := r.do(t, "GET", "/v1/auth/whoami", "", nil); decode(t, body)["role"] != "public" {
		t.Fatalf("whoami: %s", body)
	}
}

func TestPublicConfig(t *testing.T) {
	r := newRig(t, publicEnv(map[string]string{"PUBLIC_DEMO_QUESTIONS_PER_HOUR": "7", "PUBLIC_DEMO_MAX_QUESTION_CHARS": "40"}))
	_, body := r.do(t, "GET", "/v1/public/config", "", nil)
	m := decode(t, body)
	if m["public_demo"] != true || m["available"] != true || m["questions_per_hour"] != 7.0 || m["max_question_chars"] != 40.0 || m["admin_login"] != false {
		t.Fatalf("config: %s", body)
	}
	if _, ok := m["turnstile_site_key"]; ok {
		t.Fatal("no site key without turnstile")
	}
}

func TestPublicChatIsReadOnlyAndBounded(t *testing.T) {
	r := newRig(t, publicEnv(map[string]string{"PUBLIC_DEMO_MAX_QUESTION_CHARS": "40"}))
	resp, body := r.do(t, "POST", "/v1/chat/completions", chatBody(strings.Repeat("x", 41)), nil)
	if resp.StatusCode != 422 || !strings.Contains(body, "Please keep questions under 40 characters.") {
		t.Fatalf("long question: %d %s", resp.StatusCode, body)
	}

	r.model.replies = []llm.Message{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "run_kubectl", Args: map[string]any{"command": "delete pod web -n shop"}}}},
		{Content: "done"},
	}
	_, body = r.do(t, "POST", "/v1/chat/completions", chatBody("delete web"), map[string]string{"X-Session-ID": "s1"})
	for _, f := range frames(body) {
		if ch, ok := f["choices"].([]any); ok && len(ch) > 0 && ch[0].(map[string]any)["hitl_required"] == true {
			t.Fatal("a public caller must never be offered an approval")
		}
	}
	if b, _ := os.ReadFile(r.log); strings.Contains(string(b), "delete") {
		t.Fatal("a public caller ran a write")
	}
	// A short client chosen session id is replaced, a long enough one is kept.
	if fs := frames(body); len(fs) == 0 || fs[0]["session_id"] == "s1" {
		t.Fatalf("short session id kept: %v", fs[:1])
	}
	_, body = r.do(t, "POST", "/v1/chat/completions", chatBody("hi"), map[string]string{"X-Session-ID": "s-pub-12345678"})
	if fs := frames(body); fs[0]["session_id"] != "s-pub-12345678" {
		t.Fatalf("session id: %v", fs[0])
	}
}

func TestPublicBudgetStopsQuestionsButNotKeys(t *testing.T) {
	// Each scripted call costs 7 prompt tokens, so one answer spends far more than $1.
	r := newRig(t, publicEnv(map[string]string{"PUBLIC_DEMO_DAILY_BUDGET_USD": "1", "KI_V5_SPEND_IN_PRICE_PER_1K": "1000"}))
	if resp, _ := r.do(t, "POST", "/v1/chat/completions", chatBody("hi"), nil); resp.StatusCode != 200 {
		t.Fatalf("first: %d", resp.StatusCode)
	}
	resp, body := r.do(t, "POST", "/v1/chat/completions", chatBody("hi"), nil)
	if resp.StatusCode != 503 || !strings.Contains(body, "resets at 00:00 UTC") {
		t.Fatalf("over budget: %d %s", resp.StatusCode, body)
	}
	if _, body := r.do(t, "GET", "/v1/public/config", "", nil); decode(t, body)["available"] != false {
		t.Fatalf("config: %s", body)
	}
	if resp, _ := r.do(t, "POST", "/v1/chat/completions", chatBody("hi"), bearer("ak")); resp.StatusCode != 200 {
		t.Fatalf("admin key: %d", resp.StatusCode)
	}
}

func TestGuardLimits(t *testing.T) {
	cfg := config.Load(func(k string) string {
		return map[string]string{"PUBLIC_DEMO_QUESTIONS_PER_HOUR": "2", "PUBLIC_DEMO_MAX_CONCURRENT": "2", "PUBLIC_DEMO_DAILY_BUDGET_USD": "1"}[k]
	})
	g := newPublicGuard(cfg)
	now := time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	status := func(err error) int {
		var he *HTTPError
		if errors.As(err, &he) {
			return he.Status
		}
		return 0
	}

	done, err := g.Admit("a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Admit("a"); status(err) != 429 || !strings.Contains(err.Error(), "current question") {
		t.Fatalf("same client twice: %v", err)
	}
	other, err := g.Admit("b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Admit("c"); status(err) != 503 || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("over concurrency: %v", err)
	}
	done()
	done() // release is idempotent
	other()

	d2, _ := g.Admit("a")
	d2()
	if _, err := g.Admit("a"); status(err) != 429 || !strings.Contains(err.Error(), "used your 2 questions for this hour") {
		t.Fatalf("hourly: %v", err)
	}
	now = now.Add(61 * time.Minute)
	d3, err := g.Admit("a")
	if err != nil {
		t.Fatalf("after an hour: %v", err)
	}
	d3()

	g.Record(1_000_000, 0)
	if g.Available() {
		t.Fatal("over budget must close the demo")
	}
	now = now.Add(14 * time.Hour) // past 00:00 UTC
	if !g.Available() {
		t.Fatal("the budget resets at midnight UTC")
	}
}

func adminEnv(t *testing.T) (map[string]string, string) {
	t.Helper()
	pw := "correct-horse-battery"
	h, err := hashPassword(pw, []byte("0123456789abcdef"), 1000)
	if err != nil {
		t.Fatal(err)
	}
	return publicEnv(map[string]string{"ADMIN_USERNAME": "admin", "ADMIN_PASSWORD_HASH": h}), pw
}

func login(t *testing.T, r *rig, user, pw string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"username": user, "password": pw})
	h := map[string]string{"Content-Type": "application/json", csrfHeader: "1"}
	for k, v := range headers {
		h[k] = v
	}
	return r.do(t, "POST", "/v1/admin/login", string(b), h)
}

func TestAdminLoginCookieAndCSRF(t *testing.T) {
	env, pw := adminEnv(t)
	r := newRig(t, env)

	resp, body := r.do(t, "POST", "/v1/admin/login", `{"username":"admin","password":"`+pw+`"}`, nil)
	if resp.StatusCode != 403 || !strings.Contains(body, "cross-site") {
		t.Fatalf("no csrf header: %d %s", resp.StatusCode, body)
	}
	if resp, _ := login(t, r, "admin", pw, map[string]string{"Origin": "https://evil.example"}); resp.StatusCode != 403 {
		t.Fatalf("cross site origin: %d", resp.StatusCode)
	}
	if resp, body := login(t, r, "root", pw, nil); resp.StatusCode != 401 || !strings.Contains(body, "Invalid username or password.") {
		t.Fatalf("wrong user: %d %s", resp.StatusCode, body)
	}

	resp, body = login(t, r, "admin", pw, nil)
	if resp.StatusCode != 200 || decode(t, body)["role"] != "admin" {
		t.Fatalf("login: %d %s", resp.StatusCode, body)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == adminCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || len(cookie.Value) != 64 || cookie.MaxAge != 8*3600 {
		t.Fatalf("cookie: %+v", cookie)
	}
	jar := map[string]string{"Cookie": adminCookie + "=" + cookie.Value}

	if _, body := r.do(t, "GET", "/v1/auth/whoami", "", jar); decode(t, body)["role"] != "admin" || decode(t, body)["session"] != "cookie" {
		t.Fatalf("whoami: %s", body)
	}
	// The rig has no memory store, so preferences answer 503; the point is that the cookie is let in.
	if resp, _ := r.do(t, "GET", "/v1/preferences", "", jar); resp.StatusCode == 401 || resp.StatusCode == 403 {
		t.Fatalf("read with cookie: %d", resp.StatusCode)
	}
	pref := `{"key":"a","value":"b"}`
	if resp, _ := r.do(t, "PUT", "/v1/preferences", pref, jar); resp.StatusCode != 403 {
		t.Fatalf("cookie write without csrf header: %d", resp.StatusCode)
	}
	jar[csrfHeader] = "1"
	if resp, body := r.do(t, "PUT", "/v1/preferences", pref, jar); resp.StatusCode == 403 {
		t.Fatalf("cookie write with csrf header: %d %s", resp.StatusCode, body)
	}

	if resp, _ := r.do(t, "POST", "/v1/admin/logout", "", jar); resp.StatusCode != 200 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if _, body := r.do(t, "GET", "/v1/auth/whoami", "", jar); decode(t, body)["role"] != "public" {
		t.Fatalf("after logout: %s", body)
	}
}

func TestAdminLoginLockout(t *testing.T) {
	env, pw := adminEnv(t)
	r := newRig(t, env)
	for i := 0; i < 5; i++ {
		if resp, _ := login(t, r, "admin", "wrong", nil); resp.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, resp.StatusCode)
		}
	}
	resp, body := login(t, r, "admin", pw, nil)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "900" || !strings.Contains(body, "15 minutes") {
		t.Fatalf("locked: %d %s", resp.StatusCode, body)
	}
}

func TestTurnstileGatesPublicQuestions(t *testing.T) {
	r := newRig(t, publicEnv(map[string]string{"TURNSTILE_SITE_KEY": "site", "TURNSTILE_SECRET_KEY": "secret"}))
	r.srv.verifyHuman = func(_ *http.Request, token string) (bool, error) { return token == "good", nil }

	if _, body := r.do(t, "GET", "/v1/public/config", "", nil); decode(t, body)["turnstile_site_key"] != "site" || decode(t, body)["verified"] != false {
		t.Fatalf("config: %s", body)
	}
	if resp, body := r.do(t, "POST", "/v1/chat/completions", chatBody("hi"), nil); resp.StatusCode != 403 || !strings.Contains(body, "verification_required") {
		t.Fatalf("unverified: %d %s", resp.StatusCode, body)
	}
	if resp, _ := r.do(t, "POST", "/v1/public/verify", `{"token":"bad"}`, nil); resp.StatusCode != 403 {
		t.Fatalf("bad token: %d", resp.StatusCode)
	}
	if resp, _ := r.do(t, "POST", "/v1/public/verify", `{}`, nil); resp.StatusCode != 422 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	resp, _ := r.do(t, "POST", "/v1/public/verify", `{"token":"good"}`, nil)
	var human *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == humanCookie {
			human = c
		}
	}
	if resp.StatusCode != 200 || human == nil || !human.HttpOnly || human.MaxAge != 3600 {
		t.Fatalf("verify: %d %+v", resp.StatusCode, human)
	}
	jar := map[string]string{"Cookie": humanCookie + "=" + human.Value}
	if resp, _ := r.do(t, "POST", "/v1/chat/completions", chatBody("hi"), jar); resp.StatusCode != 200 {
		t.Fatalf("verified: %d", resp.StatusCode)
	}
	forged := map[string]string{"Cookie": humanCookie + "=9999999999.00000000000000000000000000000000"}
	if resp, _ := r.do(t, "POST", "/v1/chat/completions", chatBody("hi"), forged); resp.StatusCode != 403 {
		t.Fatalf("forged cookie: %d", resp.StatusCode)
	}
	// Keyed callers never face the check.
	if resp, _ := r.do(t, "POST", "/v1/chat/completions", chatBody("hi"), bearer("rk")); resp.StatusCode != 200 {
		t.Fatalf("keyed: %d", resp.StatusCode)
	}
}

func TestPasswordHashFormatMatchesAdminHash(t *testing.T) {
	// Made by the earlier build's `kube-sre admin-hash`, so existing settings keep working.
	h := "pbkdf2-sha256:600000:v79VbjwRi-kMpKDlfklYkQ:edABfaPtHzLVHNG_N-ZnEPnOFukcSKosHfwC0ZlSItE"
	if !ValidHash(h) || !verifyPassword(h, "correct-horse-battery") || verifyPassword(h, "correct-horse-batterz") {
		t.Fatal("existing hash")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short passwords are refused")
	}
	if ValidHash("plain") || ValidHash("pbkdf2-sha256:x:y:z") {
		t.Fatal("not hashes")
	}
}
