package web

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func serve(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", Redirect)
	mux.Handle("GET /ui/", Handler())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func TestRootRedirects(t *testing.T) {
	rec := serve(t, "/")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/" {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestAssetsServed(t *testing.T) {
	for path, want := range map[string]string{
		"/ui/":            "text/html",
		"/ui/app.js":      "javascript",
		"/ui/md.js":       "javascript",
		"/ui/app.css":     "text/css",
		"/ui/favicon.svg": "image/svg",
	} {
		rec := serve(t, path)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, want) {
			t.Fatalf("%s: content type %q", path, ct)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := serve(t, "/ui/").Header()
	if !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
		strings.Contains(h.Get("Content-Security-Policy"), "unsafe-inline") {
		t.Fatalf("csp: %q", h.Get("Content-Security-Policy"))
	}
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" {
		t.Fatal("missing headers")
	}
}

func TestUnknownAssetIs404(t *testing.T) {
	if rec := serve(t, "/ui/nope.js"); rec.Code != 404 {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestPageHasNoInlineCode(t *testing.T) {
	body := serve(t, "/ui/").Body.String()
	if strings.Contains(body, "onclick=") || strings.Contains(body, "<style") {
		t.Fatal("inline code would be blocked by the CSP")
	}
	for _, id := range []string{"thread", "composer", "findTable", "digestBody", "reportBody", "statusBody", "detList", "prefList", "keyDialog"} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Fatalf("missing #%s", id)
		}
	}
}

func TestScripts(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	for _, args := range [][]string{{"--check", "static/app.js"}, {"--check", "static/md.js"}, {"md_check.js"}} {
		if out, err := exec.Command(node, args...).CombinedOutput(); err != nil {
			t.Fatalf("node %v: %v\n%s", args, err, out)
		}
	}
}
