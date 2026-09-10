package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/agent"
	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/detect"
	"github.com/DinethShakya23/kube-sre/internal/memory"
)

const recheckDelay = 15 * time.Minute

// scheduleRecheck records a "did the autonomous fix hold?" re-check. It never breaks
// the investigation.
func scheduleRecheck(m *memory.Store, cfg *config.Config, cluster func(context.Context) string, f detect.Finding) {
	if !cfg.MemoryProspective {
		return
	}
	ctx := context.Background()
	m.ScheduleRecheck(ctx, cluster(ctx),
		fmt.Sprintf("Re-verify the autonomous fix for '%s' on object '%s' in namespace '%s' still holds.", f.Playbook, f.Object, f.Namespace),
		float64(time.Now().Add(recheckDelay).UnixNano())/1e9, f.Namespace, f.Playbook,
		fmt.Sprintf("recheck:%s:%s:%s", f.Playbook, f.Namespace, f.Object), "", "watchtower")
}

// recheckDispatch is the default verifier: re-read the cluster and grade the
// condition, with no model and no writes. It used to log the row and return "done"
// without looking at anything, so every scheduled re-check closed as a verification
// that had verified nothing. The same scan the coordinator's post fix check uses
// grades it, so the two cannot disagree about what resolved means; lingering
// warnings with healthy pods are not a failure. A failed read is "unverified", which
// is not a grade and leaves the row pending for the next pass.
func recheckDispatch(snap *agent.Snapshotter) memory.DispatchFn {
	return func(ctx context.Context, r memory.Recheck, level string) string {
		nsArgs := []string{"--all-namespaces"}
		if ns := strings.TrimSpace(r.Namespace); ns != "" {
			nsArgs = []string{"-n", ns}
		}
		podsOK, pods, _ := snap.Read(ctx, append([]string{"get", "pods"}, nsArgs...))
		if !podsOK {
			slog.Warn("prospective: cannot verify re-check, the cluster read failed; recording unverified", "id", r.ID, "ns", r.Namespace, "reason", pods)
			return memory.OutcomeUnverified
		}
		eventsOK, events, _ := snap.Read(ctx, append([]string{"get", "events"}, append(nsArgs, "--sort-by=.lastTimestamp", "--field-selector=type=Warning")...))
		issues, _, count := agent.ScanSnapshot(pods, events, podsOK, eventsOK)
		outcome := memory.OutcomeResolved
		if issues {
			outcome = memory.OutcomeStillBroken
		}
		slog.Info("prospective recheck", "cluster", r.ClusterID, "ns", r.Namespace, "level", level, "pods", count, "outcome", outcome, "condition", r.Condition)
		return outcome
	}
}
