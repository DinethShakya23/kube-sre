package memory

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/sensorium"
)

// The memory service owns what runs around the store: the observation drain onto the
// graph, the audit chain verifier, and the machine readable answer to "is memory
// alive". observations_dropped is what turns "the knowledge graph is empty" from a
// guess into a fact, since the sensorium keeps producing whether or not anything is
// there to receive.

const (
	ingestFailuresBeforeDegraded = 3
	ingestBackoffMin             = 50 * time.Millisecond
	ingestBackoffMax             = 30 * time.Second
	chainStaleIntervals          = 2.5
)

// Service states.
const (
	StateReady    = "ready"
	StateFlag     = "flag"
	StateDegraded = "degraded"
)

// Service runs the memory background work.
type Service struct {
	Store *Store
	Guard *Guard // nil when write hardening is off
	// ClusterID names the cluster whose audit chain is verified.
	ClusterID func(ctx context.Context) string
	Sleep     func(time.Duration)

	mu             sync.Mutex
	state, reason  string
	dropped        int64
	ingestFailures int64
	queue          chan sensorium.Observation
}

// NewService builds the service. With the hierarchy switched off no queue exists and
// every observation is counted as dropped.
func NewService(s *Store) *Service {
	v := &Service{Store: s, state: StateReady, Sleep: time.Sleep, ClusterID: func(context.Context) string { return "unknown" }}
	if !s.Cfg.MemoryHierarchy {
		v.state, v.reason = StateFlag, "MEMORY_HIERARCHY_ENABLED=false"
		return v
	}
	size := s.Cfg.MemoryObsQueue
	if size < 1 {
		size = 10000
	}
	v.queue = make(chan sensorium.Observation, size)
	return v
}

// Active is whether the hierarchy is running.
func (v *Service) Active() bool { return v.queue != nil }

// Enqueue is the sensorium sink: non blocking, never fails.
func (v *Service) Enqueue(o sensorium.Observation) {
	if o.Kind != "pod_status" {
		return
	}
	if v.queue == nil {
		v.countDropped()
		return
	}
	select {
	case v.queue <- o:
	default:
		// The graph heals from later observations, but the loss is still a loss.
		v.countDropped()
	}
}

func (v *Service) countDropped() {
	v.mu.Lock()
	v.dropped++
	n, state, reason := v.dropped, v.state, v.reason
	v.mu.Unlock()
	if n == 1 || n%1000 == 0 {
		slog.Warn("memory observations discarded", "count", n, "state", state, "reason", reason)
	}
}

// Drain moves observations onto the graph until ctx ends.
//
// A persistent failure used to be retried immediately and forever, so the loop could
// not progress, could not stop and never told anyone, and the state stayed ready
// while every observation was discarded. So: count consecutive failures, back off
// between them, and degrade the reported state once they are clearly not incidental.
func (v *Service) Drain(ctx context.Context) {
	if v.queue == nil {
		return
	}
	backoff := ingestBackoffMin
	for {
		var o sensorium.Observation
		select {
		case <-ctx.Done():
			return
		case o = <-v.queue:
		}
		if err := v.Store.IngestPodObservation(ctx, o); err != nil {
			v.mu.Lock()
			v.ingestFailures++
			n := v.ingestFailures
			if n >= ingestFailuresBeforeDegraded && v.state == StateReady {
				v.state, v.reason = StateDegraded, "observation ingest failing: "+err.Error()
				slog.Error("memory ingest keeps failing, the knowledge graph is no longer being updated", "consecutive", n, "err", err)
			}
			v.mu.Unlock()
			if n == 1 || n%100 == 0 {
				slog.Warn("memory observation ingest error", "consecutive", n, "err", err)
			}
			v.Sleep(backoff)
			if backoff *= 2; backoff > ingestBackoffMax {
				backoff = ingestBackoffMax
			}
			continue
		}
		v.mu.Lock()
		if v.ingestFailures > 0 {
			v.ingestFailures, backoff = 0, ingestBackoffMin
			if v.state == StateDegraded {
				v.state, v.reason = StateReady, ""
				slog.Info("memory observation ingest recovered")
			}
		}
		v.mu.Unlock()
	}
}

// VerifyChainOnce asks the audit chain whether it still verifies and records the
// answer. It records and does not return: /healthz is probed every few seconds while
// verifying reads every audit row, so the surface reports the last answer and its
// age and never derives one on the probe path.
func (v *Service) VerifyChainOnce(ctx context.Context) {
	if v.Guard == nil {
		return
	}
	verdict := v.Guard.Verify(ctx, v.ClusterID(ctx))
	v.Store.Live.RecordChainCheck(verdict.Valid, verdict.Verified, float64(time.Now().UnixNano())/1e9)
	switch {
	case verdict.Verified && !verdict.Valid:
		slog.Error("memory AUDIT CHAIN DOES NOT VERIFY: the recorded rows no longer hash to what they carry, or the chain is shorter than its " +
			"own head anchor. Treat memory derived answers from this cluster as untrusted until reviewed.")
	case !verdict.Verified:
		slog.Info("memory audit chain could not be verified this pass (not a tamper signal)")
	}
}

// VerifyChainLoop verifies once at startup, so a database edited while the process
// was down is reported before anything is served, then every interval. An interval
// of 0 keeps the startup pass and skips the schedule.
func (v *Service) VerifyChainLoop(ctx context.Context, interval time.Duration) {
	v.VerifyChainOnce(ctx)
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			v.VerifyChainOnce(ctx)
		}
	}
}

// chainStaleAfter is when a recorded verdict stops describing the store as it is
// now. Zero means the periodic verifier is off, where a single startup verdict is the
// only one there will be and calling it stale would report a fault the operator chose.
func (v *Service) chainStaleAfter() float64 {
	if v.Store.Cfg.MemoryChainVerifyS <= 0 {
		return 0
	}
	return float64(v.Store.Cfg.MemoryChainVerifyS) * chainStaleIntervals
}

// Status is the block reported on /healthz. The counters and symptoms answer what
// "enabled" cannot: whether memory is doing anything, and not merely reachable.
// healthy is the boolean a probe should key on.
func (v *Service) Status() map[string]any {
	v.mu.Lock()
	state, reason, dropped, failures := v.state, v.reason, v.dropped, v.ingestFailures
	v.mu.Unlock()
	now := float64(time.Now().UnixNano()) / 1e9
	chain := v.Store.Live.ChainStatus(v.Store.Cfg.MemorySecurity, now, v.chainStaleAfter())
	symptoms := v.Store.Live.Symptoms(state, int(dropped), chain)
	out := map[string]any{
		"enabled": state == StateReady, "state": state, "reason": reason,
		"observations_dropped": dropped, "ingest_failures": failures, "observation_backlog": len(v.queue),
		"chain": chain, "symptoms": symptoms,
		"healthy": (state == StateReady || state == StateFlag) && len(symptoms) == 0,
	}
	for k, n := range v.Store.Live.Counters() {
		out[k] = n
	}
	return out
}
