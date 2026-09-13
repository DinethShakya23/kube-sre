package app

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/aci"
	"github.com/DinethShakya23/kube-sre/internal/change"
	"github.com/DinethShakya23/kube-sre/internal/cortex"
	"github.com/DinethShakya23/kube-sre/internal/detect"
	"github.com/DinethShakya23/kube-sre/internal/kube"
	"github.com/DinethShakya23/kube-sre/internal/llm"
	"github.com/DinethShakya23/kube-sre/internal/memory"
)

// aciRunner is the one place the ACI verbs touch the cluster: the guarded kubectl tool,
// as a read only identity, with approval denied. The verbs only build read commands, and
// the tool would refuse a write for this role anyway.
func aciRunner(t *kube.Tool) aci.Runner {
	return func(ctx context.Context, command, stdin string) string {
		out, err := t.Run(ctx, command, stdin, kube.Call{Role: "readonly"}, nil)
		if err != nil {
			return "[Error] " + err.Error()
		}
		return out
	}
}

// investigator runs a read only investigation through the harness and returns its
// evidence bundle. It is what the change watchdogs and predictive fusion dispatch to.
type investigator struct {
	model  llm.Model
	verbs  aci.Verbs
	rounds int
}

func (i investigator) investigate(ctx context.Context, objective string) (string, bool) {
	c, err := cortex.NewContract(objective)
	if err != nil {
		return "", false
	}
	bundle, _, usable := cortex.Fanout(ctx, i.model, i.verbs, []cortex.Contract{c}, "", i.rounds)
	return bundle, usable
}

// wireCortex connects the opt in features that live outside the agent: the change
// watchdogs, predictive fusion and predictive pre capture. Each is inert unless
// CORTEX_V5_ENABLED and its own flag are set.
func (a *App) wireCortex(model llm.Model, verbs aci.Verbs, cluster func(context.Context) string, ledger *change.Ledger) {
	cfg := a.Cfg
	inv := investigator{model: model, verbs: verbs, rounds: cfg.V5HarnessMaxRounds}
	run := func(t cortex.WatchdogTask) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(t.TTLSeconds)*time.Second)
			defer cancel()
			bundle, ok := inv.investigate(ctx, t.Objective)
			slog.Info("watchdog investigation finished", "target", t.Target, "kind", t.Kind, "evidence", ok, "chars", len(bundle))
			if ok && a.Recorder != nil {
				a.Recorder.Record("watchdog:"+t.DedupKey, "watchdog_investigation", map[string]any{"target": t.Target, "kind": t.Kind, "objective": t.Objective, "evidence": bundle})
			}
		}()
	}
	dogs := cortex.NewWatchdogs()
	dogs.SetDispatch(run)
	if cfg.CortexV5 && cfg.V5ChangeWatchdog {
		a.Consolidator.Extra = append(a.Consolidator.Extra, memory.Pass{Name: "watchdogs_fired", Run: func(ctx context.Context) int {
			return dogs.Sweep(ledger.Recent(cluster(ctx), ""), float64(time.Now().Unix()), cfg.V5WatchdogTTLSeconds, cfg.V5WatchdogMaxActive)
		}})
	}
	a.Perception.OnFinding = func(f detect.Finding) {
		if cfg.CortexV5 && cfg.V5PredictivePrecapture {
			if plan := cortex.PlanPreCapture(f, cfg.V5PrecaptureETAMin, nil); plan != nil {
				slog.Info("watchtower: pre-capture armed", "ns", plan.Namespace, "target", plan.Target, "actions", strings.Join(plan.Actions, ","), "reason", plan.Reason)
			}
		}
		if cfg.CortexV5 && cfg.V5PredictiveFusion && cortex.IsPrediction(f) {
			run(cortex.PredictionTask(f, cfg.V5WatchdogTTLSeconds))
		}
		a.Watchtower.OnFinding(f)
	}
}
