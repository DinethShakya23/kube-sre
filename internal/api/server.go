package api

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/agent"
	"github.com/DinethShakya23/kube-sre/internal/audit"
	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/events"
	"github.com/DinethShakya23/kube-sre/internal/memory"
	"github.com/DinethShakya23/kube-sre/internal/metrics"
	"github.com/DinethShakya23/kube-sre/internal/nsguard"
	"github.com/DinethShakya23/kube-sre/internal/perception"
	"github.com/DinethShakya23/kube-sre/internal/recorder"
)

// StatusFunc reports the state of one subsystem for /healthz.
type StatusFunc func() map[string]any

// Server is the HTTP API.
type Server struct {
	Cfg     *config.Config
	Agent   *agent.Agent
	Emitter *events.Emitter
	Audit   *audit.Log
	// Memory serves preferences, and Recorder serves durable episode replay.
	Memory   *memory.Store
	Recorder *recorder.Recorder
	// Perception is the sensorium and detector service, when one runs.
	Perception *perception.Service
	Auth       *Authenticator
	Limiter    *Limiter
	// Version is the software version reported on /healthz.
	Version string
	// Health lists subsystems reported on /healthz, keyed by their field name
	// (audit, recorder, memory, sensorium, leader, db_schema).
	Health map[string]StatusFunc

	// extra routes registered by subsystems: pattern -> handler, all authenticated.
	routes map[string]http.HandlerFunc

	ready atomic.Bool
}

func NewServer(cfg *config.Config, ag *agent.Agent, em *events.Emitter, version string) *Server {
	return &Server{
		Cfg: cfg, Agent: ag, Emitter: em, Version: version,
		Auth: &Authenticator{Cfg: cfg}, Limiter: NewLimiter(cfg),
		Health: map[string]StatusFunc{}, routes: map[string]http.HandlerFunc{},
	}
}

// SetReady marks the process as willing, or no longer willing, to serve.
//
// Readiness is local only, so /readyz can never cascade: a probe that pings the
// database looks more thorough and is more dangerous, because when the shared
// database blips every replica goes unready at once and the Service has no
// endpoints, converting a degraded system into a total outage. Dependency health
// belongs in alerting. This flag does not drain a rolling update either: the
// listening socket closes before the shutdown hook runs, so what holds it open
// across endpoint propagation is the chart's preStop sleep.
func (s *Server) SetReady(v bool) { s.ready.Store(v) }

// Handle registers an authenticated route under /v1.
func (s *Server) Handle(pattern string, h http.HandlerFunc) { s.routes[pattern] = h }

// authed wraps a handler with authentication. Authentication is a property of the
// router, not of each handler: everything mounted through it is authenticated, so
// a route added tomorrow inherits the gate instead of needing to remember it.
func (s *Server) authed(h func(w http.ResponseWriter, r *http.Request, role string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		role, err := s.Auth.Role(r)
		if err != nil {
			writeError(w, err)
			return
		}
		h(w, r, role)
	}
}

// Handler builds the full HTTP handler with its middleware. The order matters:
// logging is outermost so the request id is bound while the access line is written;
// CORS wraps the limiter so a 429 still carries Access-Control-Allow-Origin, which
// a browser needs to show the status instead of an opaque network error; the
// limiter is inside logging so a rejection still appears in the access log.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	// Liveness and readiness must answer an unauthenticated kubelet, so they are
	// mounted outside the authenticated routes.
	mux.HandleFunc("GET /v1/healthz", s.healthz)
	mux.HandleFunc("GET /v1/readyz", s.readyz)
	if s.Cfg.MetricsEnabled {
		// Unauthenticated on purpose: a scrape target that needs a bearer token
		// silently stops being scraped when the token rotates. It exposes counts and
		// latencies only, no cluster data, prompts or credentials.
		mux.Handle("GET /metrics", metrics.Default.Handler())
	}

	mux.HandleFunc("POST /v1/chat/completions", s.authed(s.chat))
	mux.HandleFunc("GET /v1/events/replay/{session}", s.authed(s.replay))
	mux.HandleFunc("GET /v1/namespaces", s.authed(s.namespaces))
	mux.HandleFunc("GET /v1/findings", s.authed(s.findings))
	mux.HandleFunc("GET /v1/preferences", s.authed(s.listPreferences))
	mux.HandleFunc("PUT /v1/preferences", s.authed(s.setPreference))
	mux.HandleFunc("DELETE /v1/preferences/{key}", s.authed(s.forgetPreference))
	mux.HandleFunc("GET /v1/episodes/{id}/replay", s.authed(s.episodeReplay))
	mux.HandleFunc("GET /v1/auth/whoami", s.authed(s.whoami))
	mux.HandleFunc("POST /v1/auth/demo-keys", s.authed(s.mintKey))
	for pattern, h := range s.routes {
		h := h
		mux.HandleFunc(pattern, s.authed(func(w http.ResponseWriter, r *http.Request, _ string) { h(w, r) }))
	}

	return Logging(CORS(s.Cfg.AllowedOrigins, s.Limiter.Middleware(mux)))
}

// ListenAndServe serves until ctx ends, then drains for a short while.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}

// ── helpers shared by handlers ───────────────────────────────────────────────

// listNamespaces reads the namespaces the caller may see: protected ones are
// removed, and the number withheld comes back so the response can say it is short.
func (s *Server) listNamespaces(ctx context.Context) (visible []string, dropped int, err error) {
	kc := s.Cfg.KubeconfigPath
	if strings.HasPrefix(kc, "~/") {
		if home, herr := os.UserHomeDir(); herr == nil {
			kc = filepath.Join(home, kc[2:])
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "kubectl", "get", "namespaces", "-o", "jsonpath={.items[*].metadata.name}")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kc)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, rerr := cmd.Output()
	if rerr != nil {
		switch {
		case errorsIsNotFound(rerr):
			return nil, 0, &HTTPError{503, "Cannot list namespaces: kubectl is not installed on the server."}
		case cctx.Err() != nil:
			return nil, 0, &HTTPError{503, "Cannot list namespaces: kubectl did not respond within 10s."}
		}
		// Only the first stderr line is passed on: it carries the actionable part
		// ("connection refused", "Unauthorized") without echoing a dump.
		detail := "kubectl failed"
		if lines := strings.Split(strings.TrimSpace(stderr.String()), "\n"); len(lines) > 0 && lines[0] != "" {
			detail = clipRunes(lines[0], 300)
		}
		return nil, 0, &HTTPError{503, "Cannot list namespaces: " + detail}
	}
	blocked := nsguard.Blocklist(s.Cfg.BlockedNamespaces)
	names := strings.Fields(string(out))
	for _, n := range names {
		if !blocked[strings.ToLower(n)] {
			visible = append(visible, n)
		}
	}
	return visible, len(names) - len(visible), nil
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func errorsIsNotFound(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "executable file not found") || os.IsNotExist(err))
}
