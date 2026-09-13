package autonomy

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/DinethShakya23/kube-sre/internal/spans"
	"github.com/DinethShakya23/kube-sre/internal/store"
)

// The statistical promotion engine, the decision layer over the pure statistics:
// given an action class's outcomes, decide whether it earns the next rung, holds, or
// is automatically demoted. Precedence follows the asymmetry, fast down and slow up:
// demotion is checked first.
//
// What ships is the down half only. The one production caller can revoke the
// watchtower's A3 authority, never grant it. That is not a shortcut: every sample the
// store holds comes from a write the allowlist had already permitted, so promoting on
// them would be circular, and gating the write on an earned rung would deadlock a
// class with no samples out of ever producing one. Rungs are earned from shadow
// agreement (propose, a human executes, record whether the proposal matched), which
// this system does not run. Autonomy here is configured (ladder and allowlist) and can
// be taken away statistically; it is not granted statistically.

// EngineDecision is promote, hold or demote for one action class.
type EngineDecision struct {
	Action      string // promote | hold | demote
	ActionClass string
	Transition  string
	ToRung      string
	LCB         float64
	N           int
	Reasons     []string
}

// Decide decides promote, hold or demote for actionClass on transition.
func Decide(actionClass, transition, currentRung string, nowDays float64, events []Event, sevAttributed, m4AtL4, classDrift bool) (EngineDecision, error) {
	rule, err := RuleFor(transition)
	if err != nil {
		return EngineDecision{}, err
	}
	theta := 1.0
	if rule.Theta != nil {
		theta = *rule.Theta
	}
	dem := EvaluateDemotion(currentRung, theta, events, nowDays, sevAttributed, m4AtL4, classDrift)
	switch {
	case dem.Demote:
		return EngineDecision{"demote", actionClass, transition, dem.To, 0, 0, []string{dem.Reason}}, nil
	case dem.Stale:
		// Class definition drift: hold at the current rung, flagged for requalification.
		return EngineDecision{"hold", actionClass, transition, currentRung, 0, 0, []string{dem.Reason}}, nil
	}
	pr, err := EvaluatePromotion(transition, events, nowDays)
	if err != nil {
		return EngineDecision{}, err
	}
	if pr.Promote {
		return EngineDecision{"promote", actionClass, transition, toRung(transition), pr.LCB, pr.N, nil}, nil
	}
	return EngineDecision{"hold", actionClass, transition, currentRung, pr.LCB, pr.N, pr.Reasons}, nil
}

func toRung(transition string) string {
	_, after, _ := strings.Cut(transition, "->")
	rung, _, _ := strings.Cut(after, ":")
	return rung
}

// ── the outcome store ────────────────────────────────────────────────────────

var PromotionMigrations = []store.Migration{{
	Version: 47, Name: "promotion outcomes",
	SQL: `
CREATE TABLE IF NOT EXISTS promotion_outcomes (
    id            {{PK}},
    action_class  TEXT NOT NULL,
    ts_days       DOUBLE PRECISION NOT NULL,
    success       BOOLEAN NOT NULL,
    incident_id   TEXT NOT NULL,
    incident_type TEXT NOT NULL DEFAULT 'generic',
    critical      BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    DOUBLE PRECISION NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_promotion_outcomes_class ON promotion_outcomes (action_class, ts_days);`,
}}

// Outcomes is the durable source of per class outcomes. These are statistical
// samples, not an audit ledger: purpose built and not hash chained.
type Outcomes struct {
	DB  *store.DB
	Now func() float64
}

// Record persists one outcome for an action class.
func (o *Outcomes) Record(ctx context.Context, actionClass string, e Event) error {
	now := 0.0
	if o.Now != nil {
		now = o.Now()
	}
	_, err := o.DB.ExecContext(ctx, o.DB.Q(`INSERT INTO promotion_outcomes (action_class, ts_days, success, incident_id, incident_type, critical, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`), actionClass, e.TSDays, e.Success, e.IncidentID, e.kind(), e.Critical, now)
	return err
}

// Read returns a class's outcomes in chronological order, the most recent limit of them.
func (o *Outcomes) Read(ctx context.Context, actionClass string, limit int) ([]Event, error) {
	rows, err := o.DB.QueryContext(ctx, o.DB.Q(`SELECT ts_days, success, incident_id, incident_type, critical FROM promotion_outcomes
		WHERE action_class = ? ORDER BY ts_days DESC, id DESC LIMIT ?`), actionClass, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ev []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.TSDays, &e.Success, &e.IncidentID, &e.IncidentType, &e.Critical); err != nil {
			return nil, err
		}
		ev = append(ev, e)
	}
	// Chronological for the windowing math.
	for i, j := 0, len(ev)-1; i < j; i, j = i+1, j-1 {
		ev[i], ev[j] = ev[j], ev[i]
	}
	return ev, rows.Err()
}

