// Package helm is run_helm: read only Helm inspection.
//
// It lets the agent see release state, values, manifests and history with no
// risk of mutating the cluster. Layers, in order:
//
//  1. shell metacharacters are refused;
//  2. connection and identity overrides are refused;
//  3. write verbs are refused, and only an allowlist of reads runs;
//  4. protected namespaces are blocked, with the same blocklist kubectl uses;
//  5. Secret documents are stripped from `helm get` output, since `get manifest`
//     renders the chart's own Secrets with their base64 data intact;
//  6. no shell, and an output cap.
//
// "Read only" is enforced against the cluster and also against what may be read.
// Without layers 4 and 5 this tool would answer questions run_kubectl refuses.
package helm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/DinethShakya23/kube-sre/internal/kube"
	"github.com/DinethShakya23/kube-sre/internal/nsguard"
	"github.com/DinethShakya23/kube-sre/internal/policy"
)

const (
	outputCap = 6000
	timeout   = 30 * time.Second
)

var readOnlyVerbs = map[string]bool{
	"list": true, "get": true, "status": true, "history": true, "env": true,
	"version": true, "show": true, "search": true,
}

var writeVerbs = map[string]bool{
	"install": true, "upgrade": true, "rollback": true, "uninstall": true, "delete": true, "repo": true,
	"plugin": true, "dependency": true, "package": true, "push": true, "pull": true, "create": true,
	"lint": true, "template": true,
}

var shellMeta = regexp.MustCompile("[;&`$\\\\|<>]")

// Helm's own connection and identity family, spelled the way Helm spells it.
// --kube-* is matched by prefix because every Helm connection override is.
var connectionFlags = map[string]bool{"--kubeconfig": true, "--registry-config": true, "--repository-config": true}

func connectionFlagIn(tokens []string) string {
	for _, tok := range tokens {
		if !strings.HasPrefix(tok, "-") {
			continue
		}
		name, _, _ := strings.Cut(tok, "=")
		if connectionFlags[name] || strings.HasPrefix(name, "--kube-") {
			return name
		}
	}
	return ""
}

func normalise(command string) string {
	cmd := strings.TrimSpace(command)
	if !strings.HasPrefix(cmd, "helm") {
		cmd = "helm " + cmd
	}
	return cmd
}

var valueFlags = map[string]bool{"-n": true, "--namespace": true, "-o": true, "--output": true, "--kube-context": true, "--kubeconfig": true}

func skipFlags(tokens []string, start int) int {
	i := start
	for i < len(tokens) {
		tok := tokens[i]
		if !strings.HasPrefix(tok, "-") {
			return i
		}
		// `-n prod` consumes its value; `-n=prod`, `-nprod` and `-A` do not.
		if valueFlags[tok] {
			i += 2
		} else {
			i++
		}
	}
	return len(tokens)
}

// extractVerb finds the subcommand past any global flags. A fixed index fails
// closed here, because the check is an allowlist, and that is a usability bug
// rather than a bypass. The same defect behind a denylist would be catastrophic.
func extractVerb(tokens []string) string {
	i := skipFlags(tokens, 1)
	if i < len(tokens) {
		return strings.ToLower(tokens[i])
	}
	return ""
}

// `kind: Secret`, `kind: "Secret"` and `kind: Secret  # managed by ...` are one
// line. A quote is not a kind, and a comment is not part of the value.
var kindRe = regexp.MustCompile(`(?mi)^kind:\s*["']?([A-Za-z0-9.-]+)["']?\s*(?:#.*)?$`)
var docSep = regexp.MustCompile(`(?m)^---\s*$`)

type Tool struct {
	Bin              string
	Blocked          nsguard.Blocklist
	BlockedResources map[string]bool
	blockedExpanded  map[string]bool
}

func New(blocked nsguard.Blocklist, blockedResources map[string]bool) *Tool {
	t := &Tool{Bin: "helm", Blocked: blocked, BlockedResources: blockedResources, blockedExpanded: map[string]bool{}}
	for entry := range blockedResources {
		for s := range kube.ResourceSpellings(entry) {
			t.blockedExpanded[s] = true
		}
	}
	return t
}

