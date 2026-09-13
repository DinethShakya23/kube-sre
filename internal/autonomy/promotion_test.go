package autonomy

import (
	"context"
	"strings"
	"testing"

	rec "github.com/DinethShakya23/kube-sre/internal/recorder"
	"github.com/DinethShakya23/kube-sre/internal/spans"
	"github.com/DinethShakya23/kube-sre/internal/store/storetest"
)

func outcomesDB(t *testing.T) *Outcomes {
	t.Helper()
	db := storetest.New(t, PromotionMigrations, rec.Migrations)
	now := 0.0
	return &Outcomes{DB: db, Now: func() float64 { now++; return now }}
}

func b(v bool) *bool { return &v }

func TestDecideIsFastDownSlowUp(t *testing.T) {
	good := run(40, nil, 20, 3)
	d, err := Decide("scale", "L2->L3", "L2", 20, good, false, false, false)
	if err != nil || d.Action != "promote" || d.ToRung != "L3" || d.N != 40 {
		t.Fatalf("%+v %v", d, err)
	}
	if d, _ = Decide("scale", "L3->L4:versioned-workload", "L3", 20, good, false, false, false); d.Action != "hold" || d.ToRung != "L3" || len(d.Reasons) == 0 {
		t.Errorf("not enough evidence for the higher rung: %+v", d)
	}
	// Two failures inside a day: demotion is checked before promotion, even for a class that also qualifies.
	bad := append(run(40, nil, 20, 3), Event{TSDays: 20, IncidentID: "x"}, Event{TSDays: 20.4, IncidentID: "y"})
	if d, _ = Decide("scale", "L2->L3", "L3", 21, bad, false, false, false); d.Action != "demote" || d.ToRung != "L2" || !strings.Contains(d.Reasons[0], "CUSUM") {
		t.Errorf("%+v", d)
	}
	if d, _ = Decide("scale", "L2->L3", "L2", 20, good, false, false, true); d.Action != "hold" || !strings.Contains(d.Reasons[0], "re-qualification") {
		t.Errorf("drift holds and says why: %+v", d)
	}
	if d, _ = Decide("x", "L3->L4:irreversible", "L3", 1, nil, false, false, false); d.Action != "hold" {
		t.Errorf("%+v", d)
	}
	if _, err = Decide("x", "nope", "L1", 1, nil, false, false, false); err == nil {
		t.Error("unknown transition")
	}
}

