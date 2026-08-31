package memory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/DinethShakya23/kube-sre/internal/redact"
	"github.com/DinethShakya23/kube-sre/internal/store"
)

// L1 episodic memory: investigations as recallable episodes.
//
// Recall is text similarity (trigrams) combined with recency and cluster scoping,
// optionally fused with a full text channel by reciprocal rank fusion, and
// optionally weighted by importance. It is computed in process over the most
// recent candidates of the cluster, so it behaves the same on SQLite and Postgres
// and needs no database extension.
//
// Failure discipline: memory must never break a request, so every write catches,
// logs and returns a safe value. Recall is the one exception, and only in that it
// says it could not answer, because a failed lookup reads as "no prior incident".

var EpisodeMigrations = []store.Migration{{
	Version: 41, Name: "episodes",
	SQL: `
CREATE TABLE IF NOT EXISTS episodes (
    id              TEXT PRIMARY KEY,
    cluster_id      TEXT NOT NULL,
    namespace       TEXT,
    trigger_kind    TEXT NOT NULL,
    trigger_detail  TEXT,
    summary         TEXT NOT NULL DEFAULT '',
    root_cause      TEXT,
    actions         {{JSON}} NOT NULL DEFAULT '[]',
    outcome         TEXT,
    verified        BOOLEAN,
    confidence      DOUBLE PRECISION,
    playbooks       {{JSON}} NOT NULL DEFAULT '[]',
    created_by_role TEXT,
    request_id      TEXT,
    started_at      DOUBLE PRECISION NOT NULL,
    ended_at        DOUBLE PRECISION,
    importance      DOUBLE PRECISION,
    surprise        DOUBLE PRECISION,
    trust           DOUBLE PRECISION
);
CREATE INDEX IF NOT EXISTS idx_episodes_cluster_time ON episodes (cluster_id, started_at DESC);`,
}}

// Importance derives from incident severity: a regression a fix caused is the most
// valuable thing to remember, and a report only look the least. verified and
// confidence nudge it up. It is bounded to [0, 1] and modulates RANKING ONLY, never
// retention.
var outcomeSeverity = map[string]float64{"regression": 1.0, "partial": 0.7, "resolved": 0.5, "report_only": 0.3}

// surpriseFloor: a new episode this similar to something in memory is not
// surprising, and if it is also low value the surprise gate drops the write.
const surpriseFloor = 0.15

// candidateLimit bounds how many recent episodes of a cluster recall scans.
const candidateLimit = 2000

func importanceScore(outcome string, verified *bool, confidence *float64) float64 {
	base, ok := outcomeSeverity[strings.ToLower(strings.TrimSpace(outcome))]
	if !ok {
		base = 0.4
	}
	if verified != nil && *verified {
		base += 0.15
	}
	if confidence != nil {
		c := *confidence
		if c < 0 {
			c = 0
		}
		if c > 1 {
			c = 1
		}
		base += 0.1 * c
	}
	if base < 0 {
		base = 0
	}
	if base > 1 {
		base = 1
	}
	return float64(int(base*10000+0.5)) / 10000
}

// isLowValue is unverified AND (no outcome or report only). Only these are ever
// gated by surprise; anything verified or actioned is always kept.
func isLowValue(verified *bool, outcome string) bool {
	o := strings.ToLower(strings.TrimSpace(outcome))
	return (verified == nil || !*verified) && (o == "" || o == "report_only")
}

// TriggerKindFor maps a session's provenance to the episode's durable trigger
// kind. Only the in process value "detector" earns detector provenance. Three
// writers once derived it independently, and in the shipped configuration every
// watchtower investigation was stored as a user query: the column that exists to
// answer "which of these were autonomous?" could not. Provenance also drives the
// write admission trust score, so those episodes were validated as if a chat
// client had typed them.
func TriggerKindFor(source string) string {
	if strings.TrimSpace(source) == "detector" {
		return "detector"
	}
	return "user_query"
}

// impWeight keeps relevance dominant: the weight is in [0.5, 1.0], and an episode
// with no importance is neutral at 0.5 importance.
func impWeight(importance *float64) float64 {
	v := 0.5
	if importance != nil {
		v = *importance
	}
	return 0.5 + 0.5*v
}

// EpisodeInput is one episode to write.
type EpisodeInput struct {
	ClusterID     string
	Namespace     string
	TriggerKind   string
	TriggerDetail string
	Summary       string
	RootCause     string
	Actions       []map[string]any
	Outcome       string
	Verified      *bool
	Confidence    *float64
	Playbooks     []string
	Role          string
	RequestID     string
	StartedAt     float64 // zero means now
	EndedAt       float64
}

// Recalled is one episode returned by recall.
type Recalled struct {
	ID         string
	Summary    string
	RootCause  string
	Outcome    string
	Verified   bool
	Confidence *float64
	Playbooks  []string
	Namespace  string
	StartedAt  float64
	Score      float64
}

