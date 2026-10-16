package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/config"
)

// The public demo: callers with no key get the read only "public" role, held to an
// hourly question limit, a concurrency cap and a daily model budget shared by all.

const publicRole = "public"

// publicRoutes are the only routes a public caller may use.
var publicRoutes = map[string]bool{
	"POST /v1/chat/completions":        true,
	"GET /v1/auth/whoami":              true,
	"GET /v1/findings":                 true,
	"GET /v1/digest":                   true,
	"GET /v1/namespaces":               true,
	"GET /v1/episodes/{id}/replay":     true,
	"GET /v1/episodes/{id}/postmortem": true,
	"GET /v1/events/replay/{session}":  true,
}

func publicAllowed(pattern string) bool { return publicRoutes[pattern] }

// publicSessionRe bounds a client chosen session id, so a visitor cannot pick a
// short guessable id that someone else might also pick.
var publicSessionRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// publicGuard enforces the public limits. Counters are per process.
type publicGuard struct {
	cfg *config.Config
	now func() time.Time

	mu       sync.Mutex
	asked    map[string][]time.Time // client -> question times in the last hour
	running  map[string]int         // client -> questions in flight
	inFlight int
	day      string // UTC date the spend belongs to
	spent    float64
}

func newPublicGuard(cfg *config.Config) *publicGuard {
	return &publicGuard{cfg: cfg, now: time.Now, asked: map[string][]time.Time{}, running: map[string]int{}}
}

// rollover resets the spend at 00:00 UTC. Callers hold mu.
func (g *publicGuard) rollover(now time.Time) {
	if d := now.UTC().Format("2006-01-02"); d != g.day {
		g.day, g.spent = d, 0
	}
}

// overBudget is true once today's spend reached the cap. Callers hold mu.
func (g *publicGuard) overBudget() bool {
	return g.cfg.PublicDailyBudgetUSD > 0 && g.spent >= g.cfg.PublicDailyBudgetUSD
}

// Available reports whether the demo can take questions today.
func (g *publicGuard) Available() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollover(g.now())
	return !g.overBudget()
}

// Admit reserves a slot for one question from client, or says why not. The
// returned release must be called when the answer is done.
func (g *publicGuard) Admit(client string) (release func(), err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.rollover(now)
	if g.overBudget() {
		return nil, &HTTPError{503, "The demo has used today's budget. Please try again tomorrow (resets at 00:00 UTC)."}
	}
	recent := g.asked[client][:0]
	for _, t := range g.asked[client] {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	g.asked[client] = recent
	if limit := g.cfg.PublicQuestionsPerHour; limit > 0 && len(recent) >= limit {
		return nil, &HTTPError{429, fmt.Sprintf("You have used your %d questions for this hour. Please come back later.", limit)}
	}
	if g.inFlight >= max(g.cfg.PublicMaxConcurrent, 1) {
		return nil, &HTTPError{503, "The demo is busy right now. Please try again in a minute."}
	}
	if g.running[client] > 0 {
		return nil, &HTTPError{429, "Please wait for your current question to finish."}
	}
	g.asked[client] = append(recent, now)
	g.inFlight++
	g.running[client]++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.inFlight--
			if g.running[client]--; g.running[client] <= 0 {
				delete(g.running, client)
			}
		})
	}, nil
}

// Record adds the cost of a finished public answer to today's spend.
func (g *publicGuard) Record(promptTokens, completionTokens int) {
	usd := float64(promptTokens)/1000*g.cfg.V5SpendInPer1K + float64(completionTokens)/1000*g.cfg.V5SpendOutPer1K
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rollover(g.now())
	g.spent += usd
	if g.overBudget() {
		slog.Warn("public demo daily budget reached", "spent_usd", g.spent, "budget_usd", g.cfg.PublicDailyBudgetUSD)
	}
}

// Human verification: a passed Turnstile check earns a signed ks_human cookie of
// the form <expiry unix>.<hmac>, good for an hour.

