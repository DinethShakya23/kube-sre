// Package detectstore is the detector queue: candidates authored in plain English,
// staged as shadow detectors, and promoted to active only by a human.
//
//	candidate -> shadow (accruing precision) -> active | demoted
//
// Only active detectors reach the watchtower. Promotion is always a human action:
// the statistics only decide when a detector has earned a review.
package detectstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/autonomy"
	"github.com/DinethShakya23/kube-sre/internal/detect"
	"github.com/DinethShakya23/kube-sre/internal/llm"
	"github.com/DinethShakya23/kube-sre/internal/store"
)

var Migrations = []store.Migration{{
	Version: 50, Name: "detectors",
	SQL: `
CREATE TABLE IF NOT EXISTS detectors (
    id              {{PK}},
    cluster_id      TEXT NOT NULL DEFAULT 'global',
    name            TEXT NOT NULL,
    source          TEXT NOT NULL DEFAULT 'learned',
    predicate       {{JSON}} NOT NULL,
    status          TEXT NOT NULL DEFAULT 'candidate',
    precision_stats {{JSON}} NOT NULL DEFAULT '{}',
    created_from    TEXT,
    reviewed_by     TEXT,
    created_at      DOUBLE PRECISION NOT NULL DEFAULT 0,
    UNIQUE (cluster_id, name)
);`,
}}

// ErrUnavailable means the store could not be read, which is not the store holding
// no detectors. Collapsing the two is how an operator concludes their cluster has no
// coverage when in fact the question was never answered.
var ErrUnavailable = errors.New("the detector store could not be read")

// ErrCannotFire means promotion was refused because the predicates can never match.
// It is distinct from not found: reporting a dead detector as missing would be wrong,
// and reporting it as promoted is worse, because a detector that never fires reads as
// a cluster that never has the problem.
var ErrCannotFire = errors.New("the detector can never fire")

// Store reads and writes the detectors table.
type Store struct {
	DB  *store.DB
	Now func() time.Time
}

func New(db *store.DB) *Store { return &Store{DB: db, Now: time.Now} }

// Row is one stored detector.
type Row struct {
	Name           string         `json:"name"`
	Source         string         `json:"source"`
	Status         string         `json:"status"`
	Predicate      map[string]any `json:"predicate"`
	PrecisionStats map[string]any `json:"precision_stats"`
	CreatedFrom    *string        `json:"created_from"`
	ReviewedBy     *string        `json:"reviewed_by"`
	CreatedAt      string         `json:"created_at"`
}

