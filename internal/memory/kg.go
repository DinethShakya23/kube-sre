package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/sensorium"
	"github.com/DinethShakya23/kube-sre/internal/store"
)

// L2 semantic memory: a temporal knowledge graph.
//
// Entities and valid time edges: an edge with valid_to NULL is currently true,
// and closing an edge stamps valid_to. That turns "what changed between 14:02 and
// 14:07" into one indexed query instead of digging through logs.
//
// Failure discipline: no memory failure may break a user facing response. The
// write path catches, logs and returns a harmless fallback. Reads that feed the
// model are the opposite case, because their silence is an assertion, and they say
// so with ErrKGUnavailable.

var KGMigrations = []store.Migration{{
	Version: 42, Name: "knowledge graph",
	SQL: `
CREATE TABLE IF NOT EXISTS kg_entities (
    id         TEXT PRIMARY KEY,
    cluster_id TEXT NOT NULL,
    kind       TEXT NOT NULL,
    name       TEXT NOT NULL,
    namespace  TEXT NOT NULL DEFAULT '',
    attrs      {{JSON}} NOT NULL DEFAULT '{}',
    UNIQUE (cluster_id, kind, namespace, name)
);
CREATE TABLE IF NOT EXISTS kg_edges (
    id           TEXT PRIMARY KEY,
    cluster_id   TEXT NOT NULL,
    src          TEXT NOT NULL REFERENCES kg_entities(id) ON DELETE CASCADE,
    rel          TEXT NOT NULL,
    dst          TEXT NOT NULL REFERENCES kg_entities(id) ON DELETE CASCADE,
    attrs        {{JSON}} NOT NULL DEFAULT '{}',
    valid_from   DOUBLE PRECISION NOT NULL,
    valid_to     DOUBLE PRECISION,
    source_kind  TEXT NOT NULL DEFAULT 'observation',
    source_id    TEXT,
    ingested_at  DOUBLE PRECISION NOT NULL,
    retracted_at DOUBLE PRECISION
);
CREATE INDEX IF NOT EXISTS idx_kg_edges_window ON kg_edges (cluster_id, valid_from, valid_to);
CREATE INDEX IF NOT EXISTS idx_kg_edges_open ON kg_edges (src, rel);
CREATE INDEX IF NOT EXISTS idx_kg_edges_tx ON kg_edges (cluster_id, ingested_at, retracted_at);`,
}}

// ErrKGUnavailable means a graph read could not be answered, which is not the same
// as nothing matching. A failed changes() once returned an empty list, and the
// prompt simply omitted its "Recent cluster changes" section, byte for byte what a
// genuinely calm cluster produces. "What changed in the last fifteen minutes" is
// the first question of an incident, and an outage answered it with "nothing did".
var ErrKGUnavailable = errors.New("the cluster change log could not be read")

func attrsJSON(a map[string]any) string {
	if a == nil {
		a = map[string]any{}
	}
	b, _ := json.Marshal(a)
	return string(b)
}

func parseAttrs(raw any) map[string]any {
	var b []byte
	switch v := raw.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	}
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return out
}

func jsonEqual(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// UpsertEntity inserts or merge updates one entity and returns its id, or "" on failure.
func (s *Store) UpsertEntity(ctx context.Context, clusterID, kind, name, namespace string, attrs map[string]any) string {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		slog.Warn("kg upsert entity failed", "kind", kind, "name", name, "err", err)
		return ""
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, s.DB.Q(`INSERT INTO kg_entities (id, cluster_id, kind, name, namespace, attrs) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (cluster_id, kind, namespace, name) DO NOTHING`), newID(), clusterID, kind, name, namespace, attrsJSON(attrs)); err != nil {
		slog.Warn("kg upsert entity failed", "kind", kind, "name", name, "err", err)
		return ""
	}
	var id string
	var raw []byte
	if err = tx.QueryRowContext(ctx, s.DB.Q(`SELECT id, attrs FROM kg_entities WHERE cluster_id = ? AND kind = ? AND namespace = ? AND name = ?`),
		clusterID, kind, namespace, name).Scan(&id, &raw); err != nil {
		slog.Warn("kg upsert entity failed", "kind", kind, "name", name, "err", err)
		return ""
	}
	old := parseAttrs(raw)
	merged := map[string]any{}
	for k, v := range old {
		merged[k] = v
	}
	for k, v := range attrs {
		merged[k] = v
	}
	if !jsonEqual(old, merged) {
		if _, err = tx.ExecContext(ctx, s.DB.Q(`UPDATE kg_entities SET attrs = ? WHERE id = ?`), attrsJSON(merged), id); err != nil {
			slog.Warn("kg upsert entity failed", "kind", kind, "name", name, "err", err)
			return ""
		}
	}
	if err = tx.Commit(); err != nil {
		slog.Warn("kg upsert entity failed", "kind", kind, "name", name, "err", err)
		return ""
	}
	return id
}

