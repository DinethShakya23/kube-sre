package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/detect"
	"github.com/DinethShakya23/kube-sre/internal/detectstore"
	"github.com/DinethShakya23/kube-sre/internal/llm"
	"github.com/DinethShakya23/kube-sre/internal/perception"
	"github.com/DinethShakya23/kube-sre/internal/playbooks"
	"github.com/DinethShakya23/kube-sre/internal/schema"
	"github.com/DinethShakya23/kube-sre/internal/sensorium"
	"github.com/DinethShakya23/kube-sre/internal/store/storetest"
)

func detRig(t *testing.T, extra map[string]string) *rig {
	t.Helper()
	env := map[string]string{"NL_DETECTOR_AUTHORING_ENABLED": "true", "KUBESRE_ADMIN_KEYS": "adm", "KUBESRE_READONLY_KEYS": "ro"}
	for k, v := range extra {
		env[k] = v
	}
	r := newRig(t, env)
	r.srv.Detectors = detectstore.New(storetest.New(t, schema.All()))
	r.srv.Compiler = r.model
	return r
}

const oomBlock = `{"watch_predicates":[{"kind":"Pod","status_regex":"^OOMKilled$"}]}`

func TestDetectorLifecycleOverHTTP(t *testing.T) {
	r := detRig(t, nil)
	r.model.replies = []llm.Message{{Content: oomBlock}}

	resp, body := r.do(t, "POST", "/v1/detectors", `{"description":"pods killed for memory","name":"nl:oom"}`, bearer("adm"))
	var out map[string]any
	_ = json.Unmarshal([]byte(body), &out)
	if resp.StatusCode != 200 || out["staged"] != true || out["status"] != "shadow" || out["name"] != "nl:oom" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if _, body = r.do(t, "GET", "/v1/detectors?status=shadow", "", bearer("ro")); !strings.Contains(body, "nl:oom") {
		t.Errorf("list: %s", body)
	}
	if resp, _ = r.do(t, "POST", "/v1/detectors/nl:oom/promote", "", bearer("ro")); resp.StatusCode != 403 {
		t.Errorf("readonly promote: %d", resp.StatusCode)
	}
	resp, body = r.do(t, "POST", "/v1/detectors/nl:oom/promote", "", bearer("adm"))
	if resp.StatusCode != 200 || !strings.Contains(body, `"status":"active"`) || !strings.Contains(body, `"reviewed_by":"admin"`) {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
	if resp, _ = r.do(t, "POST", "/v1/detectors/nope/promote", "", bearer("adm")); resp.StatusCode != 404 {
		t.Errorf("unknown: %d", resp.StatusCode)
	}
	if resp, body = r.do(t, "POST", "/v1/detectors/nl:oom/demote", "", bearer("adm")); resp.StatusCode != 200 || !strings.Contains(body, "demoted") {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
}

func TestAuthoringReturnsErrorsInsteadOfStagingADeadDetector(t *testing.T) {
	r := detRig(t, nil)
	r.model.replies = []llm.Message{{Content: `{"watch_predicates":[{"kind":"Pod","status_regex":"^Running$"}]}`}}
	resp, body := r.do(t, "POST", "/v1/detectors", `{"description":"every pod"}`, bearer("adm"))
	if resp.StatusCode != 200 || !strings.Contains(body, `"staged":false`) || !strings.Contains(body, "HEALTHY Pod") {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
	if _, body = r.do(t, "GET", "/v1/detectors", "", bearer("ro")); strings.Contains(body, "nl:") {
		t.Errorf("nothing should be staged: %s", body)
	}

	r.model.replies = []llm.Message{{Content: `not json at all`}}
	if _, body = r.do(t, "POST", "/v1/detectors", `{"description":"x"}`, bearer("adm")); !strings.Contains(body, `"staged":false`) {
		t.Errorf("%s", body)
	}
	if resp, _ = r.do(t, "POST", "/v1/detectors", `{"description":"  "}`, bearer("adm")); resp.StatusCode != 422 {
		t.Errorf("empty description: %d", resp.StatusCode)
	}
}

func TestNamesAreDerivedWhenNotGiven(t *testing.T) {
	r := detRig(t, nil)
	r.model.replies = []llm.Message{{Content: oomBlock}}
	_, body := r.do(t, "POST", "/v1/detectors", `{"description":"pods killed for memory in prod"}`, bearer("adm"))
	if !strings.Contains(body, `"name":"nl:pods-killed-for-memory-in-prod"`) {
		t.Errorf("%s", body)
	}
}

func TestPromotingADeadStoredDetectorIs409(t *testing.T) {
	r := detRig(t, nil)
	r.srv.Detectors.Stage(context.Background(), "nl:dead", "x", map[string]any{"watch_predicates": []any{map[string]any{"kind": "Event", "reason_regex": "^(A | B)$"}}}, "op", "global")
	resp, body := r.do(t, "POST", "/v1/detectors/nl:dead/promote", "", bearer("adm"))
	if resp.StatusCode != 409 || !strings.Contains(body, "can never fire") {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
}

func TestDetectorListUnreadableIs503AndDisabledIs404(t *testing.T) {
	r := detRig(t, nil)
	_, _ = r.srv.Detectors.DB.Exec(`DROP TABLE detectors`)
	if resp, _ := r.do(t, "GET", "/v1/detectors", "", bearer("ro")); resp.StatusCode != 503 {
		t.Errorf("%d", resp.StatusCode)
	}
	off := newRig(t, map[string]string{"KUBESRE_ADMIN_KEYS": "adm"})
	if resp, _ := off.do(t, "GET", "/v1/detectors", "", bearer("adm")); resp.StatusCode != 404 {
		t.Errorf("disabled: %d", resp.StatusCode)
	}
}

func TestShadowFindingsSaysWhichKindOfEmpty(t *testing.T) {
	r := detRig(t, nil)
	resp, body := r.do(t, "GET", "/v1/detectors/nl:oom/shadow-findings", "", bearer("ro"))
	if resp.StatusCode != 503 || !strings.Contains(body, "NOT the same as 'nl:oom' having fired nothing") {
		t.Errorf("no engine: %d %s", resp.StatusCode, body)
	}

	cfg := config.Load(func(k string) string { return map[string]string{"NL_DETECTOR_AUTHORING_ENABLED": "true"}[k] })
	svc := perception.NewService(cfg, playbooks.Load())
	svc.Bin = r.bin
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); svc.Stop("", "") }()
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r.srv.Perception = svc
	eng := svc.Engine()

	resp, body = r.do(t, "GET", "/v1/detectors/nl:oom/shadow-findings", "", bearer("ro"))
	if resp.StatusCode != 200 || !strings.Contains(body, `"watching":false`) || !strings.Contains(body, "was not loaded") {
		t.Errorf("not loaded: %d %s", resp.StatusCode, body)
	}

	block, _ := detect.ParseBlock("nl:oom", map[string]any{"watch_predicates": []any{map[string]any{"kind": "Pod", "status_regex": "^OOMKilled$"}}})
	eng.SetStoredDetectors(nil, []detect.DetectBlock{*block})
	eng.Process(sensorium.Observation{Kind: "pod_status", ClusterID: "c", Namespace: "shop", Name: "web", TS: time.Now(), Fields: map[string]any{"status": "OOMKilled"}})

	resp, body = r.do(t, "GET", "/v1/detectors/nl:oom/shadow-findings", "", bearer("ro"))
	var out struct {
		Watching bool             `json:"watching"`
		Reason   string           `json:"watching_reason"`
		Findings []map[string]any `json:"findings"`
		Buffer   map[string]any   `json:"buffer"`
		Durable  bool             `json:"durable"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if resp.StatusCode != 200 || !out.Watching || len(out.Findings) != 1 || out.Findings[0]["object"] != "web" || out.Buffer["held"] != float64(1) ||
		out.Buffer["saturated"] != false || out.Durable {
		t.Errorf("%d %s", resp.StatusCode, body)
	}

	// A trend only detector with predictive detection off is loaded but never evaluated.
	tb, _ := detect.ParseBlock("nl:trend", map[string]any{"trend_predicates": []any{map[string]any{"metric": "x", "threshold": 1}}})
	eng.SetStoredDetectors(nil, []detect.DetectBlock{*tb})
	_, body = r.do(t, "GET", "/v1/detectors/nl:trend/shadow-findings", "", bearer("ro"))
	if !strings.Contains(body, `"watching":false`) || !strings.Contains(body, "PREDICTIVE_DETECTION_ENABLED is false") {
		t.Errorf("%s", body)
	}
}
