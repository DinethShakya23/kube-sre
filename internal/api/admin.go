package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Admin sign in: a username and password from the environment trade for an
// HttpOnly ks_admin session cookie, so a public demo needs no key in the browser.

const (
	adminCookie = "ks_admin"
	csrfHeader  = "X-KubeSre-CSRF"
	lockoutFor  = 15 * time.Minute
)

// adminSessions holds live session tokens, hashed. Sessions are per process.
type adminSessions struct {
	mu   sync.Mutex
	live map[string]time.Time // sha256(token) -> expiry
}

func newAdminSessions() *adminSessions { return &adminSessions{live: map[string]time.Time{}} }

func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (a *adminSessions) create(ttl time.Duration) string {
	b := make([]byte, 48)
	_, _ = rand.Read(b)
	token := base64.RawURLEncoding.EncodeToString(b)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, exp := range a.live {
		if now.After(exp) {
			delete(a.live, k)
		}
	}
	a.live[tokenKey(token)] = now.Add(ttl)
	return token
}

func (a *adminSessions) valid(token string) bool {
	if token == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.live[tokenKey(token)]
	return ok && time.Now().Before(exp)
}

func (a *adminSessions) drop(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.live, tokenKey(token))
}

// loginGuard locks a client out for 15 minutes after too many failed sign ins.
type loginGuard struct {
	max int
	mu  sync.Mutex
	bad map[string][]time.Time
}

func newLoginGuard(max int) *loginGuard { return &loginGuard{max: max, bad: map[string][]time.Time{}} }

// recent returns the failures inside the lockout window. Callers hold mu.
func (g *loginGuard) recent(client string, now time.Time) []time.Time {
	kept := g.bad[client][:0]
	for _, t := range g.bad[client] {
		if now.Sub(t) < lockoutFor {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(g.bad, client)
		return nil
	}
	g.bad[client] = kept
	return kept
}

func (g *loginGuard) locked(client string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.recent(client, time.Now())) >= max(g.max, 1)
}

func (g *loginGuard) fail(client string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	g.bad[client] = append(g.recent(client, now), now)
}

func (g *loginGuard) ok(client string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.bad, client)
}

// secureCookie is true when the browser reached us over HTTPS, directly or
// through a proxy that terminated TLS.
func secureCookie(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func unsafeMethod(m string) bool {
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

// csrfOK guards cookie authenticated writes: the custom header cannot be sent
// cross site without a CORS preflight, and an Origin from another site is refused.
func csrfOK(r *http.Request) bool {
	if r.Header.Get(csrfHeader) != "1" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

// adminLogin is POST /v1/admin/login.
func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	if !s.Cfg.AdminLoginEnabled() {
		writeError(w, &HTTPError{404, "Admin login is not set up on this server."})
		return
	}
	if !csrfOK(r) {
		writeError(w, &HTTPError{403, "Missing or cross-site request."})
		return
	}
	client := s.Limiter.clientAddress(r)
	if s.logins.locked(client) {
		w.Header().Set("Retry-After", strconv.Itoa(int(lockoutFor.Seconds())))
		writeError(w, &HTTPError{429, "Too many failed attempts. Try again in 15 minutes."})
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Username == "" || body.Password == "" {
		writeError(w, &HTTPError{422, "username and password are required"})
		return
	}
	// The password is always checked, so a wrong username costs the same time as a wrong password.
	passOK := verifyPassword(s.Cfg.AdminPasswordHash, body.Password)
	userOK := subtleEqual(body.Username, s.Cfg.AdminUsername)
	if !passOK || !userOK {
		s.logins.fail(client)
		slog.Warn("admin login failed", "ip", client)
		writeError(w, &HTTPError{401, "Invalid username or password."})
		return
	}
	s.logins.ok(client)
	ttl := time.Duration(max(s.Cfg.AdminSessionHours, 1)) * time.Hour
	http.SetCookie(w, &http.Cookie{
		Name: adminCookie, Value: s.admins.create(ttl), Path: "/", Expires: time.Now().Add(ttl), MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: secureCookie(r), SameSite: http.SameSiteStrictMode,
	})
	slog.Info("admin login", "ip", client)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "role": "admin"})
}

// adminLogout is POST /v1/admin/logout.
func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request) {
	if !csrfOK(r) {
		writeError(w, &HTTPError{403, "Missing or cross-site request."})
		return
	}
	if c, err := r.Cookie(adminCookie); err == nil {
		s.admins.drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: adminCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: secureCookie(r), SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// subtleEqual compares in constant time; hashing first hides the length too.
func subtleEqual(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}
