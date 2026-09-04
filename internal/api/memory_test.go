package api

import (
	"context"
	"strings"
	"testing"

	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/memory"
	"github.com/DinethShakya23/kube-sre/internal/recorder"
	"github.com/DinethShakya23/kube-sre/internal/schema"
	"github.com/DinethShakya23/kube-sre/internal/store/storetest"
)

func memRig(t *testing.T, env map[string]string) (*rig, *recorder.Recorder) {
	t.Helper()
	r := newRig(t, env)
	db := storetest.New(t, schema.All())
	cfg := config.Load(func(k string) string { return env[k] })
	r.srv.Memory = memory.NewStore(db, cfg)
	rec := recorder.New(db, true, true)
	rec.Start(context.Background())
	r.srv.Recorder = rec
	return r, rec
}

func TestPreferencesRoundTripAndRoles(t *testing.T) {
	env := map[string]string{"KUBESRE_ADMIN_KEYS": "adm", "KUBESRE_READONLY_KEYS": "ro"}
	r, _ := memRig(t, env)

	resp, body := r.do(t, "PUT", "/v1/preferences", `{"key":"default_namespace","value":"shop","user":"bob"}`, bearer("adm"))
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	resp, body = r.do(t, "GET", "/v1/preferences?user=bob", "", bearer("ro"))
	if resp.StatusCode != 200 || !strings.Contains(body, `"default_namespace"`) || !strings.Contains(body, `"shop"`) {
		t.Errorf("read is open to readonly: %d %s", resp.StatusCode, body)
	}
	if resp, _ = r.do(t, "PUT", "/v1/preferences", `{"key":"k","value":"v"}`, bearer("ro")); resp.StatusCode != 403 {
		t.Errorf("readonly write: %d", resp.StatusCode)
	}
	if resp, _ = r.do(t, "DELETE", "/v1/preferences/default_namespace?user=bob", "", bearer("ro")); resp.StatusCode != 403 {
		t.Errorf("readonly delete: %d", resp.StatusCode)
	}
	if resp, _ = r.do(t, "DELETE", "/v1/preferences/default_namespace?user=bob", "", bearer("adm")); resp.StatusCode != 200 {
		t.Errorf("delete: %d", resp.StatusCode)
	}
	_, body = r.do(t, "GET", "/v1/preferences?user=bob", "", bearer("ro"))
	if strings.Contains(body, "default_namespace") {
		t.Errorf("still there: %s", body)
	}
	if resp, _ = r.do(t, "PUT", "/v1/preferences", `{"value":"v"}`, bearer("adm")); resp.StatusCode != 422 {
		t.Errorf("missing key: %d", resp.StatusCode)
	}
}

func TestPreferencesUnreadableIs503NotEmpty(t *testing.T) {
	r, _ := memRig(t, nil)
	_, _ = r.srv.Memory.DB.Exec(`DROP TABLE user_prefs`)
	resp, _ := r.do(t, "GET", "/v1/preferences", "", nil)
	if resp.StatusCode != 503 {
		t.Errorf("%d", resp.StatusCode)
	}
}

func TestPreferencesDisabled(t *testing.T) {
	r, _ := memRig(t, map[string]string{"PREFERENCE_MEMORY_ENABLED": "false"})
	if resp, _ := r.do(t, "GET", "/v1/preferences", "", nil); resp.StatusCode != 404 {
		t.Errorf("%d", resp.StatusCode)
	}
}

func TestEpisodeReplayVerifiesAndStreams(t *testing.T) {
	r, rec := memRig(t, nil)
	rec.Record("ep-1", "tool_call", map[string]any{"tool": "kubectl", "cmd": "get pods"})
	rec.Record("ep-1", "final", map[string]any{"text": "done"})
	rec.Close()

	resp, body := r.do(t, "GET", "/v1/episodes/ep-1/replay", "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	fs := frames(body)
	if len(fs) != 4 || fs[0]["type"] != "replay_meta" || fs[0]["records"] != float64(2) ||
		fs[0]["chain_valid"] != true || fs[0]["chain_verified"] != true {
		t.Fatalf("%+v", fs)
	}
	if fs[1]["type"] != "tool_call" || fs[2]["type"] != "final" {
		t.Errorf("row kind must be the type: %+v", fs)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Error("no terminator")
	}
}

func TestEpisodeReplayNeverSaysNotFoundWhenItCannotTell(t *testing.T) {
	r, rec := memRig(t, nil)
	if resp, _ := r.do(t, "GET", "/v1/episodes/never/replay", "", nil); resp.StatusCode != 404 {
		t.Errorf("unknown episode: %d", resp.StatusCode)
	}

	rec.Record("ep-2", "final", map[string]any{"text": "x"})
	rec.Close()
	// Rows removed, anchor kept: the store contradicts itself.
	if _, err := r.srv.Memory.DB.Exec(`DELETE FROM decision_log WHERE episode_id = 'ep-2'`); err != nil {
		t.Fatal(err)
	}
	if resp, _ := r.do(t, "GET", "/v1/episodes/ep-2/replay", "", nil); resp.StatusCode != 409 {
		t.Errorf("total truncation: %d", resp.StatusCode)
	}

	_, _ = r.srv.Memory.DB.Exec(`DROP TABLE decision_log`)
	if resp, _ := r.do(t, "GET", "/v1/episodes/ep-3/replay", "", nil); resp.StatusCode != 503 {
		t.Errorf("unreadable recorder: %d", resp.StatusCode)
	}
}

func TestEpisodeReplayWithoutARecorder(t *testing.T) {
	r := newRig(t, nil)
	if resp, _ := r.do(t, "GET", "/v1/episodes/x/replay", "", nil); resp.StatusCode != 503 {
		t.Errorf("%d", resp.StatusCode)
	}
}
