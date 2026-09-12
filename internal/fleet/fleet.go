// Package fleet is cross cluster learning inside one tenant: a resolution learned on
// one cluster becomes available to its siblings, and the same signal on many clusters
// becomes a fleet incident.
//
// The load bearing invariant is strict tenant isolation: a cluster in tenant A must
// never read tenant B's fleet knowledge. It is enforced at the read boundary, by the
// tenant predicate on every query and by partitioning in memory, and not left to callers.
package fleet

import (
	"context"
	"sort"
	"sync"

	"github.com/DinethShakya23/kube-sre/internal/store"
)

var Migrations = []store.Migration{{
	Version: 46, Name: "fleet",
	SQL: `
CREATE TABLE IF NOT EXISTS fleet_memory (
    id         {{PK}},
    tenant     TEXT NOT NULL,
    cluster_id TEXT NOT NULL,
    signature  TEXT NOT NULL,
    summary    TEXT NOT NULL,
    created_at DOUBLE PRECISION NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_fleet_memory_tenant ON fleet_memory (tenant, signature);
CREATE TABLE IF NOT EXISTS fleet_signals (
    id         {{PK}},
    tenant     TEXT NOT NULL,
    cluster_id TEXT NOT NULL,
    kind       TEXT NOT NULL,
    severity   TEXT NOT NULL DEFAULT 'warning',
    created_at DOUBLE PRECISION NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_fleet_signals_tenant ON fleet_signals (tenant, created_at);`,
}}

// Entry is one shareable resolution.
type Entry struct {
	Tenant    string `json:"tenant"`
	ClusterID string `json:"cluster_id"`
	Signature string `json:"signature"` // the pattern or theme key, like "OOMKilled|payments"
	Summary   string `json:"summary"`
}

const maxPerTenant = 500

// Exchange is the in process, bounded, tenant partitioned store. Losing it degrades
// to per cluster memory, never to a cross tenant leak.
type Exchange struct {
	mu sync.Mutex
	by map[string][]Entry
}

func NewExchange() *Exchange { return &Exchange{by: map[string][]Entry{}} }

// Publish contributes a resolution to its tenant's knowledge.
func (x *Exchange) Publish(e Entry) {
	x.mu.Lock()
	defer x.mu.Unlock()
	xs := append(x.by[e.Tenant], e)
	if len(xs) > maxPerTenant {
		xs = xs[len(xs)-maxPerTenant:]
	}
	x.by[e.Tenant] = xs
}

// Read returns a tenant's knowledge, and only that tenant's. excludeCluster drops
// the reader's own entries and a non empty signature filters.
func (x *Exchange) Read(tenant, excludeCluster, signature string) []Entry {
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []Entry
	for _, e := range x.by[tenant] {
		if (excludeCluster == "" || e.ClusterID != excludeCluster) && (signature == "" || e.Signature == signature) {
			out = append(out, e)
		}
	}
	return out
}

