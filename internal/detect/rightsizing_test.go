package detect

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const mi = 1 << 20

func TestOOMKillsSetTheLimitFromThePeak(t *testing.T) {
	r := Recommend(Usage{PeakMemoryBytes: 400 * mi, MemoryLimitBytes: 512 * mi, OOMCount: 3})
	if r.IsNoop() || r.Actions[0] != "increase_memory" || r.MemoryLimitBytes != int64(float64(400*mi)*1.25) || r.Confidence != 0.9 || !r.Assessed {
		t.Errorf("%+v", r)
	}
	r = Recommend(Usage{PeakMemoryBytes: 400 * mi, OOMCount: 1})
	if r.Actions[0] != "set_memory_limit" || !strings.Contains(r.Rationale[0], "set memory limit") {
		t.Errorf("no limit at all: %+v", r)
	}
}

func TestRatioBands(t *testing.T) {
	high := Recommend(Usage{PeakMemoryBytes: 480 * mi, MemoryLimitBytes: 512 * mi})
	if high.Actions[0] != "increase_memory" || high.Confidence != 0.75 {
		t.Errorf("%+v", high)
	}
	low := Recommend(Usage{PeakMemoryBytes: 100 * mi, MemoryLimitBytes: 512 * mi})
	if low.Actions[0] != "decrease_memory" || low.Confidence != 0.6 {
		t.Errorf("%+v", low)
	}
	ok := Recommend(Usage{PeakMemoryBytes: 300 * mi, MemoryLimitBytes: 512 * mi})
	if !ok.IsNoop() || !ok.Assessed || ok.Confidence != 0.5 || !strings.Contains(ok.Rationale[0], "within healthy bands") {
		t.Errorf("%+v", ok)
	}
}

func TestSilenceIsNeverAnAllClear(t *testing.T) {
	unobserved := Recommend(Usage{MemoryLimitBytes: 512 * mi})
	if !unobserved.IsNoop() || unobserved.Assessed || unobserved.Confidence != 0 || !strings.Contains(unobserved.Rationale[0], "no peak-memory observation") {
		t.Errorf("%+v", unobserved)
	}
	both := Recommend(Usage{})
	if both.Assessed || !strings.Contains(both.Rationale[0], "no memory limit set") {
		t.Errorf("%+v", both)
	}
	unbounded := Recommend(Usage{PeakMemoryBytes: 200 * mi})
	if unbounded.IsNoop() || unbounded.Actions[0] != "set_memory_limit" || !strings.Contains(unbounded.Rationale[0], "unbounded") {
		t.Errorf("an unlimited container is not healthy: %+v", unbounded)
	}
}

func TestCPUThrottling(t *testing.T) {
	r := Recommend(Usage{PeakMemoryBytes: 300 * mi, MemoryLimitBytes: 512 * mi, CPUThrottlePct: 0.5, CPULimitMillicore: 1000})
	if len(r.Actions) != 1 || r.Actions[0] != "increase_cpu" || r.CPULimitMillicore != 1500 || r.Confidence != 0.8 {
		t.Errorf("%+v", r)
	}
	if r := Recommend(Usage{PeakMemoryBytes: 300 * mi, MemoryLimitBytes: 512 * mi, CPUThrottlePct: 0.5}); !r.IsNoop() {
		t.Errorf("no CPU limit to raise: %+v", r)
	}
}

func TestAgentRunaway(t *testing.T) {
	if h := DetectAgentRunaway(AgentSignal{}, 60, 1); h != nil {
		t.Errorf("%+v", h)
	}
	if h := DetectAgentRunaway(AgentSignal{SandboxEscapeAttempts: 2, CostUSDPerMin: 99}, 60, 1); h.Kind != "sandbox-escape" || h.Severity != "critical" {
		t.Errorf("escape outranks spend: %+v", h)
	}
	if h := DetectAgentRunaway(AgentSignal{CostUSDPerMin: 2.5}, 60, 1); h.Kind != "agent-runaway" || h.Severity != "critical" {
		t.Errorf("%+v", h)
	}
	if h := DetectAgentRunaway(AgentSignal{ToolCallRatePerMin: 100}, 60, 1); h.Severity != "warning" || !strings.Contains(h.Detail, "100/min") {
		t.Errorf("%+v", h)
	}
}

func TestGPUHealth(t *testing.T) {
	if DetectGPUUnhealthy(GPUSignal{}) != nil {
		t.Error("healthy")
	}
	if h := DetectGPUUnhealthy(GPUSignal{ECCErrors: 1, GPUOOM: 3}); h.Severity != "critical" || h.Kind != "gpu-unhealthy" {
		t.Errorf("%+v", h)
	}
	if h := DetectGPUUnhealthy(GPUSignal{GPUOOM: 3}); h.Severity != "warning" {
		t.Errorf("%+v", h)
	}
	if h := DetectGPUUnhealthy(GPUSignal{ResourceClaimPending: 2}); h.Kind != "gpu-unschedulable" {
		t.Errorf("%+v", h)
	}
}

func fixed(v float64) Scalar { return func(string) *float64 { return &v } }

func TestCollectorSaysWhenItIsBlind(t *testing.T) {
	if hits := CollectAndDetect(fixed(0), 60, 1); len(hits) != 0 {
		t.Errorf("healthy and readable is empty: %+v", hits)
	}
	hits := CollectAndDetect(func(string) *float64 { return nil }, 60, 1)
	if len(hits) != 1 || hits[0].Kind != MetricsBlind || !strings.Contains(hits[0].Detail, "7 of 7") || !strings.Contains(hits[0].Detail, "blind, not clear") || !strings.HasSuffix(hits[0].Detail, "…") {
		t.Errorf("%+v", hits)
	}
	// Blind first, then whatever the readable signals say.
	hits = CollectAndDetect(func(q string) *float64 {
		if strings.Contains(q, "ecc_errors") {
			return nil
		}
		v := 5.0
		if strings.Contains(q, "cost") {
			v = 3
		}
		if strings.Contains(q, "escape") {
			v = 0
		}
		return &v
	}, 60, 1)
	if len(hits) < 3 || hits[0].Kind != MetricsBlind || hits[1].Kind != "agent-runaway" {
		t.Errorf("%+v", hits)
	}
}

type fakeSeries struct {
	vals map[string][][2]float64
	err  error
}

func (f fakeSeries) QuerySeries(_ context.Context, q string, _ int) ([]Series, error) {
	if f.err != nil {
		return nil, f.err
	}
	if v, ok := f.vals[q]; ok {
		return []Series{{Values: v}}, nil
	}
	return nil, nil
}

func TestScalarFromSeriesKeepsAbsentAndFailedApart(t *testing.T) {
	s := ScalarFromSeries(context.Background(), fakeSeries{vals: map[string][][2]float64{"q": {{1, 2}, {2, 7}}}})
	if v := s("q"); v == nil || *v != 7 {
		t.Errorf("%v", v)
	}
	if v := s("absent"); v == nil || *v != 0 {
		t.Errorf("an answered empty query is zero, not unknown: %v", v)
	}
	if v := ScalarFromSeries(context.Background(), fakeSeries{err: errors.New("down")})("q"); v != nil {
		t.Errorf("a failed query is unknown: %v", v)
	}
}
