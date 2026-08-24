package api

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/config"
)

// Per caller API rate limiting.
//
// Keyed by identity, not address: behind an Ingress every request arrives from one
// address, so an address keyed limiter would throttle the whole tenancy the moment
// one client misbehaved. The key is a SHA-256 of the bearer token, so the raw key
// is never stored, logged, or kept in a heap dump. Address is the fallback only
// when a request carries no bearer token.
//
// The address fallback is only meaningful once you say who the proxies are.
// X-Forwarded-For is written by the client, so trusting it unconditionally would
// let a caller mint a fresh bucket per request. RATE_LIMIT_TRUSTED_PROXY_HOPS
// (default 0, ignore the header) says how many proxies sit in front; the client is
// then read that many entries from the right, the end a proxy appends to and a
// client cannot forge. A header shorter than the declared depth did not come from
// that chain and is ignored.
//
// Probe and metrics paths are exempt, as a safety property: a limiter that can
// answer 429 to a liveness probe can restart the pod under the load it exists to
// survive, and a throttled scrape target silently becomes an unmonitored one. The
// bucket table is bounded, with least recently seen eviction, because the limiter
// runs before authentication and an unbounded map keyed on attacker chosen tokens
// would be a memory exhaustion vector dressed as a defence.
//
// Two limits an operator must know. The counters are per replica, so N replicas
// admit up to N times the rate per caller. And this is fair use, not DDoS
// defence: volumetric abuse belongs at the ingress.

var exemptPaths = map[string]bool{"/healthz": true, "/readyz": true, "/metrics": true, "/v1/healthz": true, "/v1/readyz": true}

type bucket struct {
	key     string
	tokens  float64
	updated time.Time
}

// Limiter is a token bucket per caller.
type Limiter struct {
	cfg *config.Config
	mu  sync.Mutex
	ll  *list.List // most recently seen at the front
	idx map[string]*list.Element
}

func NewLimiter(cfg *config.Config) *Limiter {
	return &Limiter{cfg: cfg, ll: list.New(), idx: map[string]*list.Element{}}
}

// Allow spends one token for key and returns (allowed, retry after).
func (l *Limiter) Allow(key string, now time.Time) (bool, time.Duration) {
	capacity := float64(max(l.cfg.RateLimitBurst, 1))
	refill := float64(max(l.cfg.RateLimitPerMin, 1)) / 60.0
	l.mu.Lock()
	defer l.mu.Unlock()
	var b *bucket
	if el, ok := l.idx[key]; ok {
		b = el.Value.(*bucket)
		elapsed := math.Max(now.Sub(b.updated).Seconds(), 0)
		b.tokens = math.Min(capacity, b.tokens+elapsed*refill)
		b.updated = now
		l.ll.MoveToFront(el)
	} else {
		l.evictIfFull()
		b = &bucket{key: key, tokens: capacity, updated: now}
		l.idx[key] = l.ll.PushFront(b)
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	// Seconds until one whole token exists again: the honest Retry-After, so a
	// client backs off by the amount that will actually succeed.
	wait := math.Max((1-b.tokens)/refill, 1)
	return false, time.Duration(wait * float64(time.Second))
}

func (l *Limiter) evictIfFull() {
	limit := max(l.cfg.RateLimitMaxTracked, 1)
	for l.ll.Len() >= limit {
		el := l.ll.Back()
		b := el.Value.(*bucket)
		l.ll.Remove(el)
		delete(l.idx, b.key)
		slog.Warn("rate limit bucket table full, evicted the least recently seen caller. Volumetric abuse belongs at the ingress, not here.",
			"limit", limit, "evicted", b.key[:min(10, len(b.key))])
	}
}

// CallerKey identifies the caller without retaining its credential.
func (l *Limiter) CallerKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		if tok := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")); tok != "" {
			sum := sha256.Sum256([]byte(tok))
			return "k:" + hex.EncodeToString(sum[:])[:32]
		}
	}
	return "ip:" + l.clientAddress(r)
}

func (l *Limiter) clientAddress(r *http.Request) string {
	peer := "unknown"
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		peer = host
	} else if r.RemoteAddr != "" {
		peer = r.RemoteAddr
	}
	hops := l.cfg.RateLimitProxyHops
	if hops <= 0 {
		return peer
	}
	var parts []string
	for _, p := range strings.Split(r.Header.Get("X-Forwarded-For"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) < hops {
		return peer
	}
	return parts[len(parts)-hops]
}

// Middleware sheds load with a 429 and Retry-After.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.cfg.RateLimit || exemptPaths[r.URL.Path] || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		ok, retry := l.Allow(l.CallerKey(r), time.Now())
		if ok {
			next.ServeHTTP(w, r)
			return
		}
		slog.Warn("rate limit 429", "method", r.Method, "path", r.URL.Path,
			"per_min", l.cfg.RateLimitPerMin, "burst", l.cfg.RateLimitBurst)
		secs := int(retry.Seconds())
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(l.cfg.RateLimitPerMin))
		w.Header().Set("X-RateLimit-Remaining", "0")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"detail": fmt.Sprintf(
			"Rate limit exceeded: %d requests/minute per API key (burst %d). Retry after %ds.",
			l.cfg.RateLimitPerMin, l.cfg.RateLimitBurst, secs)})
	})
}