func decode(raw []byte) map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// List returns a cluster's detectors, newest first. An empty list means exactly one
// thing: the store was read and holds nothing.
func (s *Store) List(ctx context.Context, status, clusterID string) ([]Row, error) {
	q := `SELECT name, source, status, predicate, precision_stats, created_from, reviewed_by, created_at FROM detectors WHERE cluster_id = ?`
	args := []any{clusterID}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	rows, err := s.DB.QueryContext(ctx, s.DB.Q(q+` ORDER BY created_at DESC, id DESC`), args...)
	if err != nil {
		slog.Warn("detector list failed", "err", err)
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		var r Row
		var pred, stats []byte
		var at float64
		if err := rows.Scan(&r.Name, &r.Source, &r.Status, &pred, &stats, &r.CreatedFrom, &r.ReviewedBy, &at); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		r.Predicate, r.PrecisionStats = decode(pred), decode(stats)
		r.CreatedAt = time.Unix(int64(at), 0).UTC().Format(time.RFC3339)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return out, nil
}

// Stage inserts a validated block as a shadow candidate. The stored predicate is
// the raw detect mapping so the engine can recompile it. It reports whether a row
// was created, and never fails the caller.
func (s *Store) Stage(ctx context.Context, name, description string, raw map[string]any, author, clusterID string) bool {
	body, _ := json.Marshal(raw)
	from := description
	if r := []rune(from); len(r) > 500 {
		from = string(r[:500])
	}
	res, err := s.DB.ExecContext(ctx, s.DB.Q(`INSERT INTO detectors (cluster_id, name, source, predicate, status, created_from, reviewed_by, created_at)
		VALUES (?, ?, 'nl', ?, 'shadow', ?, ?, ?) ON CONFLICT (cluster_id, name) DO NOTHING`),
		clusterID, name, string(body), from, author, float64(s.Now().UnixNano())/1e9)
	if err != nil {
		slog.Warn("detector stage failed", "err", err)
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

func (s *Store) setStatus(ctx context.Context, name, status, reviewer, clusterID string) bool {
	res, err := s.DB.ExecContext(ctx, s.DB.Q(`UPDATE detectors SET status = ?, reviewed_by = ? WHERE cluster_id = ? AND name = ?`), status, reviewer, clusterID, name)
	if err != nil {
		slog.Warn("detector status change failed", "err", err)
		return false
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		slog.Info("detector review", "name", name, "status", status, "by", reviewer)
	}
	return n > 0
}

// livenessError says why a stored detector can never fire, or "". An unreadable or
// absent row is not our call and returns "".
//
// The lookup takes the cluster's own row or the global one, as the loader does:
// staging defaults to global, so a promotion requested from a cluster that sets a
// cluster id found no row here and let a dead detector through the one gate written
// to stop it.
func (s *Store) livenessError(ctx context.Context, name, clusterID string) string {
	var raw []byte
	err := s.DB.QueryRowContext(ctx, s.DB.Q(`SELECT predicate FROM detectors WHERE cluster_id IN (?, 'global') AND name = ?
		ORDER BY (CASE WHEN cluster_id = ? THEN 1 ELSE 0 END) DESC LIMIT 1`), clusterID, name, clusterID).Scan(&raw)
	if err != nil {
		return ""
	}
	block, err := detect.ParseBlock(name, decode(raw))
	if err != nil || block == nil {
		return ""
	}
	for _, p := range block.WatchPredicates {
		errs, _ := detect.PredicateLivenessErrors(p, false)
		if len(errs) == 0 {
			errs = detect.PredicateHealthErrors(p)
		}
		if len(errs) > 0 {
			return errs[0]
		}
	}
	for _, t := range block.TrendPredicates {
		if errs := detect.TrendLivenessErrors(t); len(errs) > 0 {
			return errs[0]
		}
	}
	return ""
}

// Promote makes a shadow or candidate detector active, so it now reaches the
// watchtower. It returns ErrCannotFire if the predicates provably cannot match: a
// shadow detector is promoted on the strength of its precision, and a dead one shows
// zero firings, which is indistinguishable from the condition never occurring.
func (s *Store) Promote(ctx context.Context, name, reviewer, clusterID string) (bool, error) {
	if dead := s.livenessError(ctx, name, clusterID); dead != "" {
		slog.Warn("detector promotion refused", "name", name, "reason", dead)
		return false, fmt.Errorf("%w: detector %q can never fire: %s", ErrCannotFire, name, dead)
	}
	return s.setStatus(ctx, name, "active", reviewer, clusterID), nil
}

// Demote rejects a detector: it stops firing entirely.
func (s *Store) Demote(ctx context.Context, name, reviewer, clusterID string) bool {
	return s.setStatus(ctx, name, "demoted", reviewer, clusterID)
}

func isDetectBlock(p map[string]any) bool {
	_, w := p["watch_predicates"]
	_, t := p["trend_predicates"]
	return w || t
}

// Load compiles the stored detectors into (active, shadow). No rows means exactly
// one thing: there are no stored detectors. A query that failed is ErrUnavailable and
// not empty sets: the only caller assigns the result straight into the live engine,
// so a transient blip would silently unload every promoted detector until the next
// successful refresh.
//
// Rows stored under the global cluster load on every cluster. Staging, promotion and
// listing all default to global while the reader is called with the deployment's own
// cluster id, and nothing reconciled the two, so an authored detector was stored,
// listed as shadow, promotable and never loaded. A 24 hour shadow soak reported a
// false positive rate of 0.0 while evaluating nothing.
func (s *Store) Load(ctx context.Context, clusterID string) (active, shadow []detect.DetectBlock, err error) {
	rows, err := s.DB.QueryContext(ctx, s.DB.Q(`SELECT name, predicate, status FROM detectors
		WHERE cluster_id IN (?, 'global') AND status IN ('active', 'shadow')`), clusterID)
	if err != nil {
		slog.Warn("detector load failed", "err", err)
		return nil, nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	type stored struct {
		name, status string
		pred         []byte
	}
	var all []stored
	for rows.Next() {
		var r stored
		if err := rows.Scan(&r.name, &r.pred, &r.status); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		all = append(all, r)
	}
	rows.Close()

	var skipped []string
	for _, r := range all {
		pred := decode(r.pred)
		if !isDetectBlock(pred) {
			skipped = append(skipped, r.name+": predicate is not a detect block")
			continue
		}
		block, err := detect.ParseBlock(r.name, pred)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: predicate did not compile (%v)", r.name, err))
			continue
		}
		if block == nil {
			skipped = append(skipped, r.name+": predicate compiled to nothing")
			continue
		}
		// Compiling is not being able to fire. Authoring is not the only way a row
		// reaches this table (consolidation writes here and promotion only flips the
		// status), so this is the one point every stored detector passes through.
		//
		// The refusal is per predicate and not per row, and that is load bearing. One
		// row carried a trend predicate pinned to the model's own template and a Pod
		// predicate matching every healthy pod. Refusing the whole row for the first
		// would have deleted the second, a false positive source on a soak whose
		// endpoint is the false positive rate: it would have improved the measured
		// result by removing the evidence that falsified it.
		var liveWatch []detect.WatchPredicate
		var liveTrend []detect.TrendPredicate
		var dropped []string
		for _, p := range block.WatchPredicates {
			if errs, _ := detect.PredicateLivenessErrors(p, false); len(errs) > 0 {
				dropped = append(dropped, errs...)
			} else {
				liveWatch = append(liveWatch, p)
			}
		}
		for _, t := range block.TrendPredicates {
			if errs := detect.TrendLivenessErrors(t); len(errs) > 0 {
				dropped = append(dropped, errs...)
			} else {
				liveTrend = append(liveTrend, t)
			}
		}
		if len(dropped) > 0 && len(liveWatch)+len(liveTrend) == 0 {
			slog.Warn("db_detector_can_never_fire, not loaded", "name", r.name, "status", r.status, "reason", dropped[0])
			continue
		}
		if len(dropped) > 0 {
			slog.Warn("db_detector_predicate_can_never_fire, that predicate is dropped", "name", r.name, "status", r.status,
				"reason", dropped[0], "live", len(liveWatch)+len(liveTrend))
			block.WatchPredicates, block.TrendPredicates, block.DroppedPredicates = liveWatch, liveTrend, dropped
		}
		// A live predicate that matches a healthy object is not dropped: it is loaded,
		// evaluated and named, so whoever reads its findings can see why there are so
		// many. Deleting it would remove the evidence that the detector is wrong.
		var unhealthy []string
		for _, p := range block.WatchPredicates {
			unhealthy = append(unhealthy, detect.PredicateHealthErrors(p)...)
		}
		if len(unhealthy) > 0 {
			slog.Warn("db_detector_fires_on_healthy_objects, loaded anyway", "name", r.name, "status", r.status, "reason", unhealthy[0])
			block.FiresOnHealthy = unhealthy
		}
		if r.status == "active" {
			active = append(active, *block)
		} else {
			shadow = append(shadow, *block)
		}
	}
	if len(skipped) > 0 {
		more := ""
		if len(skipped) > 5 {
			more = fmt.Sprintf(" (+%d more)", len(skipped)-5)
			skipped = skipped[:5]
		}
		slog.Warn(fmt.Sprintf("%d of %d stored detector(s) were not loaded: %s%s", len(skipped), len(all), strings.Join(skipped, "; "), more))
	}
	return active, shadow, nil
}

// ── authoring ────────────────────────────────────────────────────────────────

const authoringSystem = `You compile a Kubernetes failure description into a detector ` + "`detect:`" + ` block.

Output ONLY a JSON object with any of these keys (omit those you don't need):
- "watch_predicates": list of {kind: "Pod"|"Event"|"Node",
    status_regex (Pod/Node, matched against the STATUS column),
    reason_regex, message_regex (Event; Warning events only),
    involved_kind (optional)}
- "promql": OPTIONAL context only — these queries are recorded but NOT evaluated,
  so a detector whose only predicate is promql can never fire. Never rely on it.
- "trend_predicates": list of {metric (range PromQL), threshold (number),
    window_minutes, projection_horizon_minutes, fire_if_eta_within_minutes,
    direction: "rising"|"falling", min_r2} — for forecasting a slow-burn failure
- "debounce_seconds": integer

Hard rules, each one written because a model broke it and the result was stored as a live
detector that could never fire:
- NEVER leave a placeholder in a PromQL selector. If the description does not name a concrete
  deployment/service/namespace, omit the label matcher entirely rather than writing
  {deployment="your-deployment-name"} — an unmatched selector returns no series and the
  detector is silently dead.
- "direction" must be exactly "rising" or "falling". Anything else is read as "rising", so a
  typo asks the opposite question instead of failing.
- "min_r2" is a squared correlation coefficient: it must be in [0, 1].
- A Pod "status_regex" is matched against the STATUS column ` + "`kubectl get pods`" + ` prints — a
  waiting reason (CrashLoopBackOff, ImagePullBackOff), a terminated reason (OOMKilled, Error,
  Completed), Init:<reason>, Evicted, Terminating, or a bare phase. It is NOT the pod's phase
  alone and NOT an arbitrary word: "NotReady" in particular does NOT mean "the readiness probe
  is failing" (kubectl prints "Running" for that pod) — use an Event predicate on Unhealthy.
- A Pod "status_regex" must NEVER match a HEALTHY status: Running, Completed or Succeeded (nor
  a Node "status_regex" matching Ready). A predicate has no namespace or label scope — it is
  matched against the status and nothing else — so "^Running$" means "fire on every pod on the
  cluster", not "fire on the pod I described". Adding a trend_predicate does not narrow it: the
  two are evaluated by separate loops and OR'd, never AND'd. If the condition is a resource level
  ("pinned at its CPU limit", "memory climbing"), express it as a trend_predicate ALONE and emit
  NO watch_predicates.

Use anchored, specific RE2 regexes. Examples:
{"watch_predicates": [{"kind": "Pod", "status_regex": "^OOMKilled$"}]}
{"watch_predicates": [{"kind": "Event", "reason_regex": "^BackOff$",
  "message_regex": "Back-off restarting failed container", "involved_kind": "Pod"}]}

Return JSON only — no prose, no code fences.`

var jsonRe = regexp.MustCompile(`(?s)\{.*\}`)

// Compile has the model turn a plain English failure description into a detect
// mapping. It is a one time authoring call; the detector it produces runs with zero
// tokens like every other. It fails open with an empty mapping, so the caller
// surfaces a validation message and not a crash.
func Compile(ctx context.Context, model llm.Model, description string) map[string]any {
	if model == nil {
		return map[string]any{}
	}
	resp, err := model.Chat(ctx, []llm.Message{{Role: llm.System, Content: authoringSystem}, {Role: llm.User, Content: description}}, nil, llm.Options{})
	if err != nil {
		slog.Warn("detector authoring compile failed", "err", err)
		return map[string]any{}
	}
	return parseJSON(resp.Message.Content)
}

func parseJSON(text string) map[string]any {
	m := jsonRe.FindString(text)
	if m == "" {
		return map[string]any{}
	}
	out := map[string]any{}
	if json.Unmarshal([]byte(m), &out) != nil {
		return map[string]any{}
	}
	return out
}

// Validate runs a compiled block through the real compiler, then the checks a
// compiler cannot make. It returns a nil block and the reasons when nothing valid
// compiled, a predicate is malformed, a predicate provably can never match, a trend
// predicate is an unfilled template, or a predicate matches a healthy object and so
// fires on the whole cluster.
//
// Compiling is not the same as being able to fire. A model writing a regex from
// prose reproduces a stray space inside an anchored alternation more readily than a
// person reading the schema does. And the mirror image shipped too: a detector
// authored from "a workload is pinned at its CPU limit" compiled to a Pod predicate
// matching ^Running$, which fires on every healthy pod, 46 of them on an idle cluster
// before any fault was injected. The predicate has no scope to narrow it afterwards,
// so this is the only place to stop it.
func Validate(raw map[string]any, name string) (*detect.DetectBlock, []string) {
	if raw == nil {
		return nil, []string{"compiler did not return a JSON object"}
	}
	block, err := detect.ParseBlock(name, raw)
	if err != nil {
		return nil, []string{"invalid predicate: " + err.Error()}
	}
	if block == nil {
		return nil, []string{"no valid predicates (need watch_predicates or trend_predicates; promql is recorded but never evaluated, so it cannot fire)"}
	}
	var dead []string
	for _, p := range block.WatchPredicates {
		errs, _ := detect.PredicateLivenessErrors(p, false)
		dead = append(dead, errs...)
	}
	for _, t := range block.TrendPredicates {
		dead = append(dead, detect.TrendLivenessErrors(t)...)
	}
	for _, p := range block.WatchPredicates {
		dead = append(dead, detect.PredicateHealthErrors(p)...)
	}
	if len(dead) > 0 {
		return nil, dead
	}
	return block, nil
}

// ── the statistical ladder ───────────────────────────────────────────────────

// Verdict says whether a shadow detector has earned a human promotion review.
type Verdict struct {
	ReadyForReview bool     `json:"ready_for_review"`
	PrecisionLCB   float64  `json:"precision_lcb"`
	Firings        int      `json:"firings"`
	Reasons        []string `json:"reasons"`
}

// Evaluate scores shadow firings (true when a firing matched a real issue). Ready
// needs enough firings and a precision lower bound at or above theta. It never
// activates anything: it only flags the detector for a human's final decision.
func Evaluate(truePositive []bool, minFirings int, theta float64) Verdict {
	n, tp := len(truePositive), 0
	for _, t := range truePositive {
		if t {
			tp++
		}
	}
	lcb := autonomy.WilsonLCB(float64(tp), float64(n))
	reasons := []string{}
	if n < minFirings {
		reasons = append(reasons, fmt.Sprintf("only %d shadow firings (< %d)", n, minFirings))
	}
	if lcb < theta {
		reasons = append(reasons, fmt.Sprintf("precision LCB %.3f < θ %v", lcb, theta))
	}
	return Verdict{len(reasons) == 0, lcb, n, reasons}
}
