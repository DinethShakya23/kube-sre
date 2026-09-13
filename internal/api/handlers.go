package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/nsguard"
)

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"status":  "ok",
		"arm":     "v1",
		"version": s.Version,
	}
	// Liveness checks nothing by design: a liveness probe that touches a dependency
	// turns one blip into a restart loop. The blocks below report state only, and
	// none of them moves the top level status.
	for name, f := range s.Health {
		resp[name] = f()
	}
	// Runtime identity: which slices this process was configured with.
	mem, _ := resp["memory"].(map[string]any)
	state, _ := mem["state"].(string)
	resp["experimental_flags"] = s.Cfg.ActiveFlags()
	resp["set_but_unwired_flags"] = s.Cfg.SetButUnwired()
	resp["degraded_experimental_flags"] = s.Cfg.DegradedFlags(state)
	if _, ok := resp["leader"]; !ok {
		resp["leader"] = map[string]any{"enabled": false, "is_leader": true, "reason": "no election - single process"}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if !s.ready.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "draining"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request, role string) {
	// The role of the caller's own key, never another caller's. A client cannot
	// infer its privileges from a key prefix, which is only a naming convention.
	writeJSON(w, http.StatusOK, map[string]any{"role": role})
}

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func (s *Server) mintKey(w http.ResponseWriter, r *http.Request, role string) {
	if role != "admin" && role != "superadmin" {
		writeError(w, &HTTPError{403, "admin role required"})
		return
	}
	var body struct {
		Email    string `json:"email"`
		TTLHours *int   `json:"ttl_hours"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, &HTTPError{422, "invalid request body"})
		return
	}
	if !emailRe.MatchString(body.Email) {
		writeError(w, &HTTPError{422, "invalid email"})
		return
	}
	if body.TTLHours != nil && *body.TTLHours < 1 {
		writeError(w, &HTTPError{422, "ttl_hours must be at least 1"})
		return
	}
	if s.Cfg.DemoKeySecret == "" {
		writeError(w, &HTTPError{503, "DEMO_KEY_HMAC_SECRET not configured"})
		return
	}
	ttl := s.Cfg.DemoKeyDefaultTTL
	if body.TTLHours != nil {
		ttl = *body.TTLHours
	}
	if ttl > s.Cfg.DemoKeyMaxTTL {
		writeError(w, &HTTPError{400, fmt.Sprintf("ttl_hours exceeds max (%d)", s.Cfg.DemoKeyMaxTTL)})
		return
	}
	key, exp, err := MintDemoKey(s.Cfg.DemoKeySecret, body.Email, time.Duration(ttl)*time.Hour, time.Now())
	if err != nil {
		writeError(w, &HTTPError{503, err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"api_key": key, "email": body.Email, "expires_at": exp, "expires_in_seconds": ttl * 3600,
	})
}

// namespaces lists the namespaces the caller may see. Protected ones are removed
// for the same reason run_kubectl removes them, and a failed read is a 503, never
// an empty list: an empty list means exactly one thing, that the cluster has no
// namespaces the caller may see. A short list also says it is short, in the same
// marker a filtered -o json listing carries, because silence reads as a definite
// absence to a client.
func (s *Server) namespaces(w http.ResponseWriter, r *http.Request, _ string) {
	visible, dropped, err := s.listNamespaces(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if visible == nil {
		visible = []string{}
	}
	note := ""
	if dropped > 0 {
		note = nsguard.WithheldSentence(dropped, "namespace")
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespaces": visible, "withheldByPolicy": note})
}

// replay streams the events this process holds for a session, then [DONE].
//
// It is a 404 when this process holds no history, and the detail says why that is
// not the same as the session having produced nothing: the history is in memory
// only, so it does not survive a restart and is not shared between replicas.
func (s *Server) replay(w http.ResponseWriter, r *http.Request, _ string) {
	id := r.PathValue("session")
	if !s.Emitter.HasHistory(id) {
		writeError(w, &HTTPError{404, fmt.Sprintf("this process holds no event history for session '%s'. That history is in memory only - "+
			"it does not survive a restart and is not shared between replicas - so this is NOT evidence that the session never ran or "+
			"produced nothing. For a durable answer read GET /v1/episodes/%s/replay, which streams the hash-chained flight recorder.", id, id)})
		return
	}
	history := s.Emitter.History(id)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	// A meta frame first: without it a zero event replay is a bare [DONE], and the
	// client cannot tell "this session emitted nothing" from "I was cut off".
	meta := map[string]any{"type": "replay_meta", "session_id": id, "records": len(history), "durable": false}
	b, _ := json.Marshal(meta)
	fmt.Fprintf(w, "data: %s\n\n", b)
	for _, ev := range history {
		b, _ := json.Marshal(ev)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}
