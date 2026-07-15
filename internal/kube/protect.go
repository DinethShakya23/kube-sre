package kube

import (
	"fmt"
	"strings"
)

// Protect holds namespaces and resource types the agent must never touch.
type Protect struct {
	Namespaces map[string]bool
	Resources  map[string]bool
}

var boolShorthands = set("A", "h", "i", "q", "R", "t", "w")

// FlagValue reads a flag in every form kubectl accepts:
// -n x, -nx, -n=x, --namespace x, --namespace=x, and combined groups like -Rn x.
func FlagValue(args []string, short, long, verb string) (string, bool) {
	letter := strings.TrimPrefix(short, "-")
	bools := boolShorthands
	if verb == "logs" {
		bools = set("A", "h", "i", "q", "R", "t", "w", "f", "p")
	}
	for i, a := range args {
		if a == short || a == long {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
		if strings.HasPrefix(a, long+"=") {
			return strings.TrimPrefix(a, long+"="), true
		}
		if strings.HasPrefix(a, "--") || !strings.HasPrefix(a, "-") || len(a) < 2 {
			continue
		}
		group := a[1:]
		for j, r := range group {
			ch := string(r)
			if ch == letter {
				rest := strings.TrimPrefix(group[j+1:], "=")
				if rest != "" {
					return rest, true
				}
				if i+1 < len(args) {
					return args[i+1], true
				}
				return "", true
			}
			if !bools[ch] {
				break
			}
		}
	}
	return "", false
}

func allNamespaces(args []string) bool {
	for _, a := range args {
		if a == "--all-namespaces" || a == "-A" || strings.HasPrefix(a, "--all-namespaces=") {
			return true
		}
	}
	return false
}

// ResourceTypes returns the resource type names a command names, lowercased.
func ResourceTypes(args []string) []string {
	i := skipFlags(args, 0)
	if i >= len(args) {
		return nil
	}
	i = skipFlags(args, i+1)
	if i >= len(args) {
		return nil
	}
	var out []string
	for _, p := range strings.Split(args[i], ",") {
		p = strings.ToLower(p)
		p = strings.SplitN(p, "/", 2)[0]
		p = strings.SplitN(p, ".", 2)[0]
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Check refuses commands that target a protected namespace or resource type.
func (p *Protect) Check(args []string) error {
	verb := Verb(args)
	if ns, ok := FlagValue(args, "-n", "--namespace", verb); ok {
		if p.Namespaces[strings.ToLower(ns)] {
			return fmt.Errorf("[Protected] namespace %q is off limits", ns)
		}
	}
	if IsWrite(args) && allNamespaces(args) {
		return fmt.Errorf("[Protected] writes with --all-namespaces are not allowed")
	}
	switch verb {
	case "get", "describe", "delete", "edit", "patch", "label", "annotate", "logs", "create", "apply":
		for _, r := range ResourceTypes(args) {
			if p.Resources[r] {
				return fmt.Errorf("[Protected] resource type %q is off limits", r)
			}
		}
	}
	return nil
}

// FilterTable drops rows for protected namespaces from a table with a NAMESPACE column.
func (p *Protect) FilterTable(out string) string {
	lines := strings.Split(out, "\n")
	if len(lines) == 0 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "NAMESPACE") {
		return out
	}
	kept := lines[:1]
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) > 0 && p.Namespaces[strings.ToLower(f[0])] {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}
