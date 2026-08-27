package api

import (
	"net/http"
	"strconv"

	"github.com/DinethShakya23/kube-sre/internal/perception"
)

// findings is GET /v1/findings: recent detector firings, which cost no model
// calls. The list is an in memory ring; the durable record is in the flight
// recorder under the episode findings:<cluster_id>.
//
// sensorium and predictive come from the perception classifier, which the
// morning digest reads too, so the two surfaces answer for the same window and
// cannot disagree about whether anything was watching. shed_total above zero in
// queue means the sensorium is dropping observations and detection is lossy; it
// sits next to streams because "connected but shedding" and "not connected" are
// different failures with the same symptom.
func (s *Server) findings(w http.ResponseWriter, r *http.Request, _ string) {
	limit, since := 100, 0.0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeError(w, &HTTPError{422, "limit must be an integer from 1 to 500"})
			return
		}
		limit = n
	}
	if v := r.URL.Query().Get("since"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			writeError(w, &HTTPError{422, "since must be a number, 0 or more"})
			return
		}
		since = f
	}
	svc := s.Perception
	if svc == nil {
		writeJSON(w, http.StatusOK, map[string]any{"sensorium": perception.Disabled,
			"sensorium_reason": "perception is not configured in this process", "streams": []any{},
			"queue": map[string]any{"shed_total": 0, "high_water": 0, "maxsize": 0}, "findings": []any{}})
		return
	}
	st := svc.State()
	q := svc.Queue()
	if st.Sensorium == perception.Disabled {
		writeJSON(w, http.StatusOK, map[string]any{"sensorium": st.Sensorium, "sensorium_reason": st.SensoriumReason,
			"streams": []any{}, "queue": q, "findings": []any{}})
		return
	}
	found := []map[string]any{}
	if eng := svc.Engine(); eng != nil {
		found = eng.RecentFindings(limit, since)
		if found == nil {
			found = []map[string]any{}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sensorium": st.Sensorium, "sensorium_reason": st.SensoriumReason, "detectors": st.Detectors,
		"predictive": st.Predictive, "predictive_detectors": st.PredictiveDetectors, "predictive_error": st.PredictiveError,
		"streams": st.Streams, "queue": q, "findings": found,
	})
}
