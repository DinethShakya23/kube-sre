package digest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/llm"
	"github.com/DinethShakya23/kube-sre/internal/recorder"
)

// Narrator writes the optional prose over a postmortem's timeline.
type Narrator interface {
	Chat(ctx context.Context, msgs []llm.Message, tools []llm.ToolSpec, o llm.Options) (*llm.Response, error)
}

// TimelineEvent is one recorded event, citing its seq.
type TimelineEvent struct {
	Seq     int64   `json:"seq"`
	At      float64 `json:"at"`
	Kind    string  `json:"kind"`
	Summary string  `json:"summary"`
}

// Fired is a detector firing in an episode.
type Fired struct {
	Seq       int64  `json:"seq"`
	Playbook  string `json:"playbook"`
	Namespace string `json:"namespace"`
	Object    string `json:"object"`
	Severity  string `json:"severity"`
}

// Gap is a stretch of the record the recorder could not write.
type Gap struct {
	Seq     int64  `json:"seq"`
	Dropped int    `json:"dropped"`
	Reason  string `json:"reason"`
}

// Postmortem is a grounded narrative of one episode, built from the hash chained
// decision log. The structured timeline is the source of truth and cites every
// event's seq; the optional narrative only prettifies prose over it.
type Postmortem struct {
	EpisodeID   string  `json:"episode_id"`
	GeneratedAt float64 `json:"generated_at"`
	ChainValid  bool    `json:"chain_valid"`
	// ChainVerified says whether the chain was checked at all. chain_valid false on
	// its own cannot tell "the hashes disagree" from "there was nothing to hash".
	ChainVerified bool            `json:"chain_verified"`
	Timeline      []TimelineEvent `json:"timeline"`
	WhatFired     []Fired         `json:"what_fired"`
	Investigated  []string        `json:"investigated"`
	Tried         []string        `json:"tried"`
	Worked        []string        `json:"worked"`
	Errors        []string        `json:"errors"`
	RootCause     *string         `json:"root_cause"`
	FollowUps     []string        `json:"follow_ups"`
	Narrative     *string         `json:"narrative"`
	// EventsLost: a verified chain says nothing was altered, not that nothing is
	// missing. The recorder is fire and forget and records its own losses as gaps.
	EventsLost       int      `json:"events_lost"`
	Gaps             []Gap    `json:"gaps"`
	RecorderUp       bool     `json:"recorder_available"`
	EnrichmentFailed []string `json:"enrichment_failed"`
	Summary          string   `json:"summary"`
}

// PostmortemBuilder builds postmortems.
type PostmortemBuilder struct {
	Builder
	Recorder *recorder.Recorder
	// Narrator is used only when POSTMORTEM_LLM_NARRATIVE is on.
	Narrator Narrator
}

func mustString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			if t := strings.TrimSpace(fmt.Sprint(v)); t != "" {
				return t
			}
		}
	}
	return ""
}

