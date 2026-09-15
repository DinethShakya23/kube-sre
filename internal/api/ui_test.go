package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestUIServedWithoutKeyButDataStillNeedsOne(t *testing.T) {
	r := newRig(t, map[string]string{"KUBESRE_ADMIN_KEYS": "ak"})

	resp, body := r.do(t, "GET", "/ui/", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "<title>kube-sre</title>") {
		t.Fatalf("ui: %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("no CSP on the UI")
	}
	if resp, _ := r.do(t, "GET", "/ui/app.js", "", nil); resp.StatusCode != 200 {
		t.Fatalf("app.js: %d", resp.StatusCode)
	}

	for _, p := range []string{"/v1/findings", "/v1/digest", "/v1/preferences", "/v1/auth/whoami"} {
		if resp, _ := r.do(t, "GET", p, "", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without a key: %d", p, resp.StatusCode)
		}
	}
	if resp, _ := r.do(t, "GET", "/v1/auth/whoami", "", map[string]string{"Authorization": "Bearer ak"}); resp.StatusCode != 200 {
		t.Fatalf("whoami with key: %d", resp.StatusCode)
	}
}