// eventTime is used for valid_from and valid_to only when bi-temporality is on.
// Freshness (ingested_at minus valid_from) is only meaningful if valid_from is the
// real world event time. With the flag off, or no event time supplied, it returns
// 0 and the caller falls back to now.
func (s *Store) eventTime(t float64) float64 {
	if t == 0 || !s.Cfg.MemoryBitemporal {
		return 0
	}
	return t
}

func orNow(t, now float64) float64 {
	if t == 0 {
		return now
	}
	return t
}

// EdgeOpts carries the optional parts of an edge write.
type EdgeOpts struct {
	Attrs      map[string]any
	SourceKind string // default "observation"
	SourceID   string
	EventTime  float64
}

// OpenEdge opens a valid time edge. It is idempotent: a matching open edge is a no-op.
func (s *Store) OpenEdge(ctx context.Context, clusterID, src, rel, dst string, o EdgeOpts) {
	var existing string
	err := s.DB.QueryRowContext(ctx, s.DB.Q(`SELECT id FROM kg_edges WHERE cluster_id = ? AND src = ? AND rel = ? AND dst = ? AND valid_to IS NULL`),
		clusterID, src, rel, dst).Scan(&existing)
	if err == nil {
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		slog.Warn("kg open edge failed", "rel", rel, "err", err)
		return
	}
	kind := o.SourceKind
	if kind == "" {
		kind = "observation"
	}
	now := s.now()
	if _, err = s.DB.ExecContext(ctx, s.DB.Q(`INSERT INTO kg_edges (id, cluster_id, src, rel, dst, attrs, valid_from, source_kind, source_id, ingested_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`), newID(), clusterID, src, rel, dst, attrsJSON(o.Attrs),
		orNow(s.eventTime(o.EventTime), now), kind, nullIfEmpty(o.SourceID), now); err != nil {
		slog.Warn("kg open edge failed", "rel", rel, "err", err)
	}
}

// CloseEdge stamps valid_to on matching open edges; dst is an optional filter. With
// bi-temporality on, the event time records when the fact stopped being true in the
// world, not when we noticed.
func (s *Store) CloseEdge(ctx context.Context, clusterID, src, rel, dst string, eventTime float64) {
	vt := orNow(s.eventTime(eventTime), s.now())
	q := `UPDATE kg_edges SET valid_to = ? WHERE cluster_id = ? AND src = ? AND rel = ? AND valid_to IS NULL`
	args := []any{vt, clusterID, src, rel}
	if dst != "" {
		q += ` AND dst = ?`
		args = append(args, dst)
	}
	if _, err := s.DB.ExecContext(ctx, s.DB.Q(q), args...); err != nil {
		slog.Warn("kg close edge failed", "rel", rel, "err", err)
	}
}