// Build reconstructs the postmortem for one episode. It never fails: a recorder
// outage comes back as a postmortem that says why it is empty.
func (b *PostmortemBuilder) Build(ctx context.Context, episode string) *Postmortem {
	pm := &Postmortem{EpisodeID: episode, GeneratedAt: epoch(b.now()), Timeline: []TimelineEvent{}, WhatFired: []Fired{},
		Investigated: []string{}, Tried: []string{}, Worked: []string{}, Errors: []string{}, FollowUps: []string{},
		Gaps: []Gap{}, RecorderUp: true, EnrichmentFailed: []string{}}
	if b.Recorder == nil {
		pm.RecorderUp = false
		pm.Summary = "The flight recorder is not active in this process. This is NOT the same as the episode having no events — nothing here should be read as an absence of activity."
		return pm
	}
	rows, err := b.Recorder.FetchEpisode(ctx, episode)
	if err != nil {
		// A returned postmortem, not an error: a report that says why it is empty is
		// worth more than a stack trace, but it must not share a sentence with the
		// genuinely empty case.
		pm.RecorderUp = false
		pm.Summary = fmt.Sprintf("The flight recorder could not be read (%v). This is NOT the same as the episode having no events — "+
			"nothing here should be read as an absence of activity.", err)
		return pm
	}
	// Verified before the empty early return: an anchor with no surviving rows is a
	// total truncation, the most complete tamper there is, and must not be described
	// as "nothing was recorded here".
	verdict := b.Recorder.VerifyEpisode(ctx, episode, rows)
	if len(rows) == 0 {
		switch {
		case !verdict.Verified:
			pm.Summary = "No events survive for this episode, and the recorder's chain anchor could not be read — so this is NOT a statement " +
				"that nothing was recorded. An episode whose records were all removed looks exactly like this one."
		case verdict.Valid:
			// Genuinely empty. ChainVerified stays false on purpose: nothing was read, so
			// this is neither a statement that records are intact nor that they were altered.
			pm.Summary = "No recorded events for this episode."
		default:
			pm.ChainVerified = true
			pm.Summary = "No events survive for this episode, but the recorder's chain anchor says there were some. Every event has been " +
				"removed — this is NOT an episode in which nothing happened."
		}
		return pm
	}
	pm.ChainValid, pm.ChainVerified = verdict.Valid, verdict.Verified

	for _, row := range rows {
		p := row.Payload
		sum := recorder.Summarise(row.Kind, p)
		pm.Timeline = append(pm.Timeline, TimelineEvent{Seq: row.Seq, Kind: row.Kind, Summary: sum})
		if row.Kind == recorder.GapKind {
			lost := 0
			if v, ok := recorder.Float(p["dropped"]); ok {
				lost = int(v)
			}
			pm.EventsLost += lost
			pm.Gaps = append(pm.Gaps, Gap{row.Seq, lost, mustString(p, "reason")})
		}
		tag := fmt.Sprintf("[#%d] %s", row.Seq, sum)
		switch row.Kind {
		case "finding":
			pm.WhatFired = append(pm.WhatFired, Fired{row.Seq, orDefault(mustString(p, "playbook"), "?"), mustString(p, "namespace"),
				mustString(p, "object"), orDefault(mustString(p, "severity"), "warning")})
		case "tool_call", "tool_result":
			pm.Investigated = append(pm.Investigated, tag)
		case "rollback_point", "hitl_request":
			pm.Tried = append(pm.Tried, tag)
		case "error":
			pm.Errors = append(pm.Errors, tag)
		case "final", "answer":
			pm.Worked = append(pm.Worked, tag)
			if t := mustString(p, "text", "answer", "final_text"); t != "" {
				c := clip(t, 300)
				pm.RootCause = &c
			}
		}
	}
	b.attachTimes(ctx, pm, episode)

	// Root cause and outcome come from the episode summary: the decision log records
	// events, not the final narrative.
	meta, err := b.episodeMeta(ctx, episode)
	switch {
	case err != nil:
		pm.EnrichmentFailed = append(pm.EnrichmentFailed, fmt.Sprintf("root cause / outcome (episode store: %v)", err))
	case meta != nil:
		if meta.rootCause != "" {
			c := clip(meta.rootCause, 300)
			pm.RootCause = &c
		}
		if meta.outcome != "" {
			verdict := meta.outcome
			if meta.verified {
				verdict = "verified"
			}
			pm.Worked = append(pm.Worked, fmt.Sprintf("outcome: %s (%s)", meta.outcome, verdict))
		}
	}

	chain := "intact"
	if !pm.ChainValid {
		chain = "BROKEN"
	}
	pm.Summary = fmt.Sprintf("%d recorded events · %d detector firing(s) · %d investigation step(s) · %d mutation point(s) · %d error(s) · audit chain %s.",
		len(pm.Timeline), len(pm.WhatFired), len(pm.Investigated), len(pm.Tried), len(pm.Errors), chain)

	text, err := b.narrate(ctx, pm)
	if err != nil {
		pm.EnrichmentFailed = append(pm.EnrichmentFailed, fmt.Sprintf("narrative (%v)", err))
	} else {
		pm.Narrative = text
	}
	return pm
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// attachTimes fills the timeline's clock from the created_at column.
func (b *PostmortemBuilder) attachTimes(ctx context.Context, pm *Postmortem, episode string) {
	rows, err := b.DB.QueryContext(ctx, b.DB.Q(`SELECT seq, created_at FROM decision_log WHERE episode_id = ?`), episode)
	if err != nil {
		return
	}
	defer rows.Close()
	at := map[int64]float64{}
	for rows.Next() {
		var seq int64
		var t time.Time
		if rows.Scan(&seq, &t) == nil {
			at[seq] = epoch(t)
		}
	}
	for i := range pm.Timeline {
		pm.Timeline[i].At = at[pm.Timeline[i].Seq]
	}
}

type episodeMeta struct {
	rootCause, outcome string
	verified           bool
}

// episodeMeta looks the episode up by request id. nil with no error means there is
// no matching row, which is a real answer; a failed query is an error, so the caller
// can say the section is missing rather than let its absence read as "there was no
// root cause".
func (b *PostmortemBuilder) episodeMeta(ctx context.Context, episode string) (*episodeMeta, error) {
	var root, outcome *string
	var verified *bool
	err := b.DB.QueryRowContext(ctx, b.DB.Q(`SELECT root_cause, outcome, verified FROM episodes WHERE request_id = ? ORDER BY started_at DESC LIMIT 1`),
		episode).Scan(&root, &outcome, &verified)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		slog.Warn("postmortem episode lookup failed", "episode", episode, "err", err)
		return nil, err
	}
	return &episodeMeta{deref(root), deref(outcome), verified != nil && *verified}, nil
}

