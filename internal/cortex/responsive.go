package cortex

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Responsiveness: never silent progress and latency budgets. The AI path may not
// present as a silent block and must not be slower to first signal than the expert
// kubectl path it replaces.

// Heartbeat calls beat every interval until the returned stop function is called. The
// first beat fires after one full interval, so a fast phase emits nothing. A panic in
// the beat is swallowed: a progress ping must never break the work it wraps.
func Heartbeat(ctx context.Context, interval time.Duration, beat func()) (stop func()) {
	if interval <= 0 {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				func() {
					defer func() { _ = recover() }()
					beat()
				}()
			}
		}
	}()
	return func() { cancel(); wg.Wait() }
}

// PhaseBudget is a deterministic first signal and full investigation latency tracker.
type PhaseBudget struct {
	FirstSignal, Full time.Duration
	Clock             func() time.Time

	start, firstAt time.Time
	marked         bool
}

// NewPhaseBudget starts a budget now.
func NewPhaseBudget(firstSignal, full time.Duration, clock func() time.Time) *PhaseBudget {
	if clock == nil {
		clock = time.Now
	}
	b := &PhaseBudget{FirstSignal: firstSignal, Full: full, Clock: clock}
	b.start = clock()
	return b
}

// MarkFirstSignal records, once, when the first useful result was produced.
func (b *PhaseBudget) MarkFirstSignal() time.Duration {
	if !b.marked {
		b.firstAt, b.marked = b.Clock(), true
	}
	return b.firstAt.Sub(b.start)
}

func (b *PhaseBudget) Elapsed() time.Duration { return b.Clock().Sub(b.start) }

func (b *PhaseBudget) FirstSignalBreached() bool {
	return b.marked && b.firstAt.Sub(b.start) > b.FirstSignal
}

func (b *PhaseBudget) FullBreached() bool { return b.Elapsed() > b.Full }

// Warning is the operator facing budget warning, or "" when within budget.
func (b *PhaseBudget) Warning() string {
	var msgs []string
	if b.FirstSignalBreached() {
		msgs = append(msgs, fmt.Sprintf("first signal %.0fs > %.0fs target", b.MarkFirstSignal().Seconds(), b.FirstSignal.Seconds()))
	}
	if b.FullBreached() {
		msgs = append(msgs, fmt.Sprintf("full investigation %.0fs > %.0fs target", b.Elapsed().Seconds(), b.Full.Seconds()))
	}
	return strings.Join(msgs, "; ")
}

// ── model routing ────────────────────────────────────────────────────────────
//
// Route each request to the cheapest capable tier: a small model for triage and tool
// call formatting, the frontier model for RCA synthesis. When the frontier is
// unreachable (air gapped or disconnected operation) it degrades to a read only triage
// floor on the small model instead of failing. Unsupervised edge writes while
// disconnected are an explicit non goal, so the floor is read only by contract.

const (
	TierSmall    = "small"
	TierFrontier = "frontier"

	TaskTriage       = "triage"
	TaskToolFormat   = "tool_format"
	TaskRCASynthesis = "rca_synthesis"
)

var preferred = map[string]string{TaskTriage: TierSmall, TaskToolFormat: TierSmall, TaskRCASynthesis: TierFrontier}

func want(task string) string {
	if t, ok := preferred[task]; ok {
		return t
	}
	return TierFrontier
}

// RouteTier picks the model tier for a task, falling back to the small tier when the
// frontier a task wants is unreachable. It never fails to route.
func RouteTier(task string, connected bool, frontierReachable *bool) string {
	reach := connected
	if frontierReachable != nil {
		reach = *frontierReachable
	}
	if want(task) == TierFrontier && !reach {
		return TierSmall
	}
	return want(task)
}

// EdgeWriteAllowed: disconnected means no unsupervised writes.
func EdgeWriteAllowed(connected bool) bool { return connected }

// Degraded is true when the task is running below its preferred tier, so the answer
// can disclose it.
func Degraded(task string, connected bool, frontierReachable *bool) bool {
	reach := connected
	if frontierReachable != nil {
		reach = *frontierReachable
	}
	return want(task) == TierFrontier && !reach
}
