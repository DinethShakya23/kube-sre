package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/metrics"
)

type ctxKey int

const requestIDKey ctxKey = 1

// RequestID returns the id of the request in ctx, or "-".
func RequestID(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return "-"
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6], b[8] = (b[6]&0x0f)|0x40, (b[8]&0x3f)|0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	var he *HTTPError
	if errors.As(err, &he) {
		writeJSON(w, he.Status, map[string]any{"detail": he.Detail})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
}

// statusWriter records the status and keeps http.Flusher working, which the SSE
// stream needs.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijack not supported")
}

// silentPaths are too noisy to log at info: probes and metric scrapes.
var silentPaths = map[string]bool{"/healthz": true, "/metrics": true}

// Logging logs every request with its method, path, status and duration, reads or
// generates an X-Request-ID, echoes it in the response, and puts it in the request
// context so every downstream log line can carry it.
//
// It is the outermost middleware on purpose. The access line must be written while
// the request id is still bound, or the one line that maps a request to its status
// and duration is the one line that cannot be correlated with the request.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = newID()
		}
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey, id))
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		sw.Header().Set("X-Request-ID", id)
		start := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				elapsed := time.Since(start)
				slog.Error("unhandled panic", "method", r.Method, "path", r.URL.Path, "err", rec, "request_id", id,
					"duration_ms", elapsed.Milliseconds())
				if sw.status == http.StatusOK {
					writeJSON(sw, http.StatusInternalServerError, map[string]any{"detail": "Internal Server Error"})
				}
			}
			elapsed := time.Since(start)
			if !silentPaths[r.URL.Path] {
				level := slog.LevelInfo
				if sw.status >= 400 {
					level = slog.LevelWarn
				}
				slog.Log(r.Context(), level, r.Method+" "+r.URL.Path, "status", sw.status,
					"duration_ms", elapsed.Milliseconds(), "request_id", id)
			}
			if r.URL.Path != "/metrics" && r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
				metrics.Requests.Inc(r.Method, r.URL.Path, strconv.Itoa(sw.status))
				metrics.Latency.Observe(elapsed.Seconds(), r.Method, r.URL.Path)
			}
		}()
		next.ServeHTTP(sw, r)
	})
}

// CORS allows the configured origins, with credentials, all methods and all
// headers. Origins are compared as exact strings, so what stripping cannot repair
// (a trailing slash, a missing scheme, a bare *) is reported by the config check.
func CORS(origins []string, next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range origins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			w.Header().Add("Vary", "Origin")
		}
		if origin != "" && (allowed[origin] || allowed["*"]) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.Header().Set("Access-Control-Allow-Methods", "DELETE, GET, HEAD, OPTIONS, PATCH, POST, PUT")
				if h := r.Header.Get("Access-Control-Request-Headers"); h != "" {
					w.Header().Set("Access-Control-Allow-Headers", h)
				}
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusOK)
				return
			}
		} else if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			http.Error(w, "Disallowed CORS origin", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}