const (
	humanCookie = "ks_human"
	humanTTL    = time.Hour
)

var turnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

func (s *Server) turnstileOn() bool { return s.Cfg.TurnstileEnabled() }

// humanKey derives the cookie signing key from the Turnstile secret, so a
// restart does not make every visitor solve the check again.
func (s *Server) humanKey() []byte {
	sum := sha256.Sum256([]byte("ks_human:" + s.Cfg.TurnstileSecret))
	return sum[:]
}

func (s *Server) humanSig(exp string) string {
	m := hmac.New(sha256.New, s.humanKey())
	m.Write([]byte(exp))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

func (s *Server) isHuman(r *http.Request) bool {
	c, err := r.Cookie(humanCookie)
	if err != nil {
		return false
	}
	exp, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	n, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Unix(n, 0).Before(time.Now()) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(s.humanSig(exp)), []byte(sig)) == 1
}

func (s *Server) issueHuman(w http.ResponseWriter, r *http.Request) {
	exp := strconv.FormatInt(time.Now().Add(humanTTL).Unix(), 10)
	http.SetCookie(w, &http.Cookie{
		Name: humanCookie, Value: exp + "." + s.humanSig(exp), Path: "/", MaxAge: int(humanTTL.Seconds()),
		HttpOnly: true, Secure: secureCookie(r), SameSite: http.SameSiteLaxMode,
	})
}

// cloudflareVerifier asks Turnstile whether token was solved by this client.
func (s *Server) cloudflareVerifier(r *http.Request, token string) (bool, error) {
	form := url.Values{"secret": {s.Cfg.TurnstileSecret}, "response": {token}, "remoteip": {s.Limiter.clientAddress(r)}}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm(turnstileVerifyURL, form)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var out struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	return out.Success, nil
}

// publicVerify is POST /v1/public/verify, which trades a Turnstile token for a ks_human cookie.
func (s *Server) publicVerify(w http.ResponseWriter, r *http.Request) {
	if !s.turnstileOn() {
		writeError(w, &HTTPError{404, "verification is not enabled"})
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Token) == "" {
		writeError(w, &HTTPError{422, "token is required"})
		return
	}
	verify := s.verifyHuman
	if verify == nil {
		verify = s.cloudflareVerifier
	}
	ok, err := verify(r, body.Token)
	if err != nil {
		slog.Warn("turnstile verification unreachable", "err", err)
		writeError(w, &HTTPError{503, "could not reach the verification service"})
		return
	}
	if !ok {
		writeError(w, &HTTPError{403, "verification failed"})
		return
	}
	s.issueHuman(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// publicConfig is GET /v1/public/config, what the page needs to know before it
// has a role: whether the demo is on and open today, its limits, and the sign in options.
func (s *Server) publicConfig(w http.ResponseWriter, r *http.Request) {
	c := s.Cfg
	out := map[string]any{"public_demo": c.PublicDemo, "admin_login": c.AdminLoginEnabled()}
	if c.PublicDemo {
		out["available"] = s.public.Available()
		out["questions_per_hour"] = c.PublicQuestionsPerHour
		out["max_question_chars"] = c.PublicMaxQuestionChars
		if s.turnstileOn() {
			out["turnstile_site_key"] = c.TurnstileSiteKey
			out["verified"] = s.isHuman(r)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// admitPublic applies the public checks to a question before the agent sees it.
func (s *Server) admitPublic(r *http.Request, message string) (func(), error) {
	if s.turnstileOn() && !s.isHuman(r) {
		return nil, &HTTPError{403, "verification_required"}
	}
	if n := s.Cfg.PublicMaxQuestionChars; n > 0 && len([]rune(message)) > n {
		return nil, &HTTPError{422, fmt.Sprintf("Please keep questions under %d characters.", n)}
	}
	return s.public.Admit(s.Limiter.clientAddress(r))
}