// DecideFromStore reads a class's recorded outcomes and runs the pure decision.
func (o *Outcomes) DecideFromStore(ctx context.Context, actionClass, transition, currentRung string, nowDays float64) (EngineDecision, error) {
	ev, err := o.Read(ctx, actionClass, 500)
	if err != nil {
		return EngineDecision{}, err
	}
	return Decide(actionClass, transition, currentRung, nowDays, ev, false, false, false)
}

const (
	WatchtowerAutofix  = "watchtower-autofix"
	AutofixTransition  = "L3->L4:declarative-revert"
	AutofixGrantedRung = "L4"
	secondsPerDay      = 86400.0
)

var gradedOutcomes = map[string]bool{"resolved": true, "partial": true, "regression": true}

// RecordAutonomousAttempt records one autonomously attempted, cluster verified fix and
// reports whether a row was written. Without a writer the store held nothing, so every
// class answered hold with the honest reason "n 0 < n_min" and sat at its configured
// rung for ever; and since demotion is fast down, a class whose agreement had collapsed
// could never be demoted either. Each rejection is a sample this store must not invent:
//
//   - the trigger is a detector: only the watchtower's own investigations are attempts
//     by this class, and a human asking for a fix is not the class earning a rung;
//   - verified is known: "we could not look" reads as unknown, and treating it as an
//     answer would record an unverified fix as verified. A read is most likely to fail
//     right after a disruptive change, which is exactly when this runs, so it stays out
//     of both the numerator and the denominator;
//   - the outcome is graded: a report only run attempted nothing.
//
// Critical is always false: nothing on this path attributes a Sev-1 to an action, so
// the critical event trigger is not fed from here. That is a gap and not a claim of safety.
func (o *Outcomes) RecordAutonomousAttempt(ctx context.Context, episodeID, triggerKind, outcome string, verified *bool, playbooks []string, atSeconds float64) bool {
	if triggerKind != "detector" || verified == nil || !gradedOutcomes[strings.ToLower(strings.TrimSpace(outcome))] {
		return false
	}
	kind := "generic"
	if len(playbooks) > 0 && playbooks[0] != "" {
		kind = playbooks[0]
	}
	e := Event{TSDays: atSeconds / secondsPerDay, Success: *verified, IncidentID: episodeID, IncidentType: kind}
	if err := o.Record(ctx, WatchtowerAutofix, e); err != nil {
		slog.Warn("promotion outcome not recorded", "err", err)
		return false
	}
	return true
}

// AutofixRevocation reports whether the recorded record has revoked the watchtower's
// unattended write authority: the demotion reason, or "" if the class still holds it.
// Revocation only, and the asymmetry is the only direction these samples honestly
// support (see the top of this file). The two triggers that fire are CUSUM, two
// postcondition failures within 24 hours at any sample size, and the hysteresis band,
// only where a breach is attributable to failures and not to sample size.
func (o *Outcomes) AutofixRevocation(ctx context.Context, nowDays float64) (string, error) {
	d, err := o.DecideFromStore(ctx, WatchtowerAutofix, AutofixTransition, AutofixGrantedRung, nowDays)
	if err != nil {
		return "", err
	}
	if d.Action != "demote" {
		return "", nil
	}
	reason := strings.Join(d.Reasons, "; ")
	if reason == "" {
		reason = "demotion trigger fired"
	}
	return fmt.Sprintf("%s demoted %s→%s: %s", WatchtowerAutofix, AutofixGrantedRung, d.ToRung, reason), nil
}

// AutofixStatus is what an operator needs to see about the A3 statistical brake in
// one read: is it on, can it operate, and what does the record currently say. samples
// is the count inside the rolling window, the n every threshold is measured against.
// "Active" beside a flag named statistical promotion reads as "rungs are being earned
// here", the one thing this build does not do, so the direction is stated.
func (o *Outcomes) AutofixStatus(ctx context.Context, nowDays float64) (map[string]any, error) {
	ev, err := o.Read(ctx, WatchtowerAutofix, 500)
	if err != nil {
		return nil, err
	}
	var win []Event
	for _, e := range ev {
		if nowDays-e.TSDays <= WindowMaxDays {
			win = append(win, e)
		}
	}
	if len(win) > WindowMaxEvents {
		win = win[len(win)-WindowMaxEvents:]
	}
	d, err := Decide(WatchtowerAutofix, AutofixTransition, AutofixGrantedRung, nowDays, ev, false, false, false)
	if err != nil {
		return nil, err
	}
	revoked := d.Action == "demote"
	reason := fmt.Sprintf("no demotion trigger over %d sample(s) in the window", len(win))
	if revoked {
		reason = strings.Join(d.Reasons, "; ")
	}
	return map[string]any{"enabled": true, "direction": "revoke-only", "operating": true, "action_class": WatchtowerAutofix,
		"samples": len(win), "authority_revoked": revoked, "reason": reason}, nil
}

