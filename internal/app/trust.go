package app

import (
	"context"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/autonomy"
	"github.com/DinethShakya23/kube-sre/internal/memory"
)

// wireTrust connects the write authority brakes to the watchtower and the episode
// store, and hands them to the API for the status surface.
func (a *App) wireTrust() {
	cfg := a.Cfg
	a.Budget = autonomy.NewBudget(cfg)
	a.Outcomes = &autonomy.Outcomes{DB: a.DB, Now: func() float64 { return float64(time.Now().Unix()) / 86400 }}
	a.Watchtower.AutoWritePermitted = func() (bool, string) {
		d := a.Budget.AutoWritePermitted()
		return d.Allow, d.Reason
	}
	a.Watchtower.AutofixRevoked = a.autofixRevoked
	a.Memory.OnEpisode = func(ctx context.Context, id string, in memory.EpisodeInput) {
		if !cfg.V5StatisticalPromotion {
			return
		}
		// The promotion store's production writer. A statistics write never breaks the
		// episode write, let alone the response it hangs off.
		at := in.StartedAt
		if at == 0 {
			at = float64(time.Now().Unix())
		}
		a.Outcomes.RecordAutonomousAttempt(ctx, id, in.TriggerKind, in.Outcome, in.Verified, in.Playbooks, at)
	}
	a.Server.Budget, a.Server.Outcomes = a.Budget, a.Outcomes
}

// autofixRevoked is the A3 brake: has the recorded record taken the watchtower's
// unattended write authority away? Off means A3 is unchanged. If the store cannot be
// read the answer is revoke: a brake whose evidence cannot be read is not a brake, and
// an unreadable source reading as a clean one would hold a class whose agreement has
// collapsed at its rung, silently, by the read failure itself.
func (a *App) autofixRevoked(ctx context.Context) string {
	if !a.Cfg.V5StatisticalPromotion {
		return ""
	}
	reason, err := a.Outcomes.AutofixRevocation(ctx, float64(time.Now().Unix())/86400)
	if err != nil {
		return "outcome store unreadable: " + err.Error()
	}
	return reason
}
