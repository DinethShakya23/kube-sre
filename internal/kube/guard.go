package kube

import (
	"errors"
	"fmt"
	"regexp"
)

type Risk string

const (
	RiskNone   Risk = "none"
	RiskMedium Risk = "medium"
	RiskHigh   Risk = "high"
)

var highRisk = set("delete", "drain", "replace", "taint", "cp", "debug")

var mediumRisk = set(
	"patch", "apply", "scale", "exec", "cordon", "uncordon", "create", "run", "set",
	"label", "annotate", "expose", "autoscale", "port-forward", "attach", "evict", "certificate",
)

var readOnlyVerbs = set(
	"get", "describe", "logs", "top", "diff", "explain", "events",
	"version", "cluster-info", "api-resources", "api-versions",
	"wait", "kustomize", "completion", "help", "options",
)

var readOnlySubcommands = map[string]map[string]bool{
	"rollout":     set("status", "history"),
	"config":      set("view", "get-contexts", "current-context", "get-clusters", "get-users"),
	"certificate": set(),
	"auth":        set("can-i", "whoami"),
}

var rejectedVerbs = set("edit")

var rejectedSubcommands = map[string]map[string]bool{
	"cluster-info": set("dump"),
}

var confirmDeleteTargets = set(
	"namespace", "namespaces", "ns",
	"pv", "persistentvolume", "persistentvolumes",
	"crd", "customresourcedefinition", "customresourcedefinitions",
)

var confirmSetSubcommands = set("image", "resources")

var valueFlags = set(
	"-n", "--namespace", "--context", "--kubeconfig", "--cluster", "--user",
	"-s", "--server", "--request-timeout", "--as", "--as-group", "--token",
)

var shellMeta = regexp.MustCompile("[;&`$<>]")

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}

func skipFlags(args []string, i int) int {
	for i < len(args) {
		a := args[i]
		if len(a) == 0 || a[0] != '-' {
			return i
		}
		if valueFlags[a] {
			i += 2
			continue
		}
		i++
	}
	return i
}

func Verb(args []string) string {
	i := skipFlags(args, 0)
	if i < len(args) {
		return args[i]
	}
	return ""
}

func operand(args []string) string {
	i := skipFlags(args, 0)
	if i >= len(args) {
		return ""
	}
	i = skipFlags(args, i+1)
	if i < len(args) {
		return args[i]
	}
	return ""
}

func IsWrite(args []string) bool {
	verb := Verb(args)
	if verb == "" {
		return false
	}
	if subs, ok := readOnlySubcommands[verb]; ok {
		return !subs[operand(args)]
	}
	return !readOnlyVerbs[verb]
}

func Classify(args []string) Risk {
	verb := Verb(args)
	switch {
	case highRisk[verb]:
		return RiskHigh
	case mediumRisk[verb]:
		return RiskMedium
	case IsWrite(args):
		return RiskMedium
	}
	return RiskNone
}

func AlwaysConfirm(args []string) bool {
	switch Verb(args) {
	case "delete":
		return confirmDeleteTargets[operand(args)]
	case "set":
		return confirmSetSubcommands[operand(args)]
	case "drain":
		return true
	}
	return false
}

func Check(args []string) error {
	if len(args) == 0 {
		return errors.New("empty command")
	}
	for _, a := range args {
		if shellMeta.MatchString(a) {
			return fmt.Errorf("unsafe character in argument %q", a)
		}
	}
	verb := Verb(args)
	if rejectedVerbs[verb] {
		return fmt.Errorf("%s needs an interactive terminal and is not supported", verb)
	}
	if subs, ok := rejectedSubcommands[verb]; ok && subs[operand(args)] {
		return fmt.Errorf("%s %s is not allowed", verb, operand(args))
	}
	return nil
}
