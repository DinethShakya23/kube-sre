package api

import (
	"net/http"
	"strconv"
)

func formatParam(r *http.Request) (string, error) {
	f := r.URL.Query().Get("format")
	if f == "" {
		return "json", nil
	}
	if f != "json" && f != "markdown" {
		return "", &HTTPError{422, "format must be json or markdown"}
	}
	return f, nil
}

// digestHandler is GET /v1/digest, the morning digest rendered from the flight recorder.
func (s *Server) digestHandler(w http.ResponseWriter, r *http.Request, _ string) {
	hours := 24.0
	if v := r.URL.Query().Get("hours"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 || f > 168 {
			writeError(w, &HTTPError{422, "hours must be a number above 0 and at most 168"})
			return
		}
		hours = f
	}
	format, err := formatParam(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if s.Digest == nil {
		writeError(w, &HTTPError{503, "the digest is not available in this process"})
		return
	}
	d := s.Digest.Build(r.Context(), hours)
	if format == "markdown" {
		writeJSON(w, http.StatusOK, map[string]any{"markdown": d.Markdown()})
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// postmortemHandler is GET /v1/episodes/{id}/postmortem.
func (s *Server) postmortemHandler(w http.ResponseWriter, r *http.Request, _ string) {
	if !s.Cfg.PostmortemEnabled {
		writeError(w, &HTTPError{404, "Postmortems are disabled."})
		return
	}
	format, err := formatParam(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if s.Postmortem == nil {
		writeError(w, &HTTPError{503, "postmortems are not available in this process"})
		return
	}
	pm := s.Postmortem.Build(r.Context(), r.PathValue("id"))
	if format == "markdown" {
		// The verdict rides alongside the prose, not instead of it, so a client can
		// map it to an exit code (3 broken, 4 not verified) instead of only printing
		// the banner. Additive: callers that read just "markdown" are unaffected.
		writeJSON(w, http.StatusOK, map[string]any{
			"markdown": pm.Markdown(), "chain_valid": pm.ChainValid, "chain_verified": pm.ChainVerified,
			"events_lost": pm.EventsLost, "gaps": pm.Gaps, "enrichment_failed": pm.EnrichmentFailed,
		})
		return
	}
	writeJSON(w, http.StatusOK, pm)
}
