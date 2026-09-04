package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/DinethShakya23/kube-sre/internal/recorder"
)

func canWrite(role string) bool {
	return role == "operator" || role == "admin" || role == "superadmin"
}

func (s *Server) prefsGate() error {
	if !s.Cfg.PreferenceMemory {
		return &HTTPError{404, "Preference memory is disabled."}
	}
	if s.Memory == nil {
		return &HTTPError{503, "Memory is not active in this process."}
	}
	return nil
}

func userParam(r *http.Request) string {
	if u := r.URL.Query().Get("user"); u != "" {
		return u
	}
	return "default"
}

// listPreferences is GET /v1/preferences. An unreadable store is a 503, never an
// empty 200: "you have no preferences" and "I could not read them" are different
// answers, and only one of them invites the operator to enter everything again.
func (s *Server) listPreferences(w http.ResponseWriter, r *http.Request, _ string) {
	if err := s.prefsGate(); err != nil {
		writeError(w, err)
		return
	}
	user := userParam(r)
	prefs, err := s.Memory.RecallPreferences(r.Context(), user, 50)
	if err != nil {
		writeError(w, &HTTPError{503, err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": user, "preferences": prefs})
}

func (s *Server) setPreference(w http.ResponseWriter, r *http.Request, role string) {
	if err := s.prefsGate(); err != nil {
		writeError(w, err)
		return
	}
	if !canWrite(role) {
		writeError(w, &HTTPError{403, "operator role required"})
		return
	}
	var body struct {
		Key   string `json:"key"`
		Value string `json:"value"`
		User  string `json:"user"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Key == "" {
		writeError(w, &HTTPError{422, "key and value are required"})
		return
	}
	if body.User == "" {
		body.User = "default"
	}
	if !s.Memory.SetPreference(r.Context(), body.User, body.Key, body.Value, "explicit", nil) {
		writeError(w, &HTTPError{500, "failed to store preference"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": body.User, "key": body.Key, "value": body.Value, "source": "explicit"})
}

func (s *Server) forgetPreference(w http.ResponseWriter, r *http.Request, role string) {
	if err := s.prefsGate(); err != nil {
		writeError(w, err)
		return
	}
	if !canWrite(role) {
		writeError(w, &HTTPError{403, "operator role required"})
		return
	}
	user, key := userParam(r), r.PathValue("key")
	if !s.Memory.ForgetPreference(r.Context(), user, key) {
		writeError(w, &HTTPError{500, "failed to forget preference"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": user, "key": key, "forgotten": true})
}

// episodeReplay is GET /v1/episodes/{id}/replay: the durable, tamper evident
// replay. Unlike the in memory session replay it reads the hash chained flight
// recorder and verifies the chain before streaming. The first frame is a
// replay_meta record, then each recorded event in seq order, then [DONE].
func (s *Server) episodeReplay(w http.ResponseWriter, r *http.Request, _ string) {
	id := r.PathValue("id")
	if s.Recorder == nil {
		writeError(w, &HTTPError{503, "the flight recorder is not active in this process"})
		return
	}
	rows, err := s.Recorder.FetchEpisode(r.Context(), id)
	if err != nil {
		// 503, never 404. A 404 is a positive claim that the episode does not exist,
		// and this is the audit surface: telling an operator mid incident that the
		// episode they are living through was never recorded, because the recorder
		// happens to be off, is the most expensive wrong answer this API can give.
		var unavailable = errors.Is(err, recorder.ErrUnavailable)
		if unavailable {
			writeError(w, &HTTPError{503, fmt.Sprintf("%v - this is not the same as episode '%s' having no records", err, id)})
			return
		}
		writeError(w, &HTTPError{500, err.Error()})
		return
	}
	verdict := s.Recorder.VerifyEpisode(r.Context(), id, rows)
	if len(rows) == 0 {
		switch {
		case !verdict.Verified:
			// With an unreadable anchor there is no basis for either claim: absence and
			// total truncation look identical from here.
			writeError(w, &HTTPError{503, fmt.Sprintf("the chain anchor for episode '%s' could not be read, so an episode that never existed "+
				"cannot be told apart from one whose records were all removed. This is NOT a statement that '%s' has no records.", id, id)})
		case !verdict.Valid:
			// 404 would launder a total truncation into an absence.
			writeError(w, &HTTPError{409, fmt.Sprintf("episode '%s' has no surviving records but its chain anchor says it had some - every record "+
				"has been removed. This is NOT the same as the episode never existing.", id)})
		default:
			writeError(w, &HTTPError{404, fmt.Sprintf("no recorded episode '%s'", id)})
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	meta, _ := json.Marshal(map[string]any{"type": "replay_meta", "episode_id": id, "records": len(rows),
		"chain_valid": verdict.Valid, "chain_verified": verdict.Verified})
	fmt.Fprintf(w, "data: %s\n\n", meta)
	for _, row := range rows {
		// The row's kind is authoritative for the type column; not every payload
		// repeats it.
		p := map[string]any{}
		for k, v := range row.Payload {
			p[k] = v
		}
		p["type"] = row.Kind
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}
