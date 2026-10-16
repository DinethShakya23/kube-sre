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
	// Notes are features switched off on purpose: worth saying, but not a gap in what was recorded.
	Notes   []string `json:"notes"`
	Summary string   `json:"summary"`
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
		RollbackPoints: []RollbackPoint{}, DegradedReasons: []string{}, Notes: []string{}}

	if !b.Cfg.FlightRecorder {
		d.DegradedReasons = append(d.DegradedReasons, "the flight recorder is turned off (FLIGHT_RECORDER_ENABLED=false), "+
			"so no findings, investigations or undo points were recorded")
	}
	// Off by choice: findings are still complete, there is just nothing autonomous to report.
	if !b.Cfg.Watchtower {
		d.Notes = append(d.Notes, "Automatic investigations are turned off on this server (WATCHTOWER_ENABLED=false), so nothing was looked into on its own.")
	}
	if b.Perception != nil {
		d.DegradedReasons = append(d.DegradedReasons, perception.Gaps(b.Perception())...)
	}

	rows, err := b.DB.QueryContext(ctx, b.DB.Q(`SELECT episode_id, kind, payload, created_at FROM decision_log WHERE created_at >= ? ORDER BY created_at, id`),
		b.timeArg(cutoff))
	sessions := map[string]bool{}
	if err != nil {
		slog.Warn("digest decision_log query failed", "err", err)
		d.DegradedReasons = append(d.DegradedReasons, fmt.Sprintf("the decision_log query failed (%v), so findings, undo points and chat counts are unknown", trim(err)))
	} else {
		for rows.Next() {
			var episode, kind string
			var raw []byte
			var at time.Time
			if err := rows.Scan(&episode, &kind, &raw, &at); err != nil {
				slog.Warn("digest decision_log scan failed", "err", err)
				d.DegradedReasons = append(d.DegradedReasons, "the decision_log rows could not be read, so findings, undo points and chat counts are unknown")
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
		d.DegradedReasons = append(d.DegradedReasons, fmt.Sprintf("the episodes query failed (%v), so automatic investigations are unknown", trim(err)))
	} else {
		for eps.Next() {
			var kind, summary string
			var pb []byte
			var verified *bool
			var started float64
			var detailS, outcomeS, nsS *string
			if err := eps.Scan(&kind, &detailS, &summary, &outcomeS, &verified, &nsS, &pb, &started); err != nil {
				d.DegradedReasons = append(d.DegradedReasons, "the episode rows could not be read, so automatic investigations are unknown")
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
	window := windowText(d.WindowHours)
	if len(d.DegradedReasons) > 0 {
		// Never "quiet": an empty digest here is an absence of records, not of events,
		// and the two look identical from the outside.
		seen := "nothing could be read"
		if findings > 0 || autos > 0 {
			seen = "what could be read: " + plural(findings, "finding", "findings") + " and " + plural(autos, "investigation", "investigations")
		}
		more := ""
		if n := len(d.DegradedReasons) - 1; n > 0 {
			more = ", and " + plural(n, "other problem", "other problems") + " listed below"
		}
		return fmt.Sprintf("This digest has gaps for %s: %s%s. Here's %s.", window, d.DegradedReasons[0], more, seen)
	}
	if findings == 0 && autos == 0 {
		return fmt.Sprintf("All quiet: nothing fired in %s.", window)
	}
	var parts []string
	if findings > 0 {
		parts = append(parts, plural(findings, "finding", "findings")+" across "+plural(len(problems(d.Findings)), "problem", "problems"))
	}
	if autos > 0 {
		a := plural(autos, "automatic investigation", "automatic investigations")
		if fixes > 0 {
			a += " (" + plural(fixes, "fix", "fixes") + " verified)"
		}
		parts = append(parts, a)
	}
	return "In " + window + ": " + strings.Join(parts, ", and ") + "."
}

func windowText(hours float64) string {
	switch {
	case hours == 24:
		return "the last day"
	case hours == 168:
		return "the last week"
	case hours >= 48 && int(hours)%24 == 0:
		return fmt.Sprintf("the last %d days", int(hours)/24)
	case hours == 1:
		return "the last hour"
	}
	return fmt.Sprintf("the last %.0f hours", hours)
}

// plural writes "1 finding" or "842 findings".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func tail[T any](xs []T, n int) []T {
	if len(xs) > n {
		return xs[len(xs)-n:]
	}
	return xs
}

// atText says when, relative to now: "at 09:14", "yesterday at 22:40" or "on Mon 14 Oct at 08:00".
func atText(at, now float64) string {
	t, n := time.Unix(int64(at), 0), time.Unix(int64(now), 0)
	day := func(x time.Time) time.Time { y, m, d := x.Date(); return time.Date(y, m, d, 0, 0, 0, 0, x.Location()) }
	switch day(n).Sub(day(t)) / (24 * time.Hour) {
	case 0:
		return "at " + t.Format("15:04")
	case 1:
		return "yesterday at " + t.Format("15:04")
	}
	return "on " + t.Format("Mon 2 Jan") + " at " + t.Format("15:04")
}

// friendly says what a detector saw in everyday words; unknown ones keep their name.
var friendly = map[string]string{
	"CommandHardcodedFailure":      "exits on start",
	"ContainerCreatingStuck":       "stuck creating its container",
	"CrashLoopBackOff":             "crash looping",
	"CreateContainerConfigError":   "has a broken container config",
	"DnsResolutionFailure":         "failing DNS lookups",
	"Evicted":                      "evicted",
	"HPANotScaling":                "autoscaler not scaling",
	"ImagePullBackOff":             "can't pull its image",
	"InitContainerFailing":         "init container failing",
	"JobBackoffLimitExceeded":      "job gave up after too many retries",
	"LivenessProbeFailing":         "failing its liveness probe",
	"NetworkPolicyBlocking":        "blocked by a network policy",
	"NodeNotReady":                 "node not ready",
	"OOMKilled":                    "ran out of memory",
	"PodDisruptionBudgetBlocking":  "blocked by a disruption budget",
	"PendingInsufficientResources": "pending, not enough resources",
	"PendingSchedulingConstraints": "pending, can't be scheduled",
	"PvcPending":                   "waiting for its volume",
	"QuotaExceeded":                "over its quota",
	"ReadinessProbeFailing":        "failing its readiness probe",
	"DeploymentRolloutStuck":       "rollout stuck",
	"ServiceNoEndpoints":           "service has no endpoints",
	"ServiceUnreachable":           "service unreachable",
	"StatefulSetRolloutStuck":      "rollout stuck",
	"TerminatingStuck":             "stuck terminating",
	"TlsCertificateExpired":        "TLS certificate expired",
	"WebhookAdmissionRejected":     "rejected by an admission webhook",
}

func describe(playbook string) string {
	if f, ok := friendly[playbook]; ok {
		return f + " (" + playbook + ")"
	}
	return playbook
}

// problem is every firing on one object, so repeats read as one line with a count.
type problem struct {
	where string
	last  float64
	kinds []kind
}

type kind struct {
	playbook string
	count    int
	eta      *float64
	predict  bool
}

func problems(fs []Finding) []problem {
	byObj := map[string]*problem{}
	var order []string
	for _, f := range fs {
		where := f.Object
		if f.Namespace != "" {
			where = f.Namespace + "/" + f.Object
		}
		p, ok := byObj[where]
		if !ok {
			p = &problem{where: where}
			byObj[where] = p
			order = append(order, where)
		}
		p.last = max(p.last, f.At)
		i := -1
		for j, k := range p.kinds {
			if k.playbook == f.Playbook {
				i = j
			}
		}
		if i < 0 {
			p.kinds = append(p.kinds, kind{playbook: f.Playbook})
			i = len(p.kinds) - 1
		}
		p.kinds[i].count++
		if f.Severity == "predicted" {
			p.kinds[i].predict, p.kinds[i].eta = true, f.ETAMinutes
		}
	}
	out := make([]problem, 0, len(order))
	for _, w := range order {
		out = append(out, *byObj[w])
	}
	sortBy(out, func(a, b problem) bool { return a.last > b.last })
	return out
}

func sortBy[T any](xs []T, less func(a, b T) bool) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && less(xs[j], xs[j-1]); j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

func (k kind) text() string {
	s := describe(k.playbook)
	if k.predict {
		if k.eta != nil {
			s = fmt.Sprintf("likely %s in about %.0f minutes", s, *k.eta)
		} else {
			s = "likely " + s + " soon"
		}
	}
	if k.count > 1 {
		s += fmt.Sprintf(", %d times", k.count)
	}
	return s
}

const maxProblems = 15

// Markdown renders the digest.
func (d *Digest) Markdown() string {
	l := []string{"# What happened in " + windowText(d.WindowHours), "", d.Summary}
	if len(d.DegradedReasons) > 0 {
		l = append(l, "", "> **Heads up: parts of this window couldn't be read, so don't take it as an all clear.**")
		for _, r := range d.DegradedReasons {
			l = append(l, "> - "+upperFirst(r)+".")
		}
	}
	for _, n := range d.Notes {
		l = append(l, "", "_"+n+"_")
	}
	if ps := problems(d.Findings); len(ps) > 0 {
		l = append(l, "", "## What my detectors spotted", "", "Newest first. These checks run without the model, so they cost nothing.", "")
		for _, p := range ps[:min(len(ps), maxProblems)] {
			kinds := make([]string, len(p.kinds))
			for i, k := range p.kinds {
				kinds[i] = k.text()
			}
			l = append(l, fmt.Sprintf("- **%s**: %s. Last seen %s.", p.where, strings.Join(kinds, "; "), atText(p.last, d.GeneratedAt)))
		}
		if len(ps) > maxProblems {
			l = append(l, "", fmt.Sprintf("_And %s, on the Findings page._", plural(len(ps)-maxProblems, "more", "more")))
		}
	}
	if len(d.AutoInvestigation) > 0 {
		l = append(l, "", "## What I looked into on my own", "")
		for _, a := range reverse(tail(d.AutoInvestigation, 10)) {
			status := map[string]string{"resolved": "fixed", "": "reported back"}[a.Outcome]
			if status == "" {
				status = a.Outcome
			}
			if a.Verified != nil && *a.Verified {
				status += " and verified"
			}
			where := ""
			if a.Namespace != "" {
				where = " in " + a.Namespace
			}
			l = append(l, fmt.Sprintf("- %s%s, %s: %s", upperFirst(atText(a.At, d.GeneratedAt)), where, status, clip(a.Summary, 160)))
		}
	}
	if len(d.RollbackPoints) > 0 {
		armed := 0
		for _, r := range d.RollbackPoints {
			if r.Restorable != nil && *r.Restorable {
				armed++
			}
		}
		l = append(l, "", "## Undo points", "", fmt.Sprintf("Before each change I saved what was there. %d of %d can be put back.", armed, len(d.RollbackPoints)), "")
		for _, r := range reverse(tail(d.RollbackPoints, 10)) {
			var mark string
			switch {
			case r.Restorable == nil:
				mark = "can't tell if this one can be restored (it was saved before that was tracked)"
			case *r.Restorable:
				mark = "can be restored"
			default:
				notes := strings.Join(r.CaptureNotes, "; ")
				if notes == "" {
					notes = "redacted or truncated"
				}
				mark = "can't be restored safely, so don't apply it (" + notes + ")"
			}
			l = append(l, fmt.Sprintf("- %s, `%s`: %s. The change was `%s`.", upperFirst(atText(r.At, d.GeneratedAt)), r.RollbackID, mark, clip(r.Command, 80)))
		}
	}
	if d.UserSessions > 0 {
		l = append(l, "", "_"+plural(d.UserSessions, "chat", "chats")+" with me in this window._")
	}
	return strings.Join(l, "\n")
}

func reverse[T any](xs []T) []T {
	out := make([]T, len(xs))
	for i, x := range xs {
		out[len(xs)-1-i] = x
	}
	return out
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