// ErrRecallFailed means recall could not answer, which is distinct from nothing
// matching. Returning an empty list for both meant a database outage reached the
// model as an absence of prior incidents.
var ErrRecallFailed = errors.New("episode recall failed")

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6], b[8] = (b[6]&0x0f)|0x40, (b[8]&0x3f)|0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// WriteEpisode inserts one redacted episode and returns its id, or "" when the
// write was dropped or failed. Both are logged and neither is an error: a memory
// write never fails a turn.
//
// With importance on, the episode is scored for importance and surprise, and a
// near duplicate that is also low value is dropped by the surprise gate. With
// security hardening on, a user derived write must clear the write admission guard
// and a quarantined write is dropped.
func (s *Store) WriteEpisode(ctx context.Context, in EpisodeInput) string {
	if in.ClusterID == "" {
		in.ClusterID = "unknown"
	}
	summary := redact.Secrets(in.Summary, 1500)
	var root string
	if in.RootCause != "" {
		root = redact.Secrets(in.RootCause, 500)
	}
	var importance, surprise, trust *float64
	text := summary + " " + root

	if s.Guard != nil {
		d := s.Guard.Admit(in.TriggerKind, firstOf(in.RequestID, in.Role), text)
		t := d.Trust
		trust = &t
		if !d.Admit {
			slog.Info("episode write guard quarantined a write", "reason", d.Reason, "trust", d.Trust, "trigger", in.TriggerKind, "cluster", in.ClusterID)
			// A rejected poison attempt is itself an audit event.
			s.Guard.Audit(ctx, in.ClusterID, "quarantine", "", map[string]any{"reason": d.Reason, "trigger": in.TriggerKind, "trust": round3(d.Trust)})
			return ""
		}
	}
	if s.Cfg.MemoryImportance {
		imp := importanceScore(in.Outcome, in.Verified, in.Confidence)
		importance = &imp
		surprise = s.surprise(ctx, in.ClusterID, text)
		// nil means the score never ran. Fail open: a gate that fires on an unmeasured
		// value would drop writes on the strength of a check that failed.
		if surprise != nil && *surprise < surpriseFloor && isLowValue(in.Verified, in.Outcome) {
			slog.Info("episode surprise gate dropped a low value redundant write", "surprise", *surprise, "cluster", in.ClusterID)
			return ""
		}
	}
	actions, _ := json.Marshal(nonNilActions(in.Actions))
	acts := string(actions)
	if r := []rune(acts); len(r) > 8000 {
		acts = string(r[:8000])
	}
	pb, _ := json.Marshal(nonNilStrings(in.Playbooks))
	started := in.StartedAt
	if started == 0 {
		started = s.now()
	}
	id := newID()
	_, err := s.DB.ExecContext(ctx, s.DB.Q(`INSERT INTO episodes
		(id, cluster_id, namespace, trigger_kind, trigger_detail, summary, root_cause, actions, outcome, verified, confidence,
		 playbooks, created_by_role, request_id, started_at, ended_at, importance, surprise, trust)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		id, in.ClusterID, nullIfEmpty(in.Namespace), in.TriggerKind, redact.Secrets(in.TriggerDetail, 500), summary, nullIfEmpty(root),
		acts, nullIfEmpty(in.Outcome), nullBool(in.Verified), nullFloat(in.Confidence), string(pb), nullIfEmpty(in.Role),
		nullIfEmpty(in.RequestID), started, nullZero(in.EndedAt), nullFloat(importance), nullFloat(surprise), nullFloat(trust))
	if err != nil {
		slog.Warn("episode write failed", "err", err)
		return ""
	}
	// Live writes only. A backfill deliberately does not count: letting it satisfy
	// the "nothing has ever been written" symptom would hide a store that looks
	// populated but is not being fed.
	s.Live.RecordEpisodeWritten()
	if s.OnEpisode != nil {
		s.OnEpisode(ctx, id, in)
	}
	if s.Guard != nil {
		// Tamper evidence: chain each admitted write so a later silent edit is detectable.
		var t any
		if trust != nil {
			t = round3(*trust)
		}
		s.Guard.Audit(ctx, in.ClusterID, "episode_write", id, map[string]any{"trigger": in.TriggerKind, "trust": t})
	}
	return id
}

func round3(f float64) float64 { return float64(int(f*1000+0.5)) / 1000 }

func firstOf(items ...string) string {
	for _, s := range items {
		if s != "" {
			return s
		}
	}
	return ""
}

func nonNilActions(a []map[string]any) []map[string]any {
	if a == nil {
		return []map[string]any{}
	}
	return a
}

func nonNilStrings(a []string) []string {
	if a == nil {
		return []string{}
	}
	return a
}

func nullFloat(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func nullZero(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}

type candidate struct {
	id, summary, rootCause, outcome, namespace string
	verified                                   sql.NullBool
	confidence                                 sql.NullFloat64
	importance                                 sql.NullFloat64
	playbooks                                  string
	startedAt                                  float64
	text                                       string
}

func (s *Store) candidates(ctx context.Context, clusterID string) ([]candidate, error) {
	rows, err := s.DB.QueryContext(ctx, s.DB.Q(`
		SELECT id, summary, root_cause, outcome, verified, confidence, playbooks, namespace, started_at, importance
		FROM episodes WHERE cluster_id = ? AND summary <> '' ORDER BY started_at DESC LIMIT ?`), clusterID, candidateLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var c candidate
		var root, outcome, ns, pb sql.NullString
		if err := rows.Scan(&c.id, &c.summary, &root, &outcome, &c.verified, &c.confidence, &pb, &ns, &c.startedAt, &c.importance); err != nil {
			return nil, err
		}
		c.rootCause, c.outcome, c.namespace, c.playbooks = root.String, outcome.String, ns.String, pb.String
		c.text = c.summary + " " + c.rootCause
		out = append(out, c)
	}
	return out, rows.Err()
}

// surprise is a novelty proxy in [0, 1]: how unlike existing memory an episode is.
// 1.0 is nothing similar seen, 0.0 is a duplicate. It returns nil when it could
// not be measured. That is not a hedge: 1.0 is the strongest novelty claim this
// package makes, and returning it when the query never ran would write that claim
// into the column for every episode.
func (s *Store) surprise(ctx context.Context, clusterID, text string) *float64 {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	cands, err := s.candidates(ctx, clusterID)
	if err != nil {
		slog.Warn("episode surprise scoring failed, novelty is NOT measured and is stored as NULL, not as fully novel", "err", err)
		return nil
	}
	q := trigrams(clip(text, 500))
	top := 0.0
	for _, c := range cands {
		if sim := similarity(q, trigrams(c.text)); sim > top {
			top = sim
		}
	}
	v := 1 - top
	if v < 0 {
		v = 0
	}
	return &v
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// RecallEpisodes returns the top k similar past episodes for a cluster.
//
// Baseline is trigram similarity with recency. With hybrid retrieval on, the
// trigram channel is fused with a full text channel by reciprocal rank fusion,
// which pre filters to the channel matched set, so no trigram floor is applied on
// that path. With importance on, relevance is modulated by an importance weight,
// so recall is recency times importance times relevance. It says so when it could
// not answer.
func (s *Store) RecallEpisodes(ctx context.Context, query, clusterID string, k int) ([]Recalled, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	cands, err := s.candidates(ctx, clusterID)
	if err != nil {
		s.Live.RecordRecallFailure()
		slog.Warn("episode recall failed", "err", err)
		return nil, fmt.Errorf("%w: %v", ErrRecallFailed, err)
	}
	q := clip(query, 500)
	qTri := trigrams(q)
	weight := func(c candidate) float64 {
		if !s.Cfg.MemoryImportance {
			return 1
		}
		var imp *float64
		if c.importance.Valid {
			imp = &c.importance.Float64
		}
		return impWeight(imp)
	}
	sims := make(map[string]float64, len(cands))
	byID := make(map[string]candidate, len(cands))
	for _, c := range cands {
		sims[c.id] = similarity(qTri, trigrams(c.text))
		byID[c.id] = c
	}

	var ordered []ranked
	hybrid := s.Cfg.MemoryHybrid
	floor := s.Cfg.MemorySimFloor
	if hybrid {
		var trgm, fts []ranked
		qTerms := termSet(terms(q))
		for _, c := range cands {
			if sims[c.id] > floor {
				trgm = append(trgm, ranked{c.id, sims[c.id], c.startedAt})
			}
			if sc := lexicalScore(qTerms, terms(c.text)); sc > 0 {
				fts = append(fts, ranked{c.id, sc, c.startedAt})
			}
		}
		fused := map[string]float64{}
		for _, ch := range [][]ranked{rank(trgm), rank(fts)} {
			for i, r := range ch {
				fused[r.id] += 1.0 / float64(rrfK+i+1)
			}
		}
		for id, f := range fused {
			ordered = append(ordered, ranked{id, f * weight(byID[id]), byID[id].startedAt})
		}
	} else {
		for _, c := range cands {
			ordered = append(ordered, ranked{c.id, sims[c.id] * weight(c), c.startedAt})
		}
	}
	ordered = rank(ordered)
	if len(ordered) > k {
		ordered = ordered[:k]
	}
	var out []Recalled
	for _, r := range ordered {
		c := byID[r.id]
		// The baseline applies the trigram noise floor after the limit. The hybrid
		// path already filtered to channel matched rows, so a lexical only match
		// (similarity at or below the floor, but still relevant) must survive it.
		if !hybrid && sims[c.id] <= floor {
			continue
		}
		var pb []string
		_ = json.Unmarshal([]byte(c.playbooks), &pb)
		rec := Recalled{ID: c.id, Summary: c.summary, RootCause: c.rootCause, Outcome: c.outcome, Verified: c.verified.Valid && c.verified.Bool,
			Playbooks: pb, Namespace: c.namespace, StartedAt: c.startedAt, Score: r.score}
		if c.confidence.Valid {
			v := c.confidence.Float64
			rec.Confidence = &v
		}
		out = append(out, rec)
	}
	// Counted after the noise floor: rows that all fall below it are a miss for the
	// caller, and counting them as a hit would let a store that never returns
	// anything usable still report a healthy hit rate.
	s.Live.RecordRecall(len(out) > 0)
	return out, nil
}

// RenderRecallBlock is the compact prompt block for recalled episodes.
func RenderRecallBlock(eps []Recalled) string {
	if len(eps) == 0 {
		return ""
	}
	lines := []string{"## Similar past episodes (this cluster)"}
	for _, ep := range eps {
		verified := "unverified"
		if ep.Verified {
			verified = "verified"
		}
		outcome := ep.Outcome
		if outcome == "" {
			outcome = "report_only"
		}
		lines = append(lines, fmt.Sprintf("- [%s/%s] %s", outcome, verified, clip(strings.ReplaceAll(ep.Summary, "\n", " "), 220)))
		if ep.RootCause != "" {
			lines = append(lines, "  root_cause: "+clip(ep.RootCause, 160))
		}
	}
	return strings.Join(lines, "\n")
}

// BackfillFromRCAOutcomes is a one shot, idempotent migration of rca_outcomes
// into episodes with trigger kind "backfill". A row is skipped if an episode with
// the same start time and root cause is already there.
func (s *Store) BackfillFromRCAOutcomes(ctx context.Context) int {
	rows, err := s.DB.QueryContext(ctx, `SELECT cluster_id, namespace, root_cause, recommended_fix, outcome_feedback, verified_resolved,
		confidence, playbooks_matched, created_by_role, request_id, created_at FROM rca_outcomes`)
	if err != nil {
		slog.Warn("episode backfill failed", "err", err)
		s.Live.RecordPassFailure("backfilled", err)
		return 0
	}
	type row struct {
		cluster, ns, root, fix, outcome, pb, role, req sql.NullString
		verified                                       sql.NullBool
		conf                                           sql.NullFloat64
		at                                             float64
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.cluster, &r.ns, &r.root, &r.fix, &r.outcome, &r.verified, &r.conf, &r.pb, &r.role, &r.req, &r.at); err != nil {
			rows.Close()
			slog.Warn("episode backfill failed", "err", err)
			return 0
		}
		all = append(all, r)
	}
	rows.Close()
	n := 0
	for _, r := range all {
		var exists int
		_ = s.DB.QueryRowContext(ctx, s.DB.Q(`SELECT COUNT(*) FROM episodes WHERE trigger_kind = 'backfill' AND started_at = ? AND root_cause IS NOT DISTINCT FROM ?`),
			r.at, nullIfEmpty(r.root.String)).Scan(&exists)
		if exists > 0 {
			continue
		}
		cluster := r.cluster.String
		if cluster == "" {
			cluster = "unknown"
		}
		acts, _ := json.Marshal(r.fix.String)
		pb := r.pb.String
		if pb == "" {
			pb = "[]"
		}
		if _, err := s.DB.ExecContext(ctx, s.DB.Q(`INSERT INTO episodes (id, cluster_id, namespace, trigger_kind, trigger_detail, summary, root_cause,
			actions, outcome, verified, confidence, playbooks, created_by_role, request_id, started_at, ended_at)
			VALUES (?, ?, ?, 'backfill', 'migrated from rca_outcomes', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			newID(), cluster, nullIfEmpty(r.ns.String), r.root.String, nullIfEmpty(r.root.String), string(acts), nullIfEmpty(r.outcome.String),
			nullBoolSQL(r.verified), nullFloatSQL(r.conf), pb, nullIfEmpty(r.role.String), nullIfEmpty(r.req.String), r.at, r.at); err != nil {
			slog.Warn("episode backfill failed", "err", err)
			s.Live.RecordPassFailure("backfilled", err)
			return n
		}
		n++
	}
	if n > 0 {
		slog.Info("episodes backfilled from rca_outcomes", "rows", n)
	}
	return n
}

func nullBoolSQL(b sql.NullBool) any {
	if !b.Valid {
		return nil
	}
	return b.Bool
}

func nullFloatSQL(f sql.NullFloat64) any {
	if !f.Valid {
		return nil
	}
	return f.Float64
}
