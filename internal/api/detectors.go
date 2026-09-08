package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/DinethShakya23/kube-sre/internal/detect"
	"github.com/DinethShakya23/kube-sre/internal/detectstore"
)

const globalCluster = "global"

func (s *Server) detectorGate(role string, write bool) error {
	if !s.Cfg.NLDetectorAuthoring {
		return &HTTPError{404, "NL detector authoring is disabled."}
	}
	if s.Detectors == nil {
		return &HTTPError{503, "the detector store is not available in this process"}
	}
	if write && !canWrite(role) {
		return &HTTPError{403, "operator role required"}
	}
	return nil
}

// createDetector is POST /v1/detectors: compile plain English, validate, stage as a
// shadow candidate. Nothing reaches the watchtower without an explicit promotion.
func (s *Server) createDetector(w http.ResponseWriter, r *http.Request, role string) {
	if err := s.detectorGate(role, true); err != nil {
		writeError(w, err)
		return
	}
	var body struct {
		Description string `json:"description"`
		Name        string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Description) == "" {
		writeError(w, &HTTPError{422, "description is required"})
		return
	}
	raw := detectstore.Compile(r.Context(), s.Compiler, body.Description)
	label := body.Name
	if label == "" {
		label = "nl"
	}
	block, errs := detectstore.Validate(raw, label)
	if block == nil {
		writeJSON(w, http.StatusOK, map[string]any{"staged": false, "compiled": raw, "errors": errs})
		return
	}
	name := body.Name
	if name == "" {
		name = "nl:" + block.Playbook
	}
	if name == "nl:nl" {
		d := []rune(strings.TrimSpace(body.Description))
		if len(d) > 40 {
			d = d[:40]
		}
		name = "nl:" + strings.ReplaceAll(strings.TrimSpace(string(d)), " ", "-")
	}
	staged := s.Detectors.Stage(r.Context(), name, body.Description, raw, role, globalCluster)
	status := "not-staged"
	if staged {
		status = "shadow"
	}
	writeJSON(w, http.StatusOK, map[string]any{"staged": staged, "status": status, "name": name, "compiled": raw, "errors": errs,
		"note": "Shadow detectors observe only — promote after reviewing precision."})
}

// listDetectors is GET /v1/detectors. An unreadable store is a 503, not an empty
// 200: for a detector inventory the difference is whether the operator believes their
// cluster is unmonitored or merely unqueryable.
func (s *Server) listDetectors(w http.ResponseWriter, r *http.Request, role string) {
	if err := s.detectorGate(role, false); err != nil {
		writeError(w, err)
		return
	}
	rows, err := s.Detectors.List(r.Context(), r.URL.Query().Get("status"), globalCluster)
	if err != nil {
		writeError(w, &HTTPError{503, err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"detectors": rows})
}

func (s *Server) promoteDetector(w http.ResponseWriter, r *http.Request, role string) {
	if err := s.detectorGate(role, true); err != nil {
		writeError(w, err)
		return
	}
	name := r.PathValue("name")
	ok, err := s.Detectors.Promote(r.Context(), name, role, globalCluster)
	if errors.Is(err, detectstore.ErrCannotFire) {
		// 409, not a cheerful 200: flipping the row would answer status active about a
		// detector that can never match anything.
		writeError(w, &HTTPError{409, err.Error()})
		return
	}
	if !ok {
		writeError(w, &HTTPError{404, fmt.Sprintf("detector '%s' not found", name)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "status": "active", "reviewed_by": role})
}

func (s *Server) demoteDetector(w http.ResponseWriter, r *http.Request, role string) {
	if err := s.detectorGate(role, true); err != nil {
		writeError(w, err)
		return
	}
	name := r.PathValue("name")
	if !s.Detectors.Demote(r.Context(), name, role, globalCluster) {
		writeError(w, &HTTPError{404, fmt.Sprintf("detector '%s' not found", name)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "status": "demoted", "reviewed_by": role})
}

// shadowFindings is GET /v1/detectors/{name}/shadow-findings: what a shadow detector
// has fired, and what that count is worth. The number is the promote or reject
// decision, so an empty one has to say which kind of empty it is: a sensorium that is
// not running, a detector this process never loaded and a detector that ran quietly
// must not all read as "0 shadow firings".
func (s *Server) shadowFindings(w http.ResponseWriter, r *http.Request, role string) {
	if err := s.detectorGate(role, false); err != nil {
		writeError(w, err)
		return
	}
	name := r.PathValue("name")
	var eng *detect.Engine
	if s.Perception != nil {
		eng = s.Perception.Engine()
	}
	if eng == nil {
		writeError(w, &HTTPError{503, fmt.Sprintf("the detector engine is not running in this process, so no shadow detector has been evaluated. "+
			"This is NOT the same as '%s' having fired nothing, and it is not a basis for promoting or rejecting it.", name)})
		return
	}
	found := []map[string]any{}
	for _, f := range eng.ShadowFindings() {
		if f.Playbook == name {
			found = append(found, f.Dict())
		}
	}
	held, capacity, saturated := eng.ShadowRing()
	watching, why := s.watching(eng.ShadowBlock(name), name)
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name,
		// False also covers "the store was unreachable at the last refresh", so this
		// says "not evaluated here", never "no such detector".
		"watching": watching, "watching_reason": why, "findings": found,
		"buffer":  map[string]any{"held": held, "capacity": capacity, "saturated": saturated},
		"durable": false,
	})
}

// watching says whether the detector's predicate is actually being evaluated, and if
// not, why. It used to mean "the engine loaded it", a weaker claim than it reads as:
// a trend only detector with predictive detection off is loaded and never evaluated,
// and a bare false sent a reviewer to the predicate when the cause was a flag.
func (s *Server) watching(b *detect.DetectBlock, name string) (bool, string) {
	if b == nil {
		return false, fmt.Sprintf("%s is not in the engine's shadow set — it was not loaded (refused at load as unable to fire, malformed, "+
			"scoped to another cluster, or the detector store was unreachable at the last refresh). This is not a statement that no such "+
			"detector exists; see the server log for db_detector_can_never_fire.", name)
	}
	// A partially loaded detector is evaluated, but not as it was authored: the
	// predicate the reviewer sees in the store is not the predicate that ran.
	partial := ""
	if len(b.DroppedPredicates) > 0 {
		partial = fmt.Sprintf(" %d of its predicates were refused at load and are NOT evaluated (%s);", len(b.DroppedPredicates), b.DroppedPredicates[0])
	}
	if len(b.FiresOnHealthy) > 0 {
		partial += " WARNING: this detector fires on HEALTHY objects, so its findings are not evidence of a fault — " + b.FiresOnHealthy[0]
	}
	switch {
	case len(b.WatchPredicates) > 0:
		return true, "loaded, with watch predicates evaluated on every observation." + partial
	case len(b.TrendPredicates) > 0:
		if s.Cfg.PredictiveDetection {
			return true, "loaded, with trend predicates evaluated on the predictive interval." + partial
		}
		return false, fmt.Sprintf("%s is loaded but has only trend predicates, and PREDICTIVE_DETECTION_ENABLED is false — nothing evaluates them, "+
			"so it cannot fire on this deployment. Its zero firings are not evidence about the predicate.%s", name, partial)
	}
	return false, name + " compiled to no evaluable predicate"
}
