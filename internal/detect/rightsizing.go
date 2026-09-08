package detect

import (
	"fmt"
	"math"
)

// Evidence grounded rightsizing recommendations. A recommendation is commodity;
// being believed is the product, so this produces a resource limit recommendation
// strictly grounded in observed signals (OOM kills, peak memory against the limit,
// CPU throttling) with an explicit rationale and a confidence, and not a black box
// number. Pure and deterministic.

const (
	headroom     = 1.25 // target peak/limit after a memory bump
	memHigh      = 0.90 // peak/limit above this means under-provisioned
	memLow       = 0.40 // peak/limit below this means over-provisioned
	throttleHigh = 0.25 // CPU throttled in more than 25% of periods means CPU starved
)

// Usage is what was observed for one container.
type Usage struct {
	PeakMemoryBytes   int64
	MemoryLimitBytes  int64
	OOMCount          int
	CPUThrottlePct    float64 // fraction of periods throttled, 0 to 1
	CPULimitMillicore int
}

// Recommendation is a grounded resize.
type Recommendation struct {
	Actions           []string `json:"actions"`
	MemoryLimitBytes  int64    `json:"memory_limit_bytes"` // 0 leaves it unchanged
	CPULimitMillicore int      `json:"cpu_limit_millicores"`
	Rationale         []string `json:"rationale"`
	Confidence        float64  `json:"confidence"`
	// Assessed says whether the signals were sufficient to judge at all. IsNoop
	// cannot tell "assessed and healthy" from "we have no observation of this
	// container", and both used to render the identical sentence at the identical
	// confidence, as did a container with no memory limit whatsoever. A recommender
	// whose silence is ambiguous is worse than one that says nothing: a no-op reads
	// as an all-clear.
	Assessed bool `json:"assessed"`
}

func (r Recommendation) IsNoop() bool { return len(r.Actions) == 0 }

// Recommend grounds a resize in the observed signals. It is a no-op when nothing
// warrants a change.
func Recommend(u Usage) Recommendation {
	var actions, rationale []string
	var memLimit int64
	cpuLimit := 0
	conf := 0.5

	// An absent limit is not a ratio of zero: it is the absence of a ratio, and zero
	// is the single most reassuring value the scale has.
	hasLimit := u.MemoryLimitBytes > 0
	ratio := 0.0
	if hasLimit {
		ratio = float64(u.PeakMemoryBytes) / float64(u.MemoryLimitBytes)
	}
	// Likewise a peak of zero is "we never observed this container", not "it used nothing".
	observed := u.PeakMemoryBytes > 0
	bump := int64(float64(u.PeakMemoryBytes) * headroom)

	switch {
	case u.OOMCount > 0:
		memLimit = bump
		verb := "set"
		act := "set_memory_limit"
		if hasLimit {
			verb, act = "raise", "increase_memory"
		}
		actions = append(actions, act)
		rationale = append(rationale, fmt.Sprintf("%d OOMKill(s) observed; %s memory limit to ~%gx peak", u.OOMCount, verb, headroom))
		conf = 0.9
	case !observed:
		extra := ""
		if !hasLimit {
			extra = ", and it has no memory limit set"
		}
		rationale = append(rationale, "no peak-memory observation for this container — nothing here is a statement about whether its limits are right"+extra)
	case !hasLimit:
		// Unbounded, with a peak to size against. This is the highest risk memory
		// configuration there is, since one leak evicts every pod on the node, and it
		// used to land in the "healthy" bucket because 0.0 is below every threshold.
		memLimit = bump
		actions = append(actions, "set_memory_limit")
		rationale = append(rationale, fmt.Sprintf("no memory limit is set; the container is unbounded and can evict its node. Observed peak %d B — set a limit at ~%gx peak", u.PeakMemoryBytes, headroom))
		conf = 0.7
	case ratio >= memHigh:
		memLimit = bump
		actions = append(actions, "increase_memory")
		rationale = append(rationale, fmt.Sprintf("peak/limit %.0f%% ≥ %.0f%%; under-provisioned, bump before OOM", ratio*100, memHigh*100))
		conf = 0.75
	case ratio <= memLow:
		memLimit = bump
		actions = append(actions, "decrease_memory")
		rationale = append(rationale, fmt.Sprintf("peak/limit %.0f%% ≤ %.0f%%; over-provisioned, rightsize down", ratio*100, memLow*100))
		conf = 0.6
	}

	if u.CPUThrottlePct >= throttleHigh && u.CPULimitMillicore > 0 {
		cpuLimit = int(float64(u.CPULimitMillicore) * (1 + u.CPUThrottlePct))
		actions = append(actions, "increase_cpu")
		rationale = append(rationale, fmt.Sprintf("CPU throttled %.0f%% of periods; raise the CPU limit", u.CPUThrottlePct*100))
		conf = math.Max(conf, 0.8)
	}

	// Assessed is a claim about the evidence, not the verdict. Acting proves we had
	// enough; so does an in-band ratio. Nothing else does.
	assessed := len(actions) > 0 || (observed && hasLimit)
	if len(actions) == 0 {
		if assessed {
			rationale = append(rationale, "usage within healthy bands; no resize warranted")
			conf = 0.5
		} else {
			// Deliberately not 0.5: that said "a considered judgement of no change",
			// and there was no judgement because there was nothing to judge.
			conf = 0
		}
	}
	return Recommendation{Actions: actions, MemoryLimitBytes: memLimit, CPULimitMillicore: cpuLimit, Rationale: rationale, Confidence: conf, Assessed: assessed}
}