// AutofixStatusUnavailable is the same block when the brake cannot act: the flag is
// off, or there is no outcome store to read. operating is the field to key on and is
// separate from enabled on purpose: a deployment that set the flag without a store has
// it enabled and not operating.
func AutofixStatusUnavailable(reason string) map[string]any {
	return map[string]any{"enabled": reason != "flag off", "direction": "revoke-only", "operating": false, "action_class": WatchtowerAutofix,
		"samples": 0, "authority_revoked": false, "reason": reason}
}

// ── replay fixtures ──────────────────────────────────────────────────────────

// PostconditionKind is the recorder row kind of a mutation's postcondition outcome.
const PostconditionKind = "ki_postcondition"

// Row is a decoded decision_log row.
type Row struct {
	Kind, EpisodeID string
	Seq             int
	Payload         map[string]any
	CreatedDay      *float64
}

// ExportEvents turns flight recorder rows (mutation spans plus their postcondition
// outcomes) into a deterministic per action class list of events the statistics replay
// offline. Timestamps are normalised to day offsets from day0 (no wall clock), and
// only the fields the math needs are kept: action, success, incident id and type,
// critical. No sensitive telemetry leaves the recorder.
func ExportEvents(rows []Row, day0 float64) map[string][]Event {
	outcomes := map[string]map[string]any{}
	var muts []Row
	for _, r := range rows {
		switch {
		case r.Kind == PostconditionKind:
			if id, _ := r.Payload["mutation_span_id"].(string); id != "" {
				outcomes[id] = r.Payload
			}
		case r.Kind == spans.Kind && r.Payload["gen_ai.operation.name"] == spans.OpMutation:
			muts = append(muts, r)
		}
	}
	sort.SliceStable(muts, func(i, j int) bool {
		if muts[i].EpisodeID != muts[j].EpisodeID {
			return muts[i].EpisodeID < muts[j].EpisodeID
		}
		return muts[i].Seq < muts[j].Seq
	})
	out := map[string][]Event{}
	for i, r := range muts {
		attrs, _ := r.Payload["attributes"].(map[string]any)
		action, _ := attrs["ki.action"].(string)
		if action == "" {
			action = "unknown"
		}
		spanID, _ := r.Payload["span_id"].(string)
		if spanID == "" {
			spanID = fmt.Sprintf("%s:%d", r.EpisodeID, r.Seq)
		}
		oc := outcomes[spanID]
		held, _ := oc["held"].(bool)
		crit, _ := oc["critical"].(bool)
		kind, _ := oc["incident_type"].(string)
		day := float64(i)
		if r.CreatedDay != nil {
			day = *r.CreatedDay
		}
		id := r.EpisodeID
		if id == "" {
			id = fmt.Sprintf("ep-%d", i)
		}
		out[action] = append(out[action], Event{TSDays: day + day0, Success: held, IncidentID: id, IncidentType: kind, Critical: crit})
	}
	return out
}

// RedactSpanRow is an export safe copy of a span row: identity and the promotion
// relevant attributes only, dropping any free text or telemetry.
func RedactSpanRow(r Row) map[string]any {
	attrs, _ := r.Payload["attributes"].(map[string]any)
	keep := map[string]any{}
	for _, k := range []string{"ki.action", "ki.provenance_incomplete"} {
		if v, ok := attrs[k]; ok {
			keep[k] = v
		}
	}
	return map[string]any{"kind": r.Kind, "episode_id": r.EpisodeID, "seq": r.Seq, "payload": map[string]any{
		"span_id": r.Payload["span_id"], "gen_ai.operation.name": r.Payload["gen_ai.operation.name"], "attributes": keep}}
}

// ── spend ────────────────────────────────────────────────────────────────────

// USDFromTokens prices tokens.
func USDFromTokens(in, out, inPer1K, outPer1K float64) float64 {
	return in/1000*inPer1K + out/1000*outPer1K
}

// EpisodeSpendUSD sums an episode's recorded chat span token usage and prices it, 0 if
// it has no spans. The spans are the single spend source, so the budget gate can deny
// an action whose projected cost would breach the cap from real recorded data.
func EpisodeSpendUSD(ctx context.Context, db *store.DB, episode string, inPer1K, outPer1K float64) (float64, error) {
	rows, err := db.QueryContext(ctx, db.Q(`SELECT payload FROM decision_log WHERE episode_id = ? AND kind = ?`), episode, spans.Kind)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var in, out float64
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return 0, err
		}
		p := decodeMap(raw)
		attrs, _ := p["attributes"].(map[string]any)
		i, iok := number(attrs["gen_ai.usage.input_tokens"])
		if !iok {
			continue
		}
		o, _ := number(attrs["gen_ai.usage.output_tokens"])
		in, out = in+i, out+o
	}
	return USDFromTokens(in, out, inPer1K, outPer1K), rows.Err()
}
