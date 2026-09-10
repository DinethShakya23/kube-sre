package memory

import (
	"context"
	"log/slog"
	"strings"
)

// Investigation write back: an investigation is cheap evidence about the world, so
// feed it back. When an investigation observes a failure pattern on a cluster, a
// confirming (cluster)-[exhibits]->(pattern) edge is reconciled through the same
// path as any other write, and the graph corrects itself over time.

// EdgeSignal is one confirm or contradict signal.
type EdgeSignal struct {
	Src, Rel, Dst string
	Verdict       string // confirm | contradict
	Attrs         map[string]any
}

// SignalsFromInvestigation derives confirm signals for the patterns an
// investigation matched on this cluster.
func SignalsFromInvestigation(clusterID string, playbooks []string) []EdgeSignal {
	seen := map[string]bool{}
	var out []EdgeSignal
	for _, pb := range playbooks {
		name := strings.TrimSpace(pb)
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, EdgeSignal{Src: clusterID, Rel: "exhibits", Dst: name, Verdict: "confirm"})
		}
	}
	return out
}

// ApplyWriteback reconciles each signal and returns a tally of the decisions. It
// never fails the caller. Signals name their ends, so the entities are created
// first: an edge needs entity ids, and an unresolved name would break the foreign key.
func (s *Store) ApplyWriteback(ctx context.Context, clusterID string, signals []EdgeSignal) map[string]int {
	tally := map[string]int{}
	for _, sig := range signals {
		attrs := map[string]any{}
		for k, v := range sig.Attrs {
			attrs[k] = v
		}
		if sig.Verdict == "contradict" {
			attrs["investigation_contradicted"] = true
		} else {
			attrs["investigation_confirmed"] = true
		}
		src := s.UpsertEntity(ctx, clusterID, "Cluster", sig.Src, "", nil)
		dst := s.UpsertEntity(ctx, clusterID, "FailurePattern", sig.Dst, "", nil)
		if src == "" || dst == "" {
			slog.Warn("writeback could not resolve entities", "src", sig.Src, "dst", sig.Dst)
			tally["ERROR"]++
			continue
		}
		tally[s.ReconcileEdge(ctx, clusterID, src, sig.Rel, dst, EdgeOpts{Attrs: attrs, SourceKind: "investigation"}, nil)]++
	}
	return tally
}
