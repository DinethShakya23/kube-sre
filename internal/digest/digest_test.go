package digest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/llm"
	"github.com/DinethShakya23/kube-sre/internal/memory"
	"github.com/DinethShakya23/kube-sre/internal/perception"
	"github.com/DinethShakya23/kube-sre/internal/recorder"
	"github.com/DinethShakya23/kube-sre/internal/schema"
	"github.com/DinethShakya23/kube-sre/internal/store/storetest"
)

func cfgWith(env map[string]string) *config.Config {
	return config.Load(func(k string) string { return env[k] })
}

type rig struct {
	b   *Builder
	rec *recorder.Recorder
	pm  *PostmortemBuilder
}

func newRig(t *testing.T, env map[string]string) *rig {
	t.Helper()
	db := storetest.New(t, schema.All())
	cfg := cfgWith(env)
	rec := recorder.New(db, true, true)
	rec.Start(context.Background())
	b := &Builder{DB: db, Cfg: cfg, Perception: func() perception.State { return perception.State{Sensorium: perception.Active} }}
	return &rig{b: b, rec: rec, pm: &PostmortemBuilder{Builder: *b, Recorder: rec}}
}

func TestQuietWatchOnlyWhenEverythingWasReadable(t *testing.T) {
	r := newRig(t, nil)
	d := r.b.Build(context.Background(), 24)
	if d.Degraded || !strings.HasPrefix(d.Summary, "Quiet watch") {
		t.Errorf("%+v", d)
	}
}

func TestDisabledSourcesAreNeverQuiet(t *testing.T) {
	r := newRig(t, map[string]string{"FLIGHT_RECORDER_ENABLED": "false", "WATCHTOWER_ENABLED": "false"})
	d := r.b.Build(context.Background(), 24)
	if !d.Degraded || len(d.DegradedReasons) < 2 || !strings.Contains(d.Summary, "NOT a quiet watch") || !strings.Contains(d.Summary, "(+1 more") {
		t.Errorf("%+v", d)
	}
	r.b.Perception = func() perception.State {
		return perception.State{Sensorium: perception.Disabled, SensoriumReason: "sensorium is off"}
	}
	d = r.b.Build(context.Background(), 24)
	found := false
	for _, x := range d.DegradedReasons {
		found = found || strings.Contains(x, "sensorium is off")
	}
	if !found {
		t.Errorf("perception gap missing: %v", d.DegradedReasons)
	}
}

func TestAFailedQueryIsReportedNotHidden(t *testing.T) {
	r := newRig(t, nil)
	_, _ = r.b.DB.Exec(`DROP TABLE decision_log`)
	d := r.b.Build(context.Background(), 24)
	if !d.Degraded || !strings.Contains(strings.Join(d.DegradedReasons, " "), "decision_log query failed") {
		t.Errorf("%+v", d.DegradedReasons)
	}
}