func TestEveryRejectionInRecordAutonomousAttemptIsASampleNotInvented(t *testing.T) {
	o := outcomesDB(t)
	ctx := context.Background()
	cases := []struct {
		name     string
		trigger  string
		outcome  string
		verified *bool
		want     bool
	}{
		{"detector, verified, resolved", "detector", "resolved", b(true), true},
		{"a failed but graded fix still counts", "detector", "regression", b(false), true},
		{"a human asking is not the class earning a rung", "user_query", "resolved", b(true), false},
		{"could not look is unknown, not a sample", "detector", "resolved", nil, false},
		{"a report only run attempted nothing", "detector", "report_only", b(true), false},
		{"no outcome", "detector", "", b(true), false},
		{"grade is case insensitive", "detector", " Resolved ", b(true), true},
	}
	for _, c := range cases {
		if got := o.RecordAutonomousAttempt(ctx, "ep", c.trigger, c.outcome, c.verified, []string{"OOMKilled"}, 86400*10); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	ev, _ := o.Read(ctx, WatchtowerAutofix, 10)
	if len(ev) != 3 || ev[0].IncidentType != "OOMKilled" || ev[0].TSDays != 10 || ev[0].Critical {
		t.Errorf("%+v", ev)
	}
	o.RecordAutonomousAttempt(ctx, "ep", "detector", "resolved", b(true), nil, 0)
	if last, _ := o.Read(ctx, WatchtowerAutofix, 10); last[len(last)-1].IncidentType != "generic" && last[0].IncidentType != "generic" {
		t.Errorf("no playbook reads as generic: %+v", last)
	}
}

func TestReadIsChronologicalAndLimited(t *testing.T) {
	o := outcomesDB(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_ = o.Record(ctx, "c", Event{TSDays: float64(i), Success: i%2 == 0, IncidentID: "i"})
	}
	_ = o.Record(ctx, "other", Event{TSDays: 99, IncidentID: "z"})
	ev, err := o.Read(ctx, "c", 3)
	if err != nil || len(ev) != 3 || ev[0].TSDays != 2 || ev[2].TSDays != 4 {
		t.Errorf("the most recent three, oldest first: %+v %v", ev, err)
	}
}

func TestAutofixRevocationIsRevokeOnly(t *testing.T) {
	o := outcomesDB(t)
	ctx := context.Background()
	if r, err := o.AutofixRevocation(ctx, 30); r != "" || err != nil {
		t.Errorf("no samples, no revocation: %q %v", r, err)
	}
	// A long clean record is never a grant.
	for i := 0; i < 60; i++ {
		_ = o.Record(ctx, WatchtowerAutofix, Event{TSDays: float64(i) / 2, Success: true, IncidentID: "i", IncidentType: "t"})
	}
	if r, _ := o.AutofixRevocation(ctx, 30); r != "" {
		t.Errorf("%q", r)
	}
	st, err := o.AutofixStatus(ctx, 30)
	if err != nil || st["direction"] != "revoke-only" || st["authority_revoked"] != false || st["samples"] != 60 || !strings.Contains(st["reason"].(string), "no demotion trigger over 60") {
		t.Fatalf("%v %v", st, err)
	}
	// Two failures within a day take the authority away.
	_ = o.Record(ctx, WatchtowerAutofix, Event{TSDays: 30, IncidentID: "f1"})
	_ = o.Record(ctx, WatchtowerAutofix, Event{TSDays: 30.3, IncidentID: "f2"})
	r, _ := o.AutofixRevocation(ctx, 31)
	if !strings.HasPrefix(r, "watchtower-autofix demoted L4→L3: CUSUM") {
		t.Errorf("%q", r)
	}
	st, _ = o.AutofixStatus(ctx, 31)
	if st["authority_revoked"] != true || !strings.Contains(st["reason"].(string), "CUSUM") {
		t.Errorf("%v", st)
	}
}

func TestAnUnreadableStoreIsAnErrorNotACleanRecord(t *testing.T) {
	o := outcomesDB(t)
	_, _ = o.DB.Exec(`DROP TABLE promotion_outcomes`)
	if _, err := o.AutofixRevocation(context.Background(), 1); err == nil {
		t.Error("the caller must be able to tell 'no evidence' from 'could not read the evidence'")
	}
	if _, err := o.AutofixStatus(context.Background(), 1); err == nil {
		t.Error("status too")
	}
	un := AutofixStatusUnavailable("flag off")
	if un["enabled"] != false || un["operating"] != false || AutofixStatusUnavailable("no outcome store")["enabled"] != true {
		t.Errorf("%v", un)
	}
}

func TestExportEventsIsDeterministicAndKeepsOnlyWhatTheMathNeeds(t *testing.T) {
	m1 := spans.Mutation("ep1", 3, "scale", []string{"h"}, []string{"e"}, "")
	m2 := spans.Mutation("ep1", 7, "scale", []string{"h"}, []string{"e"}, "")
	m3 := spans.Mutation("ep2", 1, "delete pod", nil, nil, "")
	rows := []Row{
		{Kind: spans.Kind, EpisodeID: "ep2", Seq: 1, Payload: m3},
		{Kind: PostconditionKind, EpisodeID: "ep1", Seq: 4, Payload: map[string]any{"mutation_span_id": m1["span_id"], "held": true, "incident_type": "oom"}},
		{Kind: spans.Kind, EpisodeID: "ep1", Seq: 7, Payload: m2},
		{Kind: spans.Kind, EpisodeID: "ep1", Seq: 3, Payload: m1},
		{Kind: PostconditionKind, EpisodeID: "ep1", Seq: 8, Payload: map[string]any{"mutation_span_id": m2["span_id"], "held": false, "critical": true}},
		{Kind: "tool_call", EpisodeID: "ep1", Seq: 1, Payload: map[string]any{"command": "secret"}},
	}
	got := ExportEvents(rows, 100)
	sc := got["scale"]
	if len(sc) != 2 || !sc[0].Success || sc[0].IncidentType != "oom" || sc[1].Success || !sc[1].Critical || sc[0].IncidentID != "ep1" {
		t.Fatalf("%+v", got)
	}
	if sc[0].TSDays != 100 || sc[1].TSDays != 101 {
		t.Errorf("mutations are ordered by (episode, seq) and dated by position: %v %v", sc[0].TSDays, sc[1].TSDays)
	}
	if d := got["delete pod"]; len(d) != 1 || d[0].Success {
		t.Errorf("a mutation with no recorded postcondition did not hold: %+v", d)
	}
	if len(ExportEvents(nil, 0)) != 0 {
		t.Error("empty")
	}
	red := RedactSpanRow(Row{Kind: spans.Kind, EpisodeID: "ep1", Seq: 3, Payload: m1})
	attrs := red["payload"].(map[string]any)["attributes"].(map[string]any)
	if _, leaked := attrs["ki.links.hypothesis"]; leaked || attrs["ki.action"] != "scale" || attrs["ki.provenance_incomplete"] != false {
		t.Errorf("%v", attrs)
	}
}

func TestSpendIsSummedFromRecordedChatSpans(t *testing.T) {
	db := storetest.New(t, PromotionMigrations, rec.Migrations)
	r := rec.New(db, true, true)
	r.Start(context.Background())
	r.Record("ep", spans.Kind, spans.Chat("ep", 1, "openai", "m", 1000, 500, ""))
	r.Record("ep", spans.Kind, spans.Chat("ep", 2, "openai", "m", 3000, 500, ""))
	r.Record("ep", spans.Kind, spans.Tool("ep", 3, "kubectl", true, "", false))
	r.Record("ep", "tool_call", map[string]any{"x": 1})
	r.Record("other", spans.Kind, spans.Chat("other", 1, "openai", "m", 99999, 99999, ""))
	r.Close()

	got, err := EpisodeSpendUSD(context.Background(), db, "ep", 0.0025, 0.01)
	want := USDFromTokens(4000, 1000, 0.0025, 0.01)
	if err != nil || got != want || want != 0.02 {
		t.Errorf("%v %v %v", got, want, err)
	}
	if got, _ = EpisodeSpendUSD(context.Background(), db, "none", 1, 1); got != 0 {
		t.Errorf("no spans, no spend: %v", got)
	}
}