// RetractEdge retracts edges on the TRANSACTION time axis: retracted_at means "we
// no longer believe we ever should have recorded this", as opposed to CloseEdge,
// which says the fact stopped being true. It never deletes, so time travel and
// audit stay intact. It returns the number of rows retracted.
func (s *Store) RetractEdge(ctx context.Context, clusterID, src, rel, dst string) int {
	q := `UPDATE kg_edges SET retracted_at = ? WHERE cluster_id = ? AND src = ? AND rel = ? AND retracted_at IS NULL`
	args := []any{s.now(), clusterID, src, rel}
	if dst != "" {
		q += ` AND dst = ?`
		args = append(args, dst)
	}
	res, err := s.DB.ExecContext(ctx, s.DB.Q(q), args...)
	if err != nil {
		slog.Warn("kg retract edge failed", "rel", rel, "err", err)
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}

// Write reconciliation and salience.
const (
	salienceFloor = 0.2 // below this, a candidate write is a no-op
	retractFloor  = 0.5 // supersede a functional relation only above this, else add
)

var (
	functionalRels = map[string]bool{"runs_on": true, "has_status": true} // src has at most one valid dst
	highValueRels  = map[string]bool{"crashed_with": true, "fixed_by": true, "caused_by": true}
)

// salienceScore is a query independent heuristic value of a candidate fact, in
// [0, 1]. Incident linking relations and higher severity or verified facts score
// higher; structural observation edges get a passing baseline.
func salienceScore(rel string, attrs map[string]any) float64 {
	score := 0.4 // structural baseline: runs_on and owns keep the graph connected
	if highValueRels[rel] {
		score += 0.4
	}
	switch strings.ToLower(fmt.Sprint(attrs["severity"])) {
	case "critical", "error", "fatal":
		score += 0.3
	case "warning", "warn":
		score += 0.1
	}
	if v, ok := attrs["verified"].(bool); ok && v {
		score += 0.1
	}
	return math.Min(1, score)
}

// ReconcileEdge decides ADD, UPDATE, RETRACT or NOOP for one candidate edge
// against existing memory, gated by a salience value function. RETRACT sets
// retracted_at (never deletes) and fires only for corrections of a functional
// relation when confidence is high: a real world change is still a valid time
// CloseEdge, not a retraction. It is meant for the extracted fact write path, not
// sensor ingest, where observations are ground truth. With the flag off it behaves
// like OpenEdge.
func (s *Store) ReconcileEdge(ctx context.Context, clusterID, src, rel, dst string, o EdgeOpts, salience *float64) string {
	if !s.Cfg.MemoryReconcile {
		s.OpenEdge(ctx, clusterID, src, rel, dst, o)
		return "ADD"
	}
	score := salienceScore(rel, o.Attrs)
	if salience != nil {
		score = *salience
	}
	if score < salienceFloor {
		return "NOOP"
	}
	var id string
	var raw []byte
	err := s.DB.QueryRowContext(ctx, s.DB.Q(`SELECT id, attrs FROM kg_edges WHERE cluster_id = ? AND src = ? AND rel = ? AND dst = ?
		AND valid_to IS NULL AND retracted_at IS NULL`), clusterID, src, rel, dst).Scan(&id, &raw)
	switch {
	case err == nil:
		old := parseAttrs(raw)
		merged := map[string]any{}
		for k, v := range old {
			merged[k] = v
		}
		for k, v := range o.Attrs {
			merged[k] = v
		}
		if len(o.Attrs) == 0 || jsonEqual(old, merged) {
			return "NOOP" // a redundant re-assertion
		}
		if _, err := s.DB.ExecContext(ctx, s.DB.Q(`UPDATE kg_edges SET attrs = ? WHERE id = ?`), attrsJSON(merged), id); err != nil {
			slog.Warn("kg reconcile edge failed", "rel", rel, "err", err)
			return "NOOP"
		}
		return "UPDATE"
	case !errors.Is(err, sql.ErrNoRows):
		slog.Warn("kg reconcile edge failed", "rel", rel, "err", err)
		return "NOOP"
	}
	if functionalRels[rel] && score >= retractFloor {
		var other string
		err := s.DB.QueryRowContext(ctx, s.DB.Q(`SELECT dst FROM kg_edges WHERE cluster_id = ? AND src = ? AND rel = ?
			AND valid_to IS NULL AND retracted_at IS NULL LIMIT 1`), clusterID, src, rel).Scan(&other)
		if err == nil && other != dst {
			s.RetractEdge(ctx, clusterID, src, rel, other)
			slog.Info("kg supersede", "cluster", clusterID, "src", src, "rel", rel, "old_dst", other, "new_dst", dst, "salience", score)
			s.OpenEdge(ctx, clusterID, src, rel, dst, o)
			return "RETRACT"
		}
	}
	s.OpenEdge(ctx, clusterID, src, rel, dst, o)
	return "ADD"
}

func (s *Store) closeAllEdges(ctx context.Context, clusterID, entity string) {
	if _, err := s.DB.ExecContext(ctx, s.DB.Q(`UPDATE kg_edges SET valid_to = ? WHERE cluster_id = ? AND (src = ? OR dst = ?) AND valid_to IS NULL`),
		s.now(), clusterID, entity, entity); err != nil {
		slog.Warn("kg close all edges failed", "err", err)
	}
}

func (s *Store) openRunsOnDst(ctx context.Context, clusterID, pod string) string {
	var dst string
	err := s.DB.QueryRowContext(ctx, s.DB.Q(`SELECT dst FROM kg_edges WHERE cluster_id = ? AND src = ? AND rel = 'runs_on' AND valid_to IS NULL`), clusterID, pod).Scan(&dst)
	if err != nil {
		return ""
	}
	return dst
}

// ObservationRef is a checkable handle for the object version an edge was derived
// from, or "".
//
// Every edge carries a source kind, and that string is the sole input to the write
// admission trust score, where "observation" scores 1.0. The accompanying source id
// used to be NULL on every ingested edge, so the graph asserted a provenance class
// it could not resolve: WHICH observation was unanswerable. An id for the
// Observation itself would point at nothing, since observations are an in memory
// stream. The apiserver's uid plus resourceVersion is the real evidence handle: it
// names the exact object version and can be checked against the cluster. It has
// limits: resourceVersion is not retained indefinitely and the object may be
// deleted, so an old ref may no longer resolve. It still says what to look for.
func ObservationRef(o sensorium.Observation) string {
	uid := strings.TrimSpace(o.Str("uid"))
	if uid == "" {
		return ""
	}
	if rv := strings.TrimSpace(o.Str("resource_version")); rv != "" {
		return o.Kind + ":" + uid + "@" + rv
	}
	return o.Kind + ":" + uid
}

// IngestPodObservation maintains the graph from one pod status observation: the
// pod entity with its last status, Pod runs_on Node (closed and reopened when the
// pod moves), Workload owns Pod, and, on a DELETED watch event, every open edge
// touching the pod closed.
func (s *Store) IngestPodObservation(ctx context.Context, o sensorium.Observation) {
	if o.Kind != "pod_status" {
		return
	}
	ts := float64(o.TS.UnixNano()) / 1e9
	// last_seen drives consolidation's stale edge pass.
	pod := s.UpsertEntity(ctx, o.ClusterID, "Pod", o.Name, o.Namespace, map[string]any{"last_status": o.Str("status"), "last_seen": ts})
	if pod == "" {
		return
	}
	if o.Str("watch_type") == "DELETED" {
		s.closeAllEdges(ctx, o.ClusterID, pod)
		return
	}
	if node := o.Str("node"); node != "" {
		if nodeID := s.UpsertEntity(ctx, o.ClusterID, "Node", node, "", nil); nodeID != "" {
			if cur := s.openRunsOnDst(ctx, o.ClusterID, pod); cur != "" && cur != nodeID {
				// The pod moved: the old fact stopped being true at observation time.
				s.CloseEdge(ctx, o.ClusterID, pod, "runs_on", "", ts)
			}
			s.OpenEdge(ctx, o.ClusterID, pod, "runs_on", nodeID, EdgeOpts{SourceID: ObservationRef(o), EventTime: ts})
		}
	}
	if owner := o.Str("owner"); strings.Contains(owner, "/") {
		kind, name, _ := strings.Cut(owner, "/")
		if w := s.UpsertEntity(ctx, o.ClusterID, kind, name, o.Namespace, nil); w != "" {
			s.OpenEdge(ctx, o.ClusterID, w, "owns", pod, EdgeOpts{SourceID: ObservationRef(o), EventTime: ts})
		}
	}
}

// LinkIncident links a pod to an Incident entity: Pod crashed_with Incident.
func (s *Store) LinkIncident(ctx context.Context, clusterID, namespace, pod, incident, rel, sourceID string) {
	if rel == "" {
		rel = "crashed_with"
	}
	p := s.UpsertEntity(ctx, clusterID, "Pod", pod, namespace, nil)
	i := s.UpsertEntity(ctx, clusterID, "Incident", incident, "", nil)
	if p == "" || i == "" {
		return
	}
	s.OpenEdge(ctx, clusterID, p, rel, i, EdgeOpts{SourceKind: "episode", SourceID: sourceID})
}

// Edge is one edge with its endpoints named.
type Edge struct {
	Src   string         `json:"src"`
	Rel   string         `json:"rel"`
	Dst   string         `json:"dst"`
	Attrs map[string]any `json:"attrs"`
}

// Change is an edge that opened or closed inside a window.
type Change struct {
	Change string         `json:"change"` // opened | closed
	At     float64        `json:"at"`
	Src    string         `json:"src"`
	Rel    string         `json:"rel"`
	Dst    string         `json:"dst"`
	Attrs  map[string]any `json:"attrs"`
}

const edgeSelect = `SELECT e.rel, e.attrs, e.valid_from, e.valid_to, s.kind, s.namespace, s.name, d.kind, d.namespace, d.name
	FROM kg_edges e JOIN kg_entities s ON s.id = e.src JOIN kg_entities d ON d.id = e.dst`

type edgeRow struct {
	rel                            string
	attrs                          []byte
	from                           float64
	to                             sql.NullFloat64
	sk, sns, sname, dk, dns, dname string
}

func (s *Store) edgeRows(ctx context.Context, where string, args ...any) ([]edgeRow, error) {
	rows, err := s.DB.QueryContext(ctx, s.DB.Q(edgeSelect+" "+where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []edgeRow
	for rows.Next() {
		var r edgeRow
		if err := rows.Scan(&r.rel, &r.attrs, &r.from, &r.to, &r.sk, &r.sns, &r.sname, &r.dk, &r.dns, &r.dname); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (r edgeRow) edge() Edge {
	return Edge{Src: r.sk + "/" + r.sns + "/" + r.sname, Rel: r.rel, Dst: r.dk + "/" + r.dns + "/" + r.dname, Attrs: parseAttrs(r.attrs)}
}

// Changes returns the edges that opened or closed in (t1, t2], joined to entity
// identities. An edge both opened and closed inside the window yields two rows. An
// empty list means the window held no changes and nothing else; an unreadable graph
// is ErrKGUnavailable.
func (s *Store) Changes(ctx context.Context, clusterID string, t1, t2 float64) ([]Change, error) {
	rows, err := s.edgeRows(ctx, `WHERE e.cluster_id = ? AND ((e.valid_from > ? AND e.valid_from <= ?)
		OR (e.valid_to IS NOT NULL AND e.valid_to > ? AND e.valid_to <= ?)) ORDER BY e.valid_from`, clusterID, t1, t2, t1, t2)
	if err != nil {
		slog.Warn("kg changes failed", "err", err)
		return nil, fmt.Errorf("%w: %v", ErrKGUnavailable, err)
	}
	var out []Change
	for _, r := range rows {
		e := r.edge()
		if r.from > t1 && r.from <= t2 {
			out = append(out, Change{"opened", r.from, e.Src, e.Rel, e.Dst, e.Attrs})
		}
		if r.to.Valid && r.to.Float64 > t1 && r.to.Float64 <= t2 {
			out = append(out, Change{"closed", r.to.Float64, e.Src, e.Rel, e.Dst, e.Attrs})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out, nil
}

// AsOf is the bi temporal point in time query: the edges that, as the agent
// believed at transaction time txT (zero means now), were valid in the world at
// event time validT. It answers "what did we believe at T versus what was true".
func (s *Store) AsOf(ctx context.Context, clusterID string, validT, txT float64) []Edge {
	if txT == 0 {
		txT = s.now()
	}
	rows, err := s.edgeRows(ctx, `WHERE e.cluster_id = ? AND e.valid_from <= ? AND (e.valid_to IS NULL OR e.valid_to > ?)
		AND e.ingested_at <= ? AND (e.retracted_at IS NULL OR e.retracted_at > ?) ORDER BY e.valid_from`, clusterID, validT, validT, txT, txT)
	if err != nil {
		slog.Warn("kg as_of failed", "err", err)
		return nil
	}
	out := make([]Edge, len(rows))
	for i, r := range rows {
		out[i] = r.edge()
	}
	return out
}

// CurrentEdges is the default read: edges currently true and currently believed.
func (s *Store) CurrentEdges(ctx context.Context, clusterID string, limit int) []Edge {
	rows, err := s.edgeRows(ctx, `WHERE e.cluster_id = ? AND e.valid_to IS NULL AND e.retracted_at IS NULL ORDER BY e.valid_from DESC LIMIT ?`, clusterID, limit)
	if err != nil {
		slog.Warn("kg current edges failed", "err", err)
		return nil
	}
	out := make([]Edge, len(rows))
	for i, r := range rows {
		out[i] = r.edge()
	}
	return out
}

// MeanIngestLagSeconds is the freshness signal: the mean of ingested_at minus
// valid_from over edges ingested in the last N minutes. A high lag means the graph
// trails reality. It is meaningful only once valid_from is event time, and returns
// nil when bi-temporality is off, nothing is recent, or on any failure.
func (s *Store) MeanIngestLagSeconds(ctx context.Context, clusterID string, minutes int) *float64 {
	if !s.Cfg.MemoryBitemporal {
		return nil
	}
	var lag sql.NullFloat64
	err := s.DB.QueryRowContext(ctx, s.DB.Q(`SELECT AVG(ingested_at - valid_from) FROM kg_edges WHERE cluster_id = ? AND ingested_at > ?`),
		clusterID, s.now()-float64(minutes)*60).Scan(&lag)
	if err != nil || !lag.Valid {
		if err != nil {
			slog.Warn("kg mean ingest lag failed", "err", err)
		}
		return nil
	}
	return &lag.Float64
}

// RecentChangesBlock renders the last N minutes of changes as a compact prompt
// block: one line per change, capped, with a "(+N more)" tail. It returns "" when
// nothing changed, and only then; an unreadable window is ErrKGUnavailable, because
// the caller renders this into a prompt and "" is already the model's evidence of calm.
func (s *Store) RecentChangesBlock(ctx context.Context, clusterID string, minutes, limit int) (string, error) {
	now := s.now()
	rows, err := s.Changes(ctx, clusterID, now-float64(minutes)*60, now)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	var lines []string
	for i, r := range rows {
		if i >= limit {
			break
		}
		lines = append(lines, fmt.Sprintf("%s %s %s -%s-> %s", time.Unix(int64(r.At), 0).UTC().Format("15:04:05"), r.Change, r.Src, r.Rel, r.Dst))
	}
	if over := len(rows) - limit; over > 0 {
		lines = append(lines, fmt.Sprintf("(+%d more)", over))
	}
	return strings.Join(lines, "\n"), nil
}

// Related is one entity in a blast radius.
type Related struct {
	Entity    string  `json:"entity"`
	Score     float64 `json:"score"`
	RelSample string  `json:"rel_sample"`
}

// pprScores is Personalized PageRank over an undirected bounded subgraph, in
// process. It is row normalised power iteration with restart on the seed set. The
// subgraph is small (a few hundred edges), so it converges quickly without a graph
// library.
func pprScores(edges [][2]string, seeds map[string]bool) map[string]float64 {
	const damping, maxIter, tol = 0.85, 50, 1e-6
	adj := map[string][]string{}
	for _, e := range edges {
		adj[e[0]] = append(adj[e[0]], e[1])
		adj[e[1]] = append(adj[e[1]], e[0])
	}
	nodes := make([]string, 0, len(adj))
	for n := range adj {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	if len(nodes) == 0 {
		return map[string]float64{}
	}
	var seedNodes []string
	for _, n := range nodes {
		if seeds[n] {
			seedNodes = append(seedNodes, n)
		}
	}
	if len(seedNodes) == 0 {
		seedNodes = nodes // fall back to a uniform restart
	}
	restart := 1.0 / float64(len(seedNodes))
	seedSet := map[string]bool{}
	for _, n := range seedNodes {
		seedSet[n] = true
	}
	scores := map[string]float64{}
	for _, n := range nodes {
		if seedSet[n] {
			scores[n] = restart
		}
	}
	for it := 0; it < maxIter; it++ {
		next := map[string]float64{}
		for _, n := range nodes {
			if seedSet[n] {
				next[n] = (1 - damping) * restart
			}
		}
		for _, n := range nodes {
			sc := scores[n]
			if sc == 0 {
				continue
			}
			share := damping * sc / float64(len(adj[n]))
			for _, nb := range adj[n] {
				next[nb] += share
			}
		}
		delta := 0.0
		for _, n := range nodes {
			delta += math.Abs(next[n] - scores[n])
		}
		scores = next
		if delta < tol {
			break
		}
	}
	return scores
}

// PPRBlastRadius ranks the entities most related to the seeds: their multi hop
// blast radius. It loads the currently true, currently believed edges, takes the
// bounded induced subgraph within maxHops of the seeds, and ranks nodes by
// Personalized PageRank. It is empty when the flag is off, there are no seeds, or on
// any failure.
func (s *Store) PPRBlastRadius(ctx context.Context, clusterID string, seeds []string, maxHops, topK, maxEdges int) []Related {
	if !s.Cfg.MemoryKGPPR || len(seeds) == 0 {
		return nil
	}
	type ed struct{ src, dst, rel string }
	rows, err := s.DB.QueryContext(ctx, s.DB.Q(`SELECT src, dst, rel FROM kg_edges WHERE cluster_id = ? AND valid_to IS NULL AND retracted_at IS NULL`), clusterID)
	if err != nil {
		slog.Warn("kg ppr blast radius failed", "err", err)
		return nil
	}
	var all []ed
	for rows.Next() {
		var e ed
		if err := rows.Scan(&e.src, &e.dst, &e.rel); err != nil {
			rows.Close()
			return nil
		}
		all = append(all, e)
	}
	rows.Close()

	reach := map[string]bool{}
	for _, sd := range seeds {
		reach[sd] = true
	}
	for hop := 0; hop < maxHops; hop++ {
		grew := false
		for _, e := range all {
			if reach[e.src] && !reach[e.dst] {
				reach[e.dst], grew = true, true
			}
			if reach[e.dst] && !reach[e.src] {
				reach[e.src], grew = true, true
			}
		}
		if !grew {
			break
		}
	}
	var edges [][2]string
	relOf := map[string]string{}
	for _, e := range all {
		if reach[e.src] && reach[e.dst] {
			edges = append(edges, [2]string{e.src, e.dst})
			if _, ok := relOf[e.dst]; !ok {
				relOf[e.dst] = e.rel
			}
			if _, ok := relOf[e.src]; !ok {
				relOf[e.src] = e.rel
			}
			if len(edges) >= maxEdges {
				break
			}
		}
	}
	if len(edges) == 0 {
		return nil
	}
	label := map[string]string{}
	lr, err := s.DB.QueryContext(ctx, s.DB.Q(`SELECT id, kind, namespace, name FROM kg_entities WHERE cluster_id = ?`), clusterID)
	if err == nil {
		for lr.Next() {
			var id, k, ns, n string
			if lr.Scan(&id, &k, &ns, &n) == nil {
				label[id] = k + "/" + ns + "/" + n
			}
		}
		lr.Close()
	}
	seedSet := map[string]bool{}
	for _, sd := range seeds {
		seedSet[sd] = true
	}
	scores := pprScores(edges, seedSet)
	var ranked []Related
	for id, sc := range scores {
		if seedSet[id] {
			continue
		}
		name := label[id]
		if name == "" {
			name = id
		}
		ranked = append(ranked, Related{name, math.Round(sc*1e6) / 1e6, relOf[id]})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		return ranked[i].Entity < ranked[j].Entity
	})
	if len(ranked) > topK {
		ranked = ranked[:topK]
	}
	return ranked
}
