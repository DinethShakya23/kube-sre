// Package api is the HTTP server: authentication, rate limiting, the streaming
// chat endpoint, and the read endpoints.
//
// Role model, four tiers:
//
//	superadmin  all operations, approval gated; bypasses the infrastructure namespace block
//	admin       high and medium risk operations, always approval gated; infra namespaces blocked
//	operator    medium risk operations only (create, apply, scale, exec), approval gated
//	readonly    reads only; every write is rejected before it reaches the agent
//
// With no keys configured every request is treated as admin, so a first run works.
package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/config"
)

// HTTPError is an error that carries its status.
type HTTPError struct {
	Status int
	Detail string
}

func (e *HTTPError) Error() string { return e.Detail }

const demoPrefix = "ks-ro-"

func hmacHex(secret, payload string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// verifyDemoKey reports whether a token is a valid, unexpired HMAC signed demo key
// of the form ks-ro-<base64url(email:exp)>.<hmac hex[:32]>.
func verifyDemoKey(secret, token string, now time.Time) bool {
	if secret == "" || !strings.HasPrefix(token, demoPrefix) {
		return false
	}
	rest := token[len(demoPrefix):]
	i := strings.LastIndex(rest, ".")
	if i < 0 {
		return false
	}
	payload, sig := rest[:i], rest[i+1:]
	if subtle.ConstantTimeCompare([]byte(hmacHex(secret, payload)), []byte(sig)) != 1 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(payload, "="))
	if err != nil {
		return false
	}
	s := string(raw)
	exp, err := strconv.ParseInt(s[strings.LastIndex(s, ":")+1:], 10, 64)
	return err == nil && exp > now.Unix()
}

// MintDemoKey signs a readonly demo key for an email and lifetime.
func MintDemoKey(secret, email string, ttl time.Duration, now time.Time) (key string, exp int64, err error) {
	if secret == "" {
		return "", 0, errors.New("DEMO_KEY_HMAC_SECRET not configured")
	}
	exp = now.Add(ttl).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%d", email, exp)))
	return demoPrefix + payload + "." + hmacHex(secret, payload), exp, nil
}

// matches is a constant time membership test. A map lookup, and the equality on
// the way to it, exit early, so response time would vary with how many leading
// bytes of a valid key the attacker guessed. The loop still short circuits, which
// leaks only which role matched, never key content, and the role is about to be
// returned to the caller anyway.
func matches(token string, keys []string) bool {
	for _, k := range keys {
		if subtle.ConstantTimeCompare([]byte(token), []byte(k)) == 1 {
			return true
		}
	}
	return false
}

// Authenticator turns a request into a role.
type Authenticator struct {
	Cfg *config.Config
	Now func() time.Time
}

func (a *Authenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Role returns superadmin, admin, operator or readonly, or an HTTPError with 401.
func (a *Authenticator) Role(r *http.Request) (string, error) {
	c := a.Cfg
	if c.OpenAccess() {
		// Fail open, deliberately, for local development: a first run with no keys
		// must work. The same default in a data centre would silently grant every
		// unauthenticated caller admin, so REQUIRE_AUTH turns that into a refusal.
		// Checked here as well as at startup: a gate that exists in one place has a
		// single point of failure.
		if c.RequireAuth {
			return "", &HTTPError{401, "REQUIRE_AUTH is set but no API keys are configured - refusing to treat an unauthenticated caller as admin."}
		}
		return "admin", nil
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return "", &HTTPError{401, "Authorization: Bearer <api_key> required"}
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	switch {
	case matches(token, c.SuperadminKeys):
		return "superadmin", nil
	case matches(token, c.AdminKeys):
		return "admin", nil
	case matches(token, c.OperatorKeys):
		return "operator", nil
	case matches(token, c.ReadonlyKeys):
		return "readonly", nil
	case c.AuthBackend == "hmac" && verifyDemoKey(c.DemoKeySecret, token, a.now()):
		return "readonly", nil
	}
	return "", &HTTPError{401, "Invalid API key"}
}