// stripBlockedKinds removes YAML documents whose kind is a protected resource.
// Document level removal keeps the rest of the manifest readable, which is the
// point of the tool.
func (t *Tool) stripBlockedKinds(output string) string {
	docs := docSep.Split(output, -1)
	var kept []string
	dropped := 0
	for _, doc := range docs {
		if m := kindRe.FindStringSubmatch(doc); m != nil {
			hit := false
			for s := range kube.ResourceSpellings(m[1]) {
				if t.blockedExpanded[s] {
					hit = true
					break
				}
			}
			if hit {
				dropped++
				continue
			}
		}
		kept = append(kept, doc)
	}
	result := strings.Join(kept, "---")
	if dropped > 0 {
		result += fmt.Sprintf("\n[%d object(s) of a protected kind were removed from this manifest. "+
			"Kubernetes Secrets and ServiceAccount tokens are shielded from inspection to protect cluster credentials.]", dropped)
	}
	return result
}

// A bare json or yaml sequence has no field to carry a notice and no room after
// it, so `helm list -A -o json` returns a short array with nothing marking it
// short. That is a stated limit; the log says so, and the table format can.
func logSilentFilter(dropped int, format string) {
	if dropped > 0 {
		slog.Warn("run_helm withheld releases from a listing that cannot carry the notice; the table format can",
			"releases", dropped, "format", format)
	}
}

func unparseable() string {
	return "[Protected] This release listing could not be parsed, so releases in protected namespaces could not be removed from it."
}

