package kube

import "regexp"

type Risk string

const (
	RiskNone   Risk = "low"
	RiskMedium Risk = "medium"
	RiskHigh   Risk = "high"
)

// cp reads or writes arbitrary paths inside a container, including mounted
// Secrets, which is the credential block bypassed by another route. debug
// attaches an ephemeral container and, with node/..., gives a privileged pod on
// the node itself.
var highRisk = set("delete", "drain", "replace", "taint", "cp", "debug")

// `rollout` is deliberately absent: `rollout status` and `history` are reads and
// must stay available to a read only key. Its write subcommands are caught by
// IsWrite, which is subcommand aware.
var mediumRisk = set(
	"patch", "apply", "scale", "exec", "cordon", "uncordon", "create", "run", "set",
	"label", "annotate", "expose", "autoscale", "port-forward", "attach", "evict", "certificate",
)

// DestructiveVerbs is highRisk plus mediumRisk.
var DestructiveVerbs = func() map[string]bool {
	m := map[string]bool{}
	for k := range highRisk {
		m[k] = true
	}
	for k := range mediumRisk {
		m[k] = true
	}
	return m
}()

// Verbs whose blast radius is too large to auto approve. They ask for
// confirmation even when the session is on auto approve.
var confirmDeleteTargets = set(
	"namespace", "namespaces", "ns",
	"pv", "persistentvolume", "persistentvolumes",
	"crd", "customresourcedefinition", "customresourcedefinitions",
)

var confirmSetSubcommands = set("image", "resources")

// readOnlyVerbs is an allowlist and the default is write: a verb absent from it
// is a mutation and is gated, so a new kubectl verb, or one nobody thought to
// list, fails closed instead of being waved through as a read.
var readOnlyVerbs = set(
	"get", "describe", "logs", "top", "diff", "explain", "events",
	"version", "cluster-info", "api-resources", "api-versions",
	"wait", "kustomize", "completion", "help", "options",
)

// Verbs whose safety depends on the subcommand: `rollout status` reads, `rollout
// restart` does not. Anything not listed is a write. `auth reconcile` writes
// Roles and bindings from a manifest, so only can-i and whoami are reads.
var readOnlySubcommands = map[string]map[string]bool{
	"rollout":     set("status", "history"),
	"config":      set("view", "get-contexts", "current-context", "get-clusters", "get-users"),
	"certificate": set(),
	"auth":        set("can-i", "whoami"),
}

// kubectl edit needs an interactive terminal that is never available here.
var rejectedVerbs = set("edit")

// Read only against the cluster is not read only against what may be read.
// `cluster-info dump` walks every namespace and prints specs, events and logs,
// so it returns the contents of the namespaces the blocklist withholds, and it
// has no per object shape to filter. Bare `cluster-info` is untouched.
var rejectedSubcommands = map[string]map[string]bool{"cluster-info": set("dump")}

// Pipe (|) is excluded because it is handled in Go, and backslash is excluded
// because it appears in valid jsonpath separators like {"\n"} and is harmless
// with no shell to interpret it.
var shellMeta = regexp.MustCompile("[;&`$<>]")

func indexOf(tokens []string, s string) int {
	for i, t := range tokens {
		if t == s {
			return i
		}
	}
	return -1
}

// IsWrite is true when the command can change something. Unknown verbs count as
// writes.
func IsWrite(verb string, args []string) bool {
	if verb == "" {
		return false
	}
	if subs, ok := readOnlySubcommands[verb]; ok {
		i := len(args)
		if at := indexOf(args, verb); at >= 0 {
			i = skipFlags(args, at+1)
		}
		sub := ""
		if i < len(args) {
			sub = args[i]
		}
		return !subs[sub]
	}
	return !readOnlyVerbs[verb]
}

// Classify returns the risk tier. Fail closed: an unrecognised verb that is not
// a known read is a write we have not classified, and unclassified must not mean
// free.
func Classify(verb string, args []string) Risk {
	switch {
	case highRisk[verb]:
		return RiskHigh
	case mediumRisk[verb]:
		return RiskMedium
	case args != nil && IsWrite(verb, args):
		return RiskMedium
	}
	return RiskNone
}

// AlwaysConfirm is true for actions whose blast radius is too large to auto
// approve. The operand is found by skipping flags, because
// `delete --force namespace shop` is as valid as `delete namespace shop` and this
// is the one gate that fires through auto approve.
func AlwaysConfirm(verb string, args []string) bool {
	if verb == "drain" {
		return true
	}
	op := operandAfterVerb(args)
	if verb == "set" && confirmSetSubcommands[op] {
		return true
	}
	if verb == "delete" && confirmDeleteTargets[splitFirst(op, "/")] {
		return true
	}
	return false
}

func splitFirst(s, sep string) string {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i]
		}
	}
	return s
}

// DestructiveVerbsIn returns every destructive verb appearing as a bare token
// anywhere in the command. Defence in depth, independent of position: the parse
// in ExtractVerb is the right one, and this survives a parse we did not
// anticipate. Whole tokens only, so `-l app=delete` does not trip it.
func DestructiveVerbsIn(tokens []string) map[string]bool {
	out := map[string]bool{}
	for i, tok := range tokens {
		if i > 0 && DestructiveVerbs[tok] {
			out[tok] = true
		}
	}
	return out
}

// ExitIsAnAnswer is true when kubectl used a non-zero exit to answer rather than
// to fail: `diff` exits 1 when it finds differences, and `auth can-i` exits 1
// for "no". Only the listed code counts; a higher one is still an error.
func ExitIsAnAnswer(verb string, args []string, code int) bool {
	switch {
	case verb == "diff":
		return code == 1
	case verb == "auth" && operandAfterVerb(args) == "can-i":
		return code == 1
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
