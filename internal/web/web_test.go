package web

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func serve(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", Redirect)
	mux.Handle("GET /ui/", Handler(false))
	mux.Handle("GET /admin", AdminPage())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func TestAdminPageIsTheAppAndNeverCached(t *testing.T) {
	rec := serve(t, "/admin")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("admin: %d", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("headers: %v", rec.Header())
	}
}

func TestTurnstileWidensTheCSPOnlyWhenOn(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler(true).ServeHTTP(rec, httptest.NewRequest("GET", "/ui/", nil))
	on := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(on, "script-src 'self' https://challenges.cloudflare.com") || !strings.Contains(on, "frame-src https://challenges.cloudflare.com") {
		t.Fatalf("csp with turnstile: %q", on)
	}
	if off := serve(t, "/ui/").Header().Get("Content-Security-Policy"); strings.Contains(off, "cloudflare") {
		t.Fatalf("csp without turnstile: %q", off)
	}
}

func TestRootRedirects(t *testing.T) {
	rec := serve(t, "/")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/" {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// assetPath finds a built asset by pattern in index.html, since its name carries a content hash.
func assetPath(t *testing.T, pattern string) string {
	t.Helper()
	m := regexp.MustCompile(pattern).FindString(serve(t, "/ui/").Body.String())
	if m == "" {
		t.Fatalf("index.html references no %s", pattern)
	}
	return m
}

func TestAssetsServed(t *testing.T) {
	types := map[string]string{}
	types["/ui/"] = "text/html"
	types[assetPath(t, `/ui/assets/app-[^"]+\.js`)] = "javascript"
	types[assetPath(t, `/ui/assets/app-[^"]+\.css`)] = "text/css"
	types["/ui/favicon.svg"] = "image/svg"
	for path, want := range types {
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
	if !strings.Contains(body, `id="root"`) {
		t.Fatal("missing the #root the app mounts on")
	}
	// Every script is a module loaded by src; a <script> with a body would be blocked.
	for _, m := range regexp.MustCompile(`<script[^>]*>([^<]*)</script>`).FindAllStringSubmatch(body, -1) {
		if strings.TrimSpace(m[1]) != "" {
			t.Fatalf("inline script: %q", m[0])
		}
	}
}

// The page is built in ui/ and committed. An index.html pointing at a file that
// was not embedded would serve a blank page.
func TestIndexReferencesEmbeddedFiles(t *testing.T) {
	body := serve(t, "/ui/").Body.String()
	refs := regexp.MustCompile(`(?:src|href)="(/ui/[^"]+)"`).FindAllStringSubmatch(body, -1)
	if len(refs) < 3 {
		t.Fatalf("expected the script, stylesheet and icon, got %v", refs)
	}
	for _, r := range refs {
		if rec := serve(t, r[1]); rec.Code != 200 {
			t.Fatalf("%s: %d", r[1], rec.Code)
		}
	}
}

func TestScripts(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	// The markdown renderer feeds dangerouslySetInnerHTML, so its XSS cases run here too.
	if out, err := exec.Command(node, "../../ui/src/lib/md_check.mjs").CombinedOutput(); err != nil {
		t.Fatalf("md_check: %v\n%s", err, out)
	}
}