func TestDigestCollectsFindingsRollbacksAndSessions(t *testing.T) {
	r := newRig(t, nil)
	r.rec.Record("findings:c1", "finding", map[string]any{"playbook": "oom", "namespace": "shop", "object": "pod/web", "severity": "predicted", "eta_minutes": 9.0})
	r.rec.Record("sess-1", "rollback_point", map[string]any{"rollback_id": "rb1", "command": "kubectl delete deploy web", "restorable": false, "capture_notes": []any{"truncated"}})
	r.rec.Record("sess-1", "final", map[string]any{"text": "done"})
	r.rec.Record("sess-2", "final", map[string]any{"text": "done"})
	r.rec.Record("auto-x", "final", map[string]any{"text": "done"})
	r.rec.Close()

	d := r.b.Build(context.Background(), 24)
	if len(d.Findings) != 1 || d.Findings[0].Playbook != "oom" || d.Findings[0].ETAMinutes == nil {
		t.Errorf("findings %+v", d.Findings)
	}
	if len(d.RollbackPoints) != 1 || d.RollbackPoints[0].Restorable == nil || *d.RollbackPoints[0].Restorable {
		t.Errorf("rollback %+v", d.RollbackPoints)
	}
	if d.UserSessions != 2 {
		t.Errorf("sessions %d (auto episodes and findings streams are not user sessions)", d.UserSessions)
	}
	md := d.Markdown()
	for _, want := range []string{"## Detector findings", "predicted ~9m", "NOT restorable — do not apply (truncated)", "0 of 1 restorable", "2 user session(s)"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
}

func TestDigestWindowExcludesOldRows(t *testing.T) {
	r := newRig(t, nil)
	r.rec.Record("findings:c1", "finding", map[string]any{"playbook": "oom"})
	r.rec.Close()
	r.b.Now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	if d := r.b.Build(context.Background(), 24); len(d.Findings) != 0 {
		t.Errorf("%+v", d.Findings)
	}
}

func TestAutonomousInvestigationsComeFromEpisodes(t *testing.T) {
	r := newRig(t, nil)
	s := memory.NewStore(r.b.DB, r.b.Cfg)
	ok := true
	s.WriteEpisode(context.Background(), memory.EpisodeInput{ClusterID: "c1", TriggerKind: "detector", TriggerDetail: "autonomous investigation of crashloop",
		Summary: "fixed the crashloop", Outcome: "resolved", Verified: &ok, Namespace: "shop"})
	s.WriteEpisode(context.Background(), memory.EpisodeInput{ClusterID: "c1", TriggerKind: "user_query", Summary: "a chat"})
	d := r.b.Build(context.Background(), 24)
	if len(d.AutoInvestigation) != 1 || d.AutoInvestigation[0].Outcome != "resolved" {
		t.Fatalf("%+v", d.AutoInvestigation)
	}
	if !strings.Contains(d.Summary, "1 verified fix(es)") {
		t.Errorf("%s", d.Summary)
	}
}

func TestPostmortemFromARecordedEpisode(t *testing.T) {
	r := newRig(t, nil)
	r.rec.Record("ep1", "finding", map[string]any{"playbook": "crashloop", "namespace": "shop", "object": "pod/web"})
	r.rec.Record("ep1", "tool_call", map[string]any{"tool": "kubectl", "command": "get pods"})
	r.rec.Record("ep1", "rollback_point", map[string]any{"rollback_id": "rb1", "command": "kubectl set image", "restorable": true})
	r.rec.Record("ep1", "error", map[string]any{"error": "boom"})
	r.rec.Record("ep1", "final", map[string]any{"text": "the image tag was wrong"})
	r.rec.Close()

	pm := r.pm.Build(context.Background(), "ep1")
	if !pm.ChainValid || !pm.ChainVerified || len(pm.Timeline) != 5 || len(pm.WhatFired) != 1 || len(pm.Investigated) != 1 ||
		len(pm.Tried) != 1 || len(pm.Errors) != 1 || pm.RootCause == nil || *pm.RootCause != "the image tag was wrong" {
		t.Fatalf("%+v", pm)
	}
	if pm.Timeline[0].At == 0 {
		t.Error("timeline has no clock")
	}
	md := pm.Markdown()
	for _, want := range []string{"✅ Audit chain verified intact", "## Root cause", "`[#0]`", "## What fired", "## What was tried (mutations)", "## Errors encountered"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q:\n%s", want, md)
		}
	}
}

func TestPostmortemSeparatesEmptyFromUnreadableFromTruncated(t *testing.T) {
	r := newRig(t, nil)
	pm := r.pm.Build(context.Background(), "never")
	if pm.ChainVerified || pm.Summary != "No recorded events for this episode." || !pm.RecorderUp {
		t.Errorf("empty: %+v", pm)
	}
	if strings.Contains(pm.Markdown(), "✅") {
		t.Error("an empty episode must not print the intact banner")
	}

	r.rec.Record("gone", "final", map[string]any{"text": "x"})
	r.rec.Close()
	_, _ = r.b.DB.Exec(`DELETE FROM decision_log WHERE episode_id = 'gone'`)
	pm = r.pm.Build(context.Background(), "gone")
	if !pm.ChainVerified || pm.ChainValid || !strings.Contains(pm.Summary, "Every event has been removed") {
		t.Errorf("total truncation: %+v", pm)
	}

	_, _ = r.b.DB.Exec(`DROP TABLE decision_log`)
	pm = r.pm.Build(context.Background(), "x")
	if pm.RecorderUp || !strings.Contains(pm.Summary, "NOT the same as the episode having no events") {
		t.Errorf("unreadable: %+v", pm)
	}
}