// narrate is one optional model call over the timeline. Nil with no error means the
// feature is off or there is nothing to narrate; a failure is an error, because an
// operator who switched the flag on and got no narrative should not have to guess
// whether it took effect.
func (b *PostmortemBuilder) narrate(ctx context.Context, pm *Postmortem) (*string, error) {
	if !b.Cfg.PostmortemNarrative || b.Narrator == nil || len(pm.Timeline) == 0 {
		return nil, nil
	}
	const system = "You are writing a Kubernetes incident postmortem. Use ONLY the recorded events below. Every claim MUST reference the " +
		"event it came from using its [#seq] tag. Do not invent any fact not present in the timeline. If the audit chain is broken, say so " +
		"explicitly. Be concise."
	plain := *pm
	plain.Narrative = nil
	resp, err := b.Narrator.Chat(ctx, []llm.Message{{Role: llm.System, Content: system}, {Role: llm.User, Content: plain.Markdown()}}, nil, llm.Options{})
	if err != nil {
		slog.Warn("postmortem narrative failed, using the timeline only", "err", err)
		return nil, err
	}
	t := strings.TrimSpace(resp.Message.Content)
	if t == "" {
		return nil, nil
	}
	return &t, nil
}

// Markdown renders the postmortem.
func (pm *Postmortem) Markdown() string {
	l := []string{fmt.Sprintf("# Incident postmortem — `%s`", pm.EpisodeID), ""}
	// Three states, not two: a tamper warning is only worth printing if it is never
	// printed when nothing was tampered with.
	switch {
	case !pm.ChainVerified:
		l = append(l, "> ⚠️ **AUDIT CHAIN NOT VERIFIED** — no records were read, so this is neither a statement that they are intact nor that they were altered. See the reason below.")
	case pm.ChainValid:
		l = append(l, "> ✅ Audit chain verified intact — every event below is tamper-evident.")
	default:
		l = append(l, "> ⚠️ **AUDIT CHAIN BROKEN** — the recorded events may have been altered or truncated. See the server log for which.")
	}
	if pm.EventsLost > 0 {
		// Intact and complete are different claims; say the second one out loud.
		seen := map[string]bool{}
		var reasons []string
		for _, g := range pm.Gaps {
			if g.Reason != "" && !seen[g.Reason] {
				seen[g.Reason] = true
				reasons = append(reasons, g.Reason)
			}
		}
		why := strings.Join(reasons, ", ")
		if why == "" {
			why = "cause not recorded"
		}
		l = append(l, fmt.Sprintf("> ⚠️ **RECORD INCOMPLETE** — %d event(s) were never written (%s). Absence of an event below is not evidence it did not happen.", pm.EventsLost, why))
	}
	if len(pm.EnrichmentFailed) > 0 {
		l = append(l, "> ⚠️ **POSTMORTEM INCOMPLETE** — could not read: "+strings.Join(pm.EnrichmentFailed, "; ")+". A section missing below is NOT evidence that it was empty.")
	}
	l = append(l, "", pm.Summary, "")
	if len(pm.Timeline) == 0 {
		return strings.Join(l, "\n")
	}
	if pm.RootCause != nil {
		l = append(l, "## Root cause", *pm.RootCause, "")
	}
	l = append(l, "## Timeline")
	for _, e := range pm.Timeline {
		clock := "--:--:--"
		if e.At != 0 {
			clock = time.Unix(int64(e.At), 0).Format("15:04:05")
		}
		l = append(l, fmt.Sprintf("- `[#%d]` %s **%s** — %s", e.Seq, clock, e.Kind, e.Summary))
	}
	l = append(l, "")
	if len(pm.WhatFired) > 0 {
		l = append(l, "## What fired")
		for _, f := range pm.WhatFired {
			l = append(l, fmt.Sprintf("- `[#%d]` %s (%s) on %s/%s", f.Seq, f.Playbook, f.Severity, f.Namespace, f.Object))
		}
		l = append(l, "")
	}
	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		l = append(l, title)
		for _, x := range items {
			l = append(l, "- "+x)
		}
		l = append(l, "")
	}
	section("## What was investigated", pm.Investigated)
	section("## What was tried (mutations)", pm.Tried)
	section("## Errors encountered", pm.Errors)
	section("## Outcome", pm.Worked)
	section("## Follow-ups", pm.FollowUps)
	if pm.Narrative != nil {
		l = append(l, "## Narrative", *pm.Narrative, "")
	}
	return strings.Join(l, "\n")
}
