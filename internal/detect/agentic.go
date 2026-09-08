package detect

import (
	"context"
	"fmt"
	"strings"
)

// Agentic workload and GPU health detectors. Agentic workloads (sandbox pools,
// tool call rate and cost, call graphs) and the device plane (ResourceClaim,
// DeviceClass, GPU health) are pure classification over observed metrics, so they
// are testable without a cluster.

// Hit is one detector result.
type Hit struct {
	Kind     string `json:"kind"`     // agent-runaway, sandbox-escape, gpu-unhealthy, ...
	Severity string `json:"severity"` // warning | critical
	Detail   string `json:"detail"`
}

// AgentSignal is the observed agent behaviour.
type AgentSignal struct {
	ToolCallRatePerMin    float64
	CostUSDPerMin         float64
	SandboxEscapeAttempts int
}

// DetectAgentRunaway flags an agent that is looping, burning spend or probing its sandbox.
func DetectAgentRunaway(s AgentSignal, rateCap, costCap float64) *Hit {
	switch {
	case s.SandboxEscapeAttempts > 0:
		return &Hit{"sandbox-escape", "critical", fmt.Sprintf("%d sandbox-escape attempt(s) — contain the agent", s.SandboxEscapeAttempts)}
	case s.CostUSDPerMin > costCap:
		return &Hit{"agent-runaway", "critical", fmt.Sprintf("spend %.2f USD/min > cap %.2f — likely a loop", s.CostUSDPerMin, costCap)}
	case s.ToolCallRatePerMin > rateCap:
		return &Hit{"agent-runaway", "warning", fmt.Sprintf("tool-call rate %.0f/min > cap %.0f", s.ToolCallRatePerMin, rateCap)}
	}
	return nil
}

// GPUSignal is the observed device plane state.
type GPUSignal struct {
	ResourceClaimPending int // claims stuck Pending, no allocatable device
	AllocFailures        int
	ECCErrors            int
	GPUOOM               int
}

// DetectGPUUnhealthy flags device plane trouble that starves agentic and inference workloads.
func DetectGPUUnhealthy(s GPUSignal) *Hit {
	switch {
	case s.ECCErrors > 0:
		return &Hit{"gpu-unhealthy", "critical", fmt.Sprintf("%d GPU ECC/Xid error(s) — hardware fault, cordon the node", s.ECCErrors)}
	case s.GPUOOM > 0:
		return &Hit{"gpu-unhealthy", "warning", fmt.Sprintf("%d GPU OOM event(s)", s.GPUOOM)}
	case s.ResourceClaimPending > 0 || s.AllocFailures > 0:
		return &Hit{"gpu-unschedulable", "warning", fmt.Sprintf("%d pending ResourceClaim(s), %d alloc failure(s) — no allocatable device", s.ResourceClaimPending, s.AllocFailures)}
	}
	return nil
}

const (
	qToolRate    = "sum(rate(ki_agent_tool_calls_total[1m])) * 60"
	qCostRate    = "sum(rate(ki_agent_cost_usd_total[1m])) * 60"
	qEscape      = "sum(ki_agent_sandbox_escape_attempts)"
	qRCPending   = "sum(kube_resourceclaim_status_pending)"
	qAllocFail   = "sum(increase(dra_allocation_failures_total[5m]))"
	qECC         = "sum(increase(nvidia_gpu_ecc_errors_total[5m]))"
	qGPUOOM      = "sum(increase(nvidia_gpu_oom_events_total[5m]))"
	MetricsBlind = "metrics-unavailable"
)

// Scalar answers one query: nil means it could not be answered (unreachable,
// unconfigured, error), 0 means it was answered and the metric is absent or zero.
type Scalar func(promql string) *float64

// ScalarFromSeries adapts a series source: the last value of the first series, 0 for
// an empty answer, nil for a failure.
func ScalarFromSeries(ctx context.Context, src SeriesSource) Scalar {
	return func(q string) *float64 {
		series, err := src.QuerySeries(ctx, q, 0)
		v := 0.0
		if err != nil {
			return nil
		}
		if len(series) > 0 && len(series[0].Values) > 0 {
			v = series[0].Values[len(series[0].Values)-1][1]
		}
		return &v
	}
}

// CollectAndDetect queries the metrics, runs the agentic and GPU predicates and
// returns the hits. Empty means healthy and readable. If any signal could not be
// read, the first hit is metrics-unavailable: the operator is told the detectors are
// blind and not shown an empty list that looks exactly like a clean bill of health.
func CollectAndDetect(scalar Scalar, rateCap, costCap float64) []Hit {
	var unread []string
	read := func(q string) float64 {
		v := scalar(q)
		if v == nil {
			unread = append(unread, q)
			return 0
		}
		return *v
	}
	agent := AgentSignal{ToolCallRatePerMin: read(qToolRate), CostUSDPerMin: read(qCostRate), SandboxEscapeAttempts: int(read(qEscape))}
	gpu := GPUSignal{ResourceClaimPending: int(read(qRCPending)), AllocFailures: int(read(qAllocFail)), ECCErrors: int(read(qECC)), GPUOOM: int(read(qGPUOOM))}

	var hits []Hit
	if len(unread) > 0 {
		// First, because everything after it was decided on signals that may not exist.
		var names []string
		for i, q := range unread {
			if i == 3 {
				break
			}
			if len(q) > 60 {
				q = q[:60]
			}
			names = append(names, q)
		}
		more := ""
		if len(unread) > 3 {
			more = " …"
		}
		hits = append(hits, Hit{MetricsBlind, "warning", fmt.Sprintf("%d of 7 agent/GPU signals could not be read — these detectors are blind, not clear. Unread: %s%s",
			len(unread), strings.Join(names, ", "), more)})
	}
	if h := DetectAgentRunaway(agent, rateCap, costCap); h != nil {
		hits = append(hits, *h)
	}
	if h := DetectGPUUnhealthy(gpu); h != nil {
		hits = append(hits, *h)
	}
	return hits
}
