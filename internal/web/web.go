// Package web serves the built-in browser UI. The assets are static and
// unauthenticated; every data call the page makes goes through the API and its
// key check.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var assets embed.FS

const csp = "default-src 'self'; img-src 'self' data:; style-src 'self'; connect-src 'self'; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'self'; script-src 'self'"

// turnstileCSP also lets in Cloudflare's human check, which loads a script and an iframe.
const turnstileCSP = csp + " https://challenges.cloudflare.com; frame-src https://challenges.cloudflare.com"

func secure(h http.Header, turnstile bool) {
	if turnstile {
		h.Set("Content-Security-Policy", turnstileCSP)
	} else {
		h.Set("Content-Security-Policy", csp)
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
}

func static() fs.FS {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	return sub
}

// Handler serves the UI under /ui/.
func Handler(turnstile bool) http.Handler {
	files := http.StripPrefix("/ui/", http.FileServerFS(static()))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secure(w.Header(), turnstile)
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

// AdminPage serves the sign in page at /admin. It is the same app, which shows
// the sign in form when it finds itself on that path.
func AdminPage() http.Handler {
	page, err := fs.ReadFile(static(), "index.html")
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secure(w.Header(), false)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
}

// Redirect sends the bare root to the UI.
func Redirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/", http.StatusFound)
}
