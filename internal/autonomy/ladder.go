// Package autonomy decides how much the agent may do on its own, and runs the
// watchtower that acts on detector findings.
//
// Levels:
//
//	A0  observe: detectors fire and findings are logged, with no model investigation
//	A1  investigate: a firing detector opens an autonomous investigation and the
//	    report is published, with no mutations
//	A2  propose: the investigation may propose a fix; running it needs approval
//	A3  auto fix: runs without approval, ONLY for (playbook, namespace) pairs on the
//	    explicit allowlist, and always verified afterwards
//
// Protected namespaces (KUBECTL_BLOCKED_NAMESPACES) are pinned to A0 whatever the
// configuration says: the watchtower never investigates or changes infrastructure
// namespaces on its own.
//
// A cluster scoped object has no namespace, so this model cannot evaluate it: a
// Warning event about a Node or a PersistentVolume arrives with an empty
// namespace. Letting that fall through to the configured default meant an
// allowlist entry like "SomePlaybook/*", a glob this package supports, silently
// made Nodes auto-fixable, the object where an unattended remediation (cordon,
// drain, delete) is least recoverable. An unattributable namespace is capped at A1:
// investigate and report, never change anything.
package autonomy

import (
	"path"
	"strings"

	"github.com/DinethShakya23/kube-sre/internal/config"
)

var order = []string{"A0", "A1", "A2", "A3"}

func rank(level string) int {
	for i, l := range order {
		if l == level {
			return i
		}
	}
	return -1
}

// normalise matches how the kubectl tool compares namespaces, so the two cannot disagree.
func normalise(ns string) string { return strings.ToLower(strings.TrimSpace(ns)) }

func parseOverrides(raw string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		ns, level, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		if level = strings.TrimSpace(level); rank(level) >= 0 {
			out[normalise(ns)] = level
		}
	}
	return out
}

// capLevel returns the more restrictive of two levels.
func capLevel(level, ceiling string) string {
	if rank(level) <= rank(ceiling) {
		return level
	}
	return ceiling
}

// Ladder resolves autonomy levels from configuration.
type Ladder struct{ Cfg *config.Config }

// LevelFor is the effective autonomy level for a namespace.
func (l Ladder) LevelFor(namespace string) string {
	ns := normalise(namespace)
	if l.Cfg.BlockedNamespaces[ns] {
		return "A0"
	}
	level, ok := parseOverrides(l.Cfg.AutonomyNsLevels)[ns]
	if !ok {
		level = l.Cfg.AutonomyLevel
	}
	if rank(level) < 0 {
		level = "A1"
	}
	if ns == "" {
		// Not a flat "A1": a deployment that pinned everything to A0 stays at A0.
		return capLevel(level, "A1")
	}
	return level
}

// AtLeast reports whether level is at or above floor.
func AtLeast(level, floor string) bool { return rank(level) >= rank(floor) }

// A3Allowed is true iff (playbook, namespace) is explicitly allowlisted for auto
// fix. The allowlist is "playbook/namespace" entries, and the namespace part
// supports glob patterns such as dev-*.
func (l Ladder) A3Allowed(playbook, namespace string) bool {
	if normalise(namespace) == "" {
		return false // never auto fix an object the namespace model cannot evaluate
	}
	if l.LevelFor(namespace) != "A3" {
		return false
	}
	for _, entry := range strings.Split(l.Cfg.AutonomyA3Allowlist, ",") {
		entry = strings.TrimSpace(entry)
		pb, pattern, ok := strings.Cut(entry, "/")
		if entry == "" || !ok {
			continue
		}
		if strings.TrimSpace(pb) != playbook {
			continue
		}
		if m, err := path.Match(strings.TrimSpace(pattern), normalise(namespace)); err == nil && m {
			return true
		}
	}
	return false
}
