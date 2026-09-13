package cortex

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/DinethShakya23/kube-sre/internal/change"
	"github.com/DinethShakya23/kube-sre/internal/detect"
)

// Per change ephemeral watchdogs: a change from the ledger arms a bounded, TTL scoped
// read only investigation that reads the diff and checks whether that specific change
// degraded health. Change conditioned detection and not blanket polling. This file is
// the pure scheduler; dispatch, which runs the investigation through the harness, is a
// pluggable seam.

// WatchdogTask is one armed investigation.
type WatchdogTask struct {
	Target, Kind, Objective string
	CreatedEpoch            float64
	TTLSeconds              int
	DedupKey                string
}

func (t WatchdogTask) Expired(now float64) bool { return now-t.CreatedEpoch > float64(t.TTLSeconds) }

func dedupKey(c change.Record) string { return c.Namespace + "/" + c.Kind + "/" + c.Target }

// PlanWatchdogs arms a watchdog per recent, not yet watched change, most recent first,
// bounded by maxActive. sinceEpoch filters to newer changes and seen dedups against
// watchdogs already armed.
func PlanWatchdogs(changes []change.Record, now float64, ttlSeconds, maxActive int, sinceEpoch float64, seen map[string]bool) []WatchdogTask {
	if seen == nil {
		seen = map[string]bool{}
	}
	var recent []change.Record
	for _, c := range changes {
		if c.TS >= sinceEpoch {
			recent = append(recent, c)
		}
	}
	sort.SliceStable(recent, func(i, j int) bool { return recent[i].TS > recent[j].TS })
	var out []WatchdogTask
	for _, c := range recent {
		key := dedupKey(c)
		if seen[key] {
			continue
		}
		seen[key] = true
		where := ""
		if c.Namespace != "" {
			where = " in " + c.Namespace
		}
		out = append(out, WatchdogTask{Target: c.Target, Kind: c.Kind, DedupKey: key, CreatedEpoch: now, TTLSeconds: ttlSeconds,
			Objective: fmt.Sprintf("A %s to %s%s just happened — verify it did not degrade health (readiness, restarts, events); read the change, don't blanket-scan.", c.Kind, c.Target, where)})
		if len(out) >= maxActive {
			break
		}
	}
	return out
}

// Dispatcher runs one armed task.
type Dispatcher func(WatchdogTask)

// Watchdogs fires armed tasks through a dispatcher. The default only logs.
type Watchdogs struct {
	mu       sync.Mutex
	dispatch Dispatcher
	lastAt   float64
}

func NewWatchdogs() *Watchdogs {
	return &Watchdogs{dispatch: func(t WatchdogTask) { slog.Info("watchdog armed (log-only)", "objective", t.Objective) }}
}

func (w *Watchdogs) SetDispatch(d Dispatcher) {
	w.mu.Lock()
	w.dispatch = d
	w.mu.Unlock()
}

// Fire dispatches each task and returns the count fired. It never panics out.
func (w *Watchdogs) Fire(tasks []WatchdogTask) int {
	w.mu.Lock()
	d := w.dispatch
	w.mu.Unlock()
	n := 0
	for _, t := range tasks {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Warn("watchdog dispatch failed", "target", t.Target, "err", r)
				}
			}()
			d(t)
			n++
		}()
	}
	return n
}

// Sweep arms watchdogs for changes recorded since the last sweep and fires them. It is
// timestamp deduped, so a change is watched once, and it returns the count fired.
func (w *Watchdogs) Sweep(changes []change.Record, now float64, ttlSeconds, maxActive int) int {
	w.mu.Lock()
	since := w.lastAt
	w.lastAt = now
	w.mu.Unlock()
	return w.Fire(PlanWatchdogs(changes, now, ttlSeconds, maxActive, since, nil))
}

// ── predictive fusion ────────────────────────────────────────────────────────

// IsPrediction is true for an anticipatory finding.
func IsPrediction(f detect.Finding) bool { return f.Severity == "predicted" }

// PredictionObjective is a read only investigation objective for a predicted finding.
func PredictionObjective(f detect.Finding) string {
	eta := ""
	if f.ETAMinutes != nil {
		eta = fmt.Sprintf(" (ETA ~%.0fm)", *f.ETAMinutes)
	}
	return fmt.Sprintf("PREDICTED %s for %s in %s%s — investigate the leading indicators NOW (read-only) and report whether it is realizing; "+
		"do not take any mutating action, a prediction never drives a fix.", f.Playbook, f.Object, f.Namespace, eta)
}

// PredictionTask builds a read only investigation task from a predicted finding.
// Predictive findings used to cap at advise only; fusing them launches a bounded read
// only look at the leading indicators now, so the operator has grounded evidence before
// the incident. Never a mutation: a prediction is lower confidence than a realised failure.
func PredictionTask(f detect.Finding, ttlSeconds int) WatchdogTask {
	return WatchdogTask{Target: f.Object, Kind: "predicted", Objective: PredictionObjective(f), TTLSeconds: ttlSeconds,
		DedupKey: fmt.Sprintf("predicted/%s/%s", f.Namespace, f.Object)}
}

// ── predictive pre capture ───────────────────────────────────────────────────

const (
	LogVerbosity   = "raise_log_verbosity"
	CRIUCheckpoint = "arm_criu_checkpoint"
	HeapDump       = "capture_heap_dump"
)

// PreCapturePlan arms recorders before a workload dies, so the post mortem has the
// evidence that would be lost when the pod terminates.
type PreCapturePlan struct {
	Target, Namespace string
	Actions           []string
	Reason            string
}

// PlanPreCapture plans pre capture for an imminent predicted finding, else nil. This
// converts always on high verbosity cost into targeted spend: only a prediction whose
// ETA is within the threshold arms capture, never a realised finding (too late) or a
// far off one (wasteful).
func PlanPreCapture(f detect.Finding, etaThresholdMin float64, actions []string) *PreCapturePlan {
	if f.Severity != "predicted" || f.ETAMinutes == nil || *f.ETAMinutes <= 0 || *f.ETAMinutes > etaThresholdMin {
		return nil
	}
	if len(actions) == 0 {
		actions = []string{LogVerbosity, CRIUCheckpoint}
	}
	return &PreCapturePlan{Target: f.Object, Namespace: f.Namespace, Actions: actions,
		Reason: fmt.Sprintf("predicted %s in ~%.0fm — arm recorders before death so the post-mortem has evidence", f.Playbook, *f.ETAMinutes)}
}