func TestPostmortemAnnouncesGapsAndFailedEnrichment(t *testing.T) {
	r := newRig(t, nil)
	r.rec.Record("ep2", recorder.GapKind, map[string]any{"dropped": 3.0, "reason": "db down"})
	r.rec.Record("ep2", "final", map[string]any{"text": "ok"})
	r.rec.Close()
	_, _ = r.b.DB.Exec(`DROP TABLE episodes`)
	pm := r.pm.Build(context.Background(), "ep2")
	if pm.EventsLost != 3 || len(pm.EnrichmentFailed) != 1 {
		t.Fatalf("%+v", pm)
	}
	md := pm.Markdown()
	if !strings.Contains(md, "RECORD INCOMPLETE") || !strings.Contains(md, "db down") || !strings.Contains(md, "POSTMORTEM INCOMPLETE") {
		t.Errorf("%s", md)
	}
}

func TestPostmortemTakesRootCauseAndOutcomeFromTheEpisode(t *testing.T) {
	r := newRig(t, nil)
	s := memory.NewStore(r.b.DB, r.b.Cfg)
	ok := true
	s.WriteEpisode(context.Background(), memory.EpisodeInput{ClusterID: "c1", TriggerKind: "user_query", Summary: "s", RootCause: "OOM at 512Mi",
		Outcome: "resolved", Verified: &ok, RequestID: "ep3"})
	r.rec.Record("ep3", "final", map[string]any{"text": "done"})
	r.rec.Close()
	pm := r.pm.Build(context.Background(), "ep3")
	if pm.RootCause == nil || *pm.RootCause != "OOM at 512Mi" || pm.Worked[len(pm.Worked)-1] != "outcome: resolved (verified)" {
		t.Errorf("%+v", pm)
	}
}

type fakeNarrator struct {
	seen string
	err  error
}

func (f *fakeNarrator) Chat(_ context.Context, msgs []llm.Message, _ []llm.ToolSpec, _ llm.Options) (*llm.Response, error) {
	f.seen = msgs[len(msgs)-1].Content
	if f.err != nil {
		return nil, f.err
	}
	return &llm.Response{Message: llm.Message{Content: " The pod crashed [#0]. "}}, nil
}

func TestNarrativeIsOptInAndItsFailureIsSaid(t *testing.T) {
	r := newRig(t, map[string]string{"POSTMORTEM_LLM_NARRATIVE": "true"})
	r.rec.Record("ep4", "final", map[string]any{"text": "done"})
	r.rec.Close()
	n := &fakeNarrator{}
	r.pm.Narrator = n
	pm := r.pm.Build(context.Background(), "ep4")
	if pm.Narrative == nil || *pm.Narrative != "The pod crashed [#0]." || !strings.Contains(n.seen, "`[#0]`") {
		t.Errorf("%+v / %s", pm.Narrative, n.seen)
	}
	n.err = context.DeadlineExceeded
	pm = r.pm.Build(context.Background(), "ep4")
	if pm.Narrative != nil || len(pm.EnrichmentFailed) != 1 {
		t.Errorf("%+v", pm)
	}

	off := newRig(t, nil)
	off.pm.Narrator = &fakeNarrator{}
	off.rec.Record("ep5", "final", map[string]any{"text": "done"})
	off.rec.Close()
	if pm := off.pm.Build(context.Background(), "ep5"); pm.Narrative != nil || len(pm.EnrichmentFailed) != 0 {
		t.Errorf("flag off: %+v", pm)
	}
}
