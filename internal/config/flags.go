package config

import (
	"fmt"
	"sort"
	"strings"
)

// Version identity answers "which build am I and what is active": the arm (the
// architecture generation), the software version, and the experimental flags that are
// on. Every experimental slice is default off, so two builds at the same version
// behave identically until flags flip, which makes the active flag set part of the
// runtime identity and not only the number.

// Flag is one experimental setting as loaded.
type Flag struct {
	Env     string
	Bool    bool
	Default any
	Value   any
}

var experimentalPrefixes = []string{"KI_V5_", "MEMORY_", "CORTEX_V4", "CORTEX_V5"}

func isExperimental(env string) bool {
	for _, p := range experimentalPrefixes {
		if strings.HasPrefix(env, p) {
			return true
		}
	}
	return false
}

// UnwiredExperimental are settings that are declared and documented but read by no
// code. Reporting one as active is a false statement about the running system: the
// operator set a switch, the product answered that a feature is on, and nothing
// changed, and the status surface is what an operator uses to confirm a rollout. So
// they are excluded from the active set and reported separately.
//
// This set may only shrink. TestFlagWiring verifies it against the source: a flag
// that is read must not be listed, and a flag that is listed must not be read.
var UnwiredExperimental = map[string]bool{
	"KI_V5_ACI_MUTATING_VERBS":         true,
	"KI_V5_ACI_READ_VERBS_ENABLED":     true,
	"KI_V5_AGENTIC_WORKLOAD_DETECTOR":  true,
	"KI_V5_AGENT_COST_RATE_CAP":        true,
	"KI_V5_AGENT_TOOL_RATE_CAP":        true,
	"KI_V5_AIRGAP_FLOOR":               true,
	"KI_V5_BLAST_RADIUS_BUDGET":        true,
	"KI_V5_CAPABILITY_SANDBOX":         true,
	"KI_V5_DETECTOR_MIN_FIRINGS":       true,
	"KI_V5_DETECTOR_PRECISION_THETA":   true,
	"KI_V5_FAILURE_DOMAIN_BUDGET":      true,
	"KI_V5_FLEET_EXCHANGE":             true,
	"KI_V5_FLEET_PATTERN_MIN_CLUSTERS": true,
	"KI_V5_FLEET_SIGNAL_POOLING":       true,
	"KI_V5_GPU_HEALTH_DETECTOR":        true,
	"KI_V5_MAX_UNAVAILABLE_PER_ZONE":   true,
	"KI_V5_MODEL_ROUTING":              true,
	"KI_V5_NL_DETECTOR_LADDER":         true,
	"KI_V5_OFFLINE_SHADOW_WEIGHT":      true,
	"KI_V5_OTEL_SPANS_ENABLED":         true,
	"KI_V5_RIGHTSIZING":                true,
	"KI_V5_SPEND_IN_PRICE_PER_1K":      true,
	"KI_V5_SPEND_OUT_PRICE_PER_1K":     true,
	"KI_V5_STAGED_PROPAGATION":         true,
	"KI_V5_STAGE_SIZE":                 true,
	"KI_V5_STAGE_WINDOW_SECONDS":       true,
}

// hierarchyDependent flags are wired, do not carry the MEMORY_ prefix, and still cannot
// act unless the memory hierarchy is ready: every one of their consumers reads through it.
var hierarchyDependent = map[string]bool{"KI_V5_STATISTICAL_PROMOTION": true}

func (c *Config) onBooleans() map[string]bool {
	out := map[string]bool{}
	for _, f := range c.Flags {
		if v, ok := f.Value.(bool); ok && f.Bool && v {
			out[f.Env] = true
		}
	}
	return out
}

// movedKnobs are the non boolean experimental settings moved off their declared
// default. Truthiness is the wrong test for a knob: a cap of 0 is a deliberate setting
// and a stage size of 1 is already the default, so only the default separates "the
// operator asked for this" from "nobody touched it".
func (c *Config) movedKnobs() map[string]bool {
	out := map[string]bool{}
	for _, f := range c.Flags {
		if !f.Bool && fmt.Sprint(f.Value) != fmt.Sprint(f.Default) {
			out[f.Env] = true
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ActiveFlags are the experimental booleans that are on and wired.
func (c *Config) ActiveFlags() []string {
	on := c.onBooleans()
	for k := range on {
		if UnwiredExperimental[k] {
			delete(on, k)
		}
	}
	return sortedKeys(on)
}

// SetButUnwired are settings the operator changed that no code reads, switches and
// knobs both, so a silently ignored setting is not how an operator ends up believing a
// slice is live during a rollout. A dead cost cap is the one that matters: it reads as
// a spend brake and is the quietest of them.
func (c *Config) SetButUnwired() []string {
	out := map[string]bool{}
	for k := range c.onBooleans() {
		if UnwiredExperimental[k] {
			out[k] = true
		}
	}
	for k := range c.movedKnobs() {
		if UnwiredExperimental[k] {
			out[k] = true
		}
	}
	return sortedKeys(out)
}

// DegradedFlags are on, wired flags whose subsystem is not running right now: the
// flag is read by code, inside a subsystem that is dead, so the operator set a switch,
// was told a slice is on, and nothing changed. Every MEMORY_ slice lives inside the
// memory hierarchy, so when it is not ready none of them can act, including the case
// where a slice is on and the hierarchy is off.
//
// They stay in ActiveFlags: that list is rollout identity, which arm this process was
// configured as, and a blip must not make it flap.
func (c *Config) DegradedFlags(memoryState string) []string {
	if memoryState == "ready" {
		return []string{}
	}
	out := map[string]bool{}
	for k := range c.onBooleans() {
		if !UnwiredExperimental[k] && (strings.HasPrefix(k, "MEMORY_") || hierarchyDependent[k]) {
			out[k] = true
		}
	}
	return sortedKeys(out)
}

// VersionLine is the one line form for logs.
func (c *Config) VersionLine(arm, version string) string {
	flags := c.ActiveFlags()
	suffix := " [flags: none — baseline]"
	if len(flags) > 0 {
		suffix = " [flags: " + strings.Join(flags, ", ") + "]"
	}
	if dead := c.SetButUnwired(); len(dead) > 0 {
		suffix += " [set but NOT WIRED, no effect: " + strings.Join(dead, ", ") + "]"
	}
	return fmt.Sprintf("kube-sre %s (%s)%s", arm, version, suffix)
}
