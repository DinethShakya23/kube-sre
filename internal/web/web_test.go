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
		"/ui/assets/app.js":  "javascript",
		"/ui/assets/app.css": "text/css",
		"/ui/favicon.svg":    "image/svg",
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