func (t *Tool) filterReleaseNamespaces(output string) string {
	stripped := strings.TrimLeft(output, " \t\r\n")
	nsOf := func(r any) string {
		m, ok := r.(map[string]any)
		if !ok {
			return ""
		}
		return strings.ToLower(fmt.Sprint(orEmpty(m["namespace"])))
	}
	switch {
	case strings.HasPrefix(stripped, "[") || strings.HasPrefix(stripped, "{"):
		var doc any
		if json.Unmarshal([]byte(output), &doc) != nil {
			return unparseable()
		}
		list, ok := doc.([]any)
		if !ok {
			return output
		}
		kept := []any{}
		for _, r := range list {
			if !t.Blocked[nsOf(r)] {
				kept = append(kept, r)
			}
		}
		logSilentFilter(len(list)-len(kept), "json")
		b, _ := json.MarshalIndent(kept, "", "  ")
		return string(b)
	case strings.HasPrefix(stripped, "- "):
		var doc any
		if yaml.Unmarshal([]byte(output), &doc) != nil {
			return unparseable()
		}
		list, ok := doc.([]any)
		if !ok {
			return output
		}
		kept := []any{}
		for _, r := range list {
			if !t.Blocked[nsOf(r)] {
				kept = append(kept, r)
			}
		}
		logSilentFilter(len(list)-len(kept), "yaml")
		b, _ := yaml.Marshal(kept)
		return string(b)
	}
	// Default table: NAME <tab> NAMESPACE <tab> ...
	lines := policy.SplitLines(output)
	var kept strings.Builder
	n := 0
	for i, line := range lines {
		f := strings.Fields(line)
		if i == 0 || len(f) < 2 || !t.Blocked[strings.ToLower(f[1])] {
			kept.WriteString(line)
			n++
		}
	}
	dropped := len(lines) - n
	if dropped > 0 {
		return kept.String() + nsguard.WithheldNote(dropped, "release")
	}
	return kept.String()
}

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// Run is the tool the model calls. It always returns text for the model; a
// problem with the command is worded as the answer.
func (t *Tool) Run(ctx context.Context, command string) string {
	cmd := normalise(command)
	slog.Debug("run_helm", "cmd", cmd)

	if shellMeta.MatchString(cmd) {
		return "[Error] Command contains disallowed shell characters. Use plain helm subcommands without shell operators."
	}
	tokens, err := kube.Split(cmd)
	if err != nil {
		return "[Error] Could not parse command: " + err.Error()
	}
	if flag := connectionFlagIn(tokens); flag != "" {
		slog.Warn("run_helm refused a connection or identity override", "flag", flag, "cmd", cmd)
		return fmt.Sprintf("[Protected] '%s' is not permitted. Which cluster this connects to, and the identity it uses, "+
			"are fixed by the deployment, so they are not part of a query. Ask the question without it.", flag)
	}
	verb := extractVerb(tokens)
	if writeVerbs[verb] {
		return fmt.Sprintf("[Not Allowed] 'helm %s' is a write operation. run_helm only supports read-only inspection "+
			"commands (list, get, status, history, env, version, show, search). To apply Helm changes, ask the user to "+
			"run the command manually.", verb)
	}
	if !readOnlyVerbs[verb] {
		var names []string
		for v := range readOnlyVerbs {
			names = append(names, v)
		}
		sort.Strings(names)
		return fmt.Sprintf("[Not Allowed] 'helm %s' is not a supported subcommand. Supported: %s.", verb, strings.Join(names, ", "))
	}
	// The same blocklist run_kubectl enforces, read with the same parser, so the
	// two tools cannot disagree about which namespaces are off limits.
	if ns := strings.ToLower(kube.FlagValue(tokens, "-n", "--namespace", "")); ns != "" && t.Blocked[ns] {
		slog.Warn("run_helm blocked protected access", "cmd", cmd)
		return fmt.Sprintf("[Protected] Access to namespace '%s' is not permitted. This is an infrastructure namespace.", ns)
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(cctx, t.Bin, tokens[1:]...)
	var so, se bytes.Buffer
	c.Stdout, c.Stderr = &so, &se
	runErr := c.Run()
	code := 0
	if runErr != nil {
		var ee *exec.ExitError
		switch {
		case errors.As(runErr, &ee):
			code = ee.ExitCode()
			if cctx.Err() != nil {
				return "[Error] helm command timed out after 30 seconds."
			}
		case errors.Is(runErr, exec.ErrNotFound) || errors.Is(runErr, os.ErrNotExist):
			return policy.MarkUnavailable("[Error] 'helm' binary not found on PATH. Helm may not be installed in this environment.",
				"helm is not on PATH.")
		default:
			return "[Error] Failed to run helm: " + runErr.Error()
		}
	}
	stdout := strings.ToValidUTF8(so.String(), "�")
	stderr := strings.ToValidUTF8(se.String(), "�")

	// Every `helm get`, not an enumerated pair: `helm -n prod get manifest shop`
	// with the flag first must strip too, and `get hooks` renders manifests. The
	// stripper does nothing to output with no protected kind line.
	// STDOUT ONLY. A filter parses a listing and must never be shown an error:
	// merging stderr in made a routine kubeconfig warning part of the document
	// handed to the JSON parser, so a successful listing and an unreachable
	// cluster returned the same string.
	if verb == "get" {
		stdout = t.stripBlockedKinds(stdout)
	}
	if verb == "list" {
		stdout = t.filterReleaseNamespaces(stdout)
	}
	stdout, detail := strings.TrimSpace(stdout), strings.TrimSpace(stderr)

	var output string
	if code != 0 {
		if detail == "" {
			detail = "(helm wrote nothing to stderr)"
		}
		output = fmt.Sprintf("[helm exited %d] %s", code, detail)
		// helm talks to the same apiserver kubectl does, so it fails the same way
		// and is classified by the same patterns.
		if name, _ := kube.Interpret(detail); kube.IsTerminal(name) {
			output += "\n" + policy.UnavailableNotice("The cluster is not reachable from here.")
		}
		if stdout != "" {
			output += "\n\nhelm also produced this output before or alongside the error. It may be partial, " +
				"and absence from it is NOT evidence:\n" + stdout
		}
	} else {
		switch {
		case stdout != "":
			output = stdout
		case detail != "":
			output = detail
		default:
			output = "(no output)"
		}
		if stdout != "" && detail != "" {
			output += "\n\n[helm also wrote to stderr. helm exited 0, so this is a warning about the client, " +
				"not part of the result:\n" + detail + "]"
		}
	}
	if r := []rune(output); len(r) > outputCap {
		omitted := len(r) - outputCap
		output = string(r[:outputCap]) + "\n" + policy.TruncationMarker(omitted, "chars", "narrow with -n <namespace> or name a single release")
	}
	if output == "" {
		return "(no output)"
	}
	return output
}
