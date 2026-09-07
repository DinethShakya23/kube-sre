// Package digest builds the morning digest and the incident postmortem. Both are
// views over durable state (the flight recorder and the episode store), never a
// separate history, and neither uses a model unless a narrative is asked for.
package digest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/perception"
	"github.com/DinethShakya23/kube-sre/internal/store"
)

// Builder reads the sources both reports need.
type Builder struct {
	DB  *store.DB
	Cfg *config.Config
	// Perception reports what the sensing side was doing; nil when it is not part of this process.
	Perception func() perception.State
	Now        func() time.Time
}

func (b *Builder) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// timeArg is a cutoff in the form the dialect compares correctly: SQLite keeps
// timestamps as text, so the argument must be text in the same layout.
func (b *Builder) timeArg(t time.Time) any {
	if b.DB.Dialect == store.SQLite {
		return t.UTC().Format("2006-01-02 15:04:05")
	}
	return t.UTC()
}

// Finding is one detector firing in the digest.
type Finding struct {
	At         float64  `json:"at"`
	Playbook   string   `json:"playbook"`
	Namespace  string   `json:"namespace"`
	Object     string   `json:"object"`
	Severity   string   `json:"severity"`
	ETAMinutes *float64 `json:"eta_minutes"`
}

// RollbackPoint is one pre-mutation capture in the digest.
type RollbackPoint struct {
	At           float64  `json:"at"`
	RollbackID   string   `json:"rollback_id"`
	Command      string   `json:"command"`
	Restorable   *bool    `json:"restorable"`
	CaptureNotes []string `json:"capture_notes"`
}

// AutoInvestigation is one autonomous investigation in the digest.
type AutoInvestigation struct {
	At        float64  `json:"at"`
	Namespace string   `json:"namespace"`
	Summary   string   `json:"summary"`
	Outcome   string   `json:"outcome"`
	Verified  *bool    `json:"verified"`
	Playbooks []string `json:"playbooks"`
}

// Digest is the structured report for a window.
type Digest struct {
	WindowHours       float64             `json:"window_hours"`
	GeneratedAt       float64             `json:"generated_at"`
	Findings          []Finding           `json:"findings"`
	AutoInvestigation []AutoInvestigation `json:"auto_investigations"`
	UserSessions      int                 `json:"user_sessions"`
	RollbackPoints    []RollbackPoint     `json:"rollback_points"`
	Degraded          bool                `json:"degraded"`
	DegradedReasons   []string            `json:"degraded_reasons"`
	Summary           string              `json:"summary"`
}

func decode(raw []byte) map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

func s(m map[string]any, k, def string) string {
	if v, ok := m[k].(string); ok && v != "" {
		return v
	}
	return def
}