// Tenants lists the tenants that have published.
func (x *Exchange) Tenants() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := make([]string, 0, len(x.by))
	for t := range x.by {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Signal is one cluster's detector or runaway signal.
type Signal struct {
	Tenant, ClusterID, Kind, Severity string
}

// Alert is a pattern seen on enough distinct clusters of one tenant.
type Alert struct {
	Tenant           string   `json:"tenant"`
	Kind             string   `json:"kind"`
	AffectedClusters []string `json:"affected_clusters"`
	Severity         string   `json:"severity"`
}

func (a Alert) ClusterCount() int { return len(a.AffectedClusters) }

// DetectPatterns raises an Alert per (tenant, kind) seen on at least minClusters
// distinct clusters. Signals are grouped by (tenant, kind), so an alert never spans
// tenants. Severity escalates to critical if any contributing signal was critical.
func DetectPatterns(signals []Signal, minClusters int) []Alert {
	type key struct{ tenant, kind string }
	clusters := map[key]map[string]bool{}
	critical := map[key]bool{}
	for _, s := range signals {
		k := key{s.Tenant, s.Kind}
		if clusters[k] == nil {
			clusters[k] = map[string]bool{}
		}
		clusters[k][s.ClusterID] = true
		if s.Severity == "critical" {
			critical[k] = true
		}
	}
	out := []Alert{}
	for k, set := range clusters {
		if len(set) < minClusters {
			continue
		}
		var cl []string
		for c := range set {
			cl = append(cl, c)
		}
		sort.Strings(cl)
		sev := "warning"
		if critical[k] {
			sev = "critical"
		}
		out = append(out, Alert{k.tenant, k.kind, cl, sev})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ClusterCount() != out[j].ClusterCount() {
			return out[i].ClusterCount() > out[j].ClusterCount()
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// Store is the durable backing: resolutions and signals survive restarts, and the
// tenant predicate on every read is the isolation boundary.
type Store struct {
	DB  *store.DB
	Now func() float64
}

func (s *Store) now() float64 {
	if s.Now != nil {
		return s.Now()
	}
	return nowSeconds()
}

// Publish persists a resolution to its tenant's knowledge.
func (s *Store) Publish(ctx context.Context, e Entry) error {
	_, err := s.DB.ExecContext(ctx, s.DB.Q(`INSERT INTO fleet_memory (tenant, cluster_id, signature, summary, created_at) VALUES (?, ?, ?, ?, ?)`),
		e.Tenant, e.ClusterID, e.Signature, e.Summary, s.now())
	return err
}

// Read returns a tenant's knowledge, newest first.
func (s *Store) Read(ctx context.Context, tenant, excludeCluster, signature string, limit int) ([]Entry, error) {
	q, args := `SELECT tenant, cluster_id, signature, summary FROM fleet_memory WHERE tenant = ?`, []any{tenant}
	if excludeCluster != "" {
		q += ` AND cluster_id <> ?`
		args = append(args, excludeCluster)
	}
	if signature != "" {
		q += ` AND signature = ?`
		args = append(args, signature)
	}
	rows, err := s.DB.QueryContext(ctx, s.DB.Q(q+` ORDER BY created_at DESC, id DESC LIMIT ?`), append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Tenant, &e.ClusterID, &e.Signature, &e.Summary); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecordSignal persists one cluster's signal into its tenant's stream.
func (s *Store) RecordSignal(ctx context.Context, sig Signal) error {
	sev := sig.Severity
	if sev == "" {
		sev = "warning"
	}
	_, err := s.DB.ExecContext(ctx, s.DB.Q(`INSERT INTO fleet_signals (tenant, cluster_id, kind, severity, created_at) VALUES (?, ?, ?, ?, ?)`),
		sig.Tenant, sig.ClusterID, sig.Kind, sev, s.now())
	return err
}

// RecentSignals returns a tenant's signals within the window (tenant scoped).
func (s *Store) RecentSignals(ctx context.Context, tenant string, windowSeconds float64) ([]Signal, error) {
	rows, err := s.DB.QueryContext(ctx, s.DB.Q(`SELECT tenant, cluster_id, kind, severity FROM fleet_signals WHERE tenant = ? AND created_at >= ?`),
		tenant, s.now()-windowSeconds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Signal
	for rows.Next() {
		var g Signal
		if err := rows.Scan(&g.Tenant, &g.ClusterID, &g.Kind, &g.Severity); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// DetectFromStore reads a tenant's recent signals and raises alerts for
// cross cluster patterns.
func (s *Store) DetectFromStore(ctx context.Context, tenant string, minClusters int, windowSeconds float64) ([]Alert, error) {
	sigs, err := s.RecentSignals(ctx, tenant, windowSeconds)
	if err != nil {
		return nil, err
	}
	return DetectPatterns(sigs, minClusters), nil
}

func nowSeconds() float64 { return float64(timeNow().UnixNano()) / 1e9 }