func epoch(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// Build is the digest for the last hours.
//
// "Quiet" and "unrecorded" are different answers and must not share a sentence. The
// digest is the operator's morning check, so an empty result is only reassuring if
// the sources were readable. A genuinely quiet night, a failed decision_log query, a
// disabled recorder and a disabled watchtower all used to print the same confident
// line. And the record can be flawless and empty because nothing was ever looking,
// so the perception sources are asked too, through the same classifier
// GET /v1/findings uses, so the two cannot disagree about one window.
//
// DegradedReasons names every source that could not answer. It is empty exactly
// when the digest is a real observation of the window.
func (b *Builder) Build(ctx context.Context, hours float64) *Digest {
	now := b.now()
	cutoff := now.Add(-time.Duration(hours * float64(time.Hour)))
	d := &Digest{WindowHours: hours, GeneratedAt: epoch(now), Findings: []Finding{}, AutoInvestigation: []AutoInvestigation{},
		RollbackPoints: []RollbackPoint{}, DegradedReasons: []string{}}

	if !b.Cfg.FlightRecorder {
		d.DegradedReasons = append(d.DegradedReasons, "the flight recorder is disabled (FLIGHT_RECORDER_ENABLED=false) — "+
			"no findings, investigations or rollback points were recorded")
	}
	if !b.Cfg.Watchtower {
		d.DegradedReasons = append(d.DegradedReasons, "the watchtower is disabled (WATCHTOWER_ENABLED=false) — "+
			"no autonomous investigation could have run")
	}
	if b.Perception != nil {
		d.DegradedReasons = append(d.DegradedReasons, perception.Gaps(b.Perception())...)
	}

	rows, err := b.DB.QueryContext(ctx, b.DB.Q(`SELECT episode_id, kind, payload, created_at FROM decision_log WHERE created_at >= ? ORDER BY created_at, id`),
		b.timeArg(cutoff))
	sessions := map[string]bool{}
	if err != nil {
		slog.Warn("digest decision_log query failed", "err", err)
		d.DegradedReasons = append(d.DegradedReasons, fmt.Sprintf("the decision_log query failed (%v) — findings, rollback points and session counts are unknown", trim(err)))
	} else {
		for rows.Next() {
			var episode, kind string
			var raw []byte
			var at time.Time
			if err := rows.Scan(&episode, &kind, &raw, &at); err != nil {
				slog.Warn("digest decision_log scan failed", "err", err)
				d.DegradedReasons = append(d.DegradedReasons, "the decision_log rows could not be read — findings, rollback points and session counts are unknown")
				break
			}
			p := decode(raw)
			switch {
			case kind == "finding":
				f := Finding{At: epoch(at), Playbook: s(p, "playbook", "?"), Namespace: s(p, "namespace", ""), Object: s(p, "object", ""), Severity: s(p, "severity", "warning")}
				if v, ok := p["eta_minutes"].(float64); ok {
					f.ETAMinutes = &v
				}
				d.Findings = append(d.Findings, f)
			case kind == "rollback_point":
				r := RollbackPoint{At: epoch(at), RollbackID: s(p, "rollback_id", ""), Command: s(p, "command", ""), CaptureNotes: []string{}}
				// A capture whose YAML was redacted or truncated is evidence, not a restore
				// point. Records from before this field cannot be judged, so they read as
				// unknown and are never promoted to armed.
				if v, ok := p["restorable"].(bool); ok {
					r.Restorable = &v
				}
				if notes, ok := p["capture_notes"].([]any); ok {
					for _, n := range notes {
						r.CaptureNotes = append(r.CaptureNotes, fmt.Sprint(n))
					}
				}
				d.RollbackPoints = append(d.RollbackPoints, r)
			case strings.HasPrefix(episode, "auto-"), !strings.HasPrefix(episode, "findings:") && !strings.HasPrefix(episode, "shadow-findings:"):
				sessions[episode] = true
			}
		}
		rows.Close()
	}
	for e := range sessions {
		if !strings.HasPrefix(e, "auto-") {
			d.UserSessions++
		}
	}

	eps, err := b.DB.QueryContext(ctx, b.DB.Q(`SELECT trigger_kind, trigger_detail, summary, outcome, verified, namespace, playbooks, started_at
		FROM episodes WHERE started_at >= ? AND trigger_kind <> 'backfill' ORDER BY started_at`), epoch(cutoff))
	if err != nil {
		slog.Warn("digest episodes query failed", "err", err)
		d.DegradedReasons = append(d.DegradedReasons, fmt.Sprintf("the episodes query failed (%v) — autonomous investigations are unknown", trim(err)))
	} else {
		for eps.Next() {
			var kind, summary string
			var pb []byte
			var verified *bool
			var started float64
			var detailS, outcomeS, nsS *string
			if err := eps.Scan(&kind, &detailS, &summary, &outcomeS, &verified, &nsS, &pb, &started); err != nil {
				d.DegradedReasons = append(d.DegradedReasons, "the episode rows could not be read — autonomous investigations are unknown")
				break
			}
			if !strings.Contains(deref(detailS), "autonomous investigation") && kind != "detector" {
				continue
			}
			var playbooks []string
			_ = json.Unmarshal(pb, &playbooks)
			if playbooks == nil {
				playbooks = []string{}
			}
			d.AutoInvestigation = append(d.AutoInvestigation, AutoInvestigation{At: started, Namespace: deref(nsS), Summary: clip(summary, 300),
				Outcome: deref(outcomeS), Verified: verified, Playbooks: playbooks})
		}
		eps.Close()
	}

	d.Degraded = len(d.DegradedReasons) > 0
	d.Summary = oneLiner(d)
	return d
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func trim(err error) string { return clip(err.Error(), 120) }

func oneLiner(d *Digest) string {
	findings, autos := len(d.Findings), len(d.AutoInvestigation)
	fixes := 0
	for _, a := range d.AutoInvestigation {
		if a.Outcome == "resolved" {
			fixes++
		}
	}
	if len(d.DegradedReasons) > 0 {
		// Never "quiet": an empty digest here is an absence of records, not of events,
		// and the two look identical from the outside.
		seen := "nothing readable"
		if findings > 0 || autos > 0 {
			seen = fmt.Sprintf("%d finding(s), %d investigation(s) readable", findings, autos)
		}
		more := ""
		if len(d.DegradedReasons) > 1 {
			more = fmt.Sprintf(" (+%d more reason(s))", len(d.DegradedReasons)-1)
		}
		return fmt.Sprintf("Digest INCOMPLETE for the last %.0fh — %s. This is NOT a quiet watch: %s%s.", d.WindowHours, seen, d.DegradedReasons[0], more)
	}
	if findings == 0 && autos == 0 {
		return fmt.Sprintf("Quiet watch: no findings in the last %.0fh.", d.WindowHours)
	}
	fixText := ""
	if fixes > 0 {
		fixText = fmt.Sprintf(", %d verified fix(es)", fixes)
	}
	return fmt.Sprintf("%d finding(s), %d autonomous investigation(s)%s in the last %.0fh.", findings, autos, fixText, d.WindowHours)
}

func tail[T any](xs []T, n int) []T {
	if len(xs) > n {
		return xs[len(xs)-n:]
	}
	return xs
}

func hhmm(at float64) string { return time.Unix(int64(at), 0).Format("15:04") }

// Markdown renders the digest.
func (d *Digest) Markdown() string {
	l := []string{fmt.Sprintf("# kube-sre digest — last %.0fh", d.WindowHours), "", d.Summary}
	if len(d.DegradedReasons) > 0 {
		l = append(l, "", "> **⚠️ This digest is incomplete — do not read it as an all-clear.**")
		for _, r := range d.DegradedReasons {
			l = append(l, "> - "+r)
		}
	}
	if len(d.Findings) > 0 {
		l = append(l, "", "## Detector findings (zero-token)")
		for _, f := range tail(d.Findings, 20) {
			tag := ""
			if f.Severity == "predicted" {
				if f.ETAMinutes != nil {
					tag = fmt.Sprintf(" _(predicted ~%.0fm)_", *f.ETAMinutes)
				} else {
					tag = " _(predicted)_"
				}
			}
			l = append(l, fmt.Sprintf("- %s **%s** %s/%s%s", hhmm(f.At), f.Playbook, f.Namespace, f.Object, tag))
		}
	}
	if len(d.AutoInvestigation) > 0 {
		l = append(l, "", "## Autonomous investigations")
		for _, a := range tail(d.AutoInvestigation, 10) {
			status := a.Outcome
			if status == "" {
				status = "report"
			}
			l = append(l, fmt.Sprintf("- %s [%s] ns=%s: %s", hhmm(a.At), status, a.Namespace, clip(a.Summary, 160)))
		}
	}
	if len(d.RollbackPoints) > 0 {
		armed := 0
		for _, r := range d.RollbackPoints {
			if r.Restorable != nil && *r.Restorable {
				armed++
			}
		}
		l = append(l, "", fmt.Sprintf("## Pre-mutation state captures (%d of %d restorable)", armed, len(d.RollbackPoints)))
		for _, r := range tail(d.RollbackPoints, 10) {
			var mark string
			switch {
			case r.Restorable == nil:
				mark = "⚠️ restorability unknown (captured before this was recorded)"
			case *r.Restorable:
				mark = "restorable"
			default:
				notes := strings.Join(r.CaptureNotes, "; ")
				if notes == "" {
					notes = "redacted or truncated"
				}
				mark = "⚠️ NOT restorable — do not apply (" + notes + ")"
			}
			l = append(l, fmt.Sprintf("- %s `%s` [%s] — %s", hhmm(r.At), r.RollbackID, mark, clip(r.Command, 80)))
		}
	}
	if d.UserSessions > 0 {
		l = append(l, "", fmt.Sprintf("_%d user session(s) in the window._", d.UserSessions))
	}
	return strings.Join(l, "\n")
}
