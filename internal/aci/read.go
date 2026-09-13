// Package aci is the agent-computer interface for Kubernetes: bounded, normalised,
// never silent verbs over the single kubectl seam, so they inherit its injection guard,
// protected namespace block, secret redaction and recorder rows. It holds the
// read only verbs, the mutation chokepoint, the transactional executor with its
// postcondition oracle, the capability sandbox and the misconfiguration fix flow.
package aci

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/DinethShakya23/kube-sre/internal/kube"
	"github.com/DinethShakya23/kube-sre/internal/llm"
)

// Runner is the one place a verb touches the cluster: it runs a kubectl command
// through the guarded tool and returns its text. It is a seam so tests need no cluster.
type Runner func(ctx context.Context, command, stdin string) string

// ReadVerbs are the four verbs an investigation subagent may call.
var ReadVerbs = []string{"inspect", "search", "logs", "diff_change"}

// ReadVerbAllowlist is exported regardless of any flag, so the harness can constrain a
// read only subagent to exactly these verbs.
func ReadVerbAllowlist() map[string]bool {
	m := map[string]bool{}
	for _, v := range ReadVerbs {
		m[v] = true
	}
	return m
}

// ContractError is a programming error: a verb built a mutating command. It is never
// surfaced to the cluster or the model.
type ContractError struct{ Msg string }

func (e *ContractError) Error() string { return e.Msg }

// Health is a coarse reading of an object's state.
type Health string

const (
	Current     Health = "Current"
	InProgress  Health = "InProgress"
	Failed      Health = "Failed"
	Terminating Health = "Terminating"
	Unknown     Health = "Unknown"
)

// Result is what a verb produced. Render is the bounded string the model sees and is
// never empty.
type Result struct {
	Verb, Target, Command string
	OK, Empty             bool
	Health                Health
	TotalLines, Shown     int
	Cursor                *int
	Body, Error           string
}

func (r Result) Render() string {
	switch {
	case r.Error != "":
		return fmt.Sprintf("[aci:%s] %s — FAILED\n%s", r.Verb, r.Target, r.Error)
	case r.Empty:
		return fmt.Sprintf("[aci:%s] %s — ran successfully, no matching results.", r.Verb, r.Target)
	}
	header := fmt.Sprintf("[aci:%s] %s", r.Verb, r.Target)
	if r.Health != "" {
		header += " — health=" + string(r.Health)
	}
	if r.Cursor != nil {
		header += fmt.Sprintf(" — showing %d/%d lines (next offset %d)", r.Shown, r.TotalLines, *r.Cursor)
	}
	return header + "\n" + r.Body
}

// Limits bound a verb's output.
type Limits struct{ MaxLines, MaxChars int }

func (l Limits) lines() int {
	if l.MaxLines < 1 {
		return 100
	}
	return l.MaxLines
}

func (l Limits) chars() int {
	if l.MaxChars < 1 {
		return 8000
	}
	return l.MaxChars
}

// ── output classification ────────────────────────────────────────────────────

// Output verdicts. A run_kubectl result is a string, so every layer above it recovers
// the machine readable answer from prose, and doing that badly is how a refusal
// becomes a success and an unreadable cluster becomes a health verdict. One
// classifier, used by all of them, so they cannot disagree about what a string meant.
// Matching is on line prefixes, never substrings anywhere: that is the difference
// between a kubectl failure and a Deployment named error-budget-exporter.
const (
	Refused = "refused" // kube-sre blocked it, nothing was sent to the cluster
	FailedO = "failed"  // kubectl or the local tooling reported an error
	OK      = "ok"      // nothing says otherwise; the caller's own oracle decides
	NoOut   = "(no output)"
)

var (
	refusalPrefixes   = []string{"[permission denied]", "[protected]", "[unsupported]", "[blocked]", "[error]"}
	failurePrefixes   = []string{"error:", "error from server", "the connection to the server", "unable to connect to the server"}
	unreachablePrefix = []string{"the connection to the server", "unable to connect to the server"}
	exitMarker        = regexp.MustCompile(`^\[(?:kubectl|helm) exited -?\d+\]\s*`)
	emptyPrefixes     = []string{"no resources found", "no resources", "(no output)"}
)

func hasPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// ClassifyOutput is Refused, FailedO or OK for one result. Anything unrecognised is OK,
// not because it certainly worked but because the caller's own oracle is a better
// authority than a keyword.
func ClassifyOutput(out string) string {
	text := strings.TrimSpace(out)
	if text == "" || text == NoOut {
		return FailedO
	}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case line == "":
		case exitMarker.MatchString(line):
			return FailedO
		case hasPrefix(line, refusalPrefixes):
			return Refused
		case hasPrefix(line, failurePrefixes):
			return FailedO
		}
	}
	return OK
}

// ReachedCluster is false iff nothing ever got as far as the API server. A refusal is
// emitted by kube-sre itself and a connection error by kubectl's client, so in both
// cases the server never saw the command, and a gate that reports on what the server
// said must not report at all.
func ReachedCluster(out string) bool {
	text := strings.TrimSpace(out)
	if text == "" || text == NoOut {
		return false
	}
	for _, raw := range strings.Split(text, "\n") {
		line := exitMarker.ReplaceAllString(strings.ToLower(strings.TrimSpace(raw)), "")
		if line == "" {
			continue
		}
		if hasPrefix(line, refusalPrefixes) || hasPrefix(line, unreachablePrefix) {
			return false
		}
	}
	return true
}

// ── bounds ───────────────────────────────────────────────────────────────────

var noiseLine = regexp.MustCompile(`^\s*(managedFields:|resourceVersion:|uid:|generation:|creationTimestamp:|kubectl\.kubernetes\.io/last-applied-configuration:)`)

// IsReadOnly is true iff the command cannot change anything, by the kubectl tool's own
// definition. rollout is only read only in part (status and history report, restart,
// undo and pause mutate), so this delegates to the tool and unknown verbs fail closed.
func IsReadOnly(command string) bool {
	toks, err := kube.Split(strings.TrimSpace(command))
	if err != nil || len(toks) == 0 {
		return false
	}
	if toks[0] != "kubectl" {
		toks = append([]string{"kubectl"}, toks...)
	}
	verb := kube.ExtractVerb(toks)
	return verb != "" && !kube.IsWrite(verb, toks)
}

// NormalizeKRM strips server noise keys from KRM output.
func NormalizeKRM(text string) string {
	var out []string
	skip := -1
	for _, line := range strings.Split(text, "\n") {
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if skip >= 0 {
			stripped := strings.TrimLeft(line, " ")
			if strings.TrimSpace(line) == "" || indent > skip || (indent == skip && strings.HasPrefix(stripped, "- ")) {
				continue
			}
			skip = -1
		}
		if noiseLine.MatchString(line) {
			if strings.HasSuffix(strings.TrimRight(line, " "), ":") {
				skip = indent
			}
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// Window returns the trimmed body, the total and shown line counts and the next
// cursor. It enforces the line window first, then the char cap, never splitting a line
// mid way. The cursor is the offset to resume from when output was truncated.
func Window(body string, maxLines, maxChars, offset int) (trimmed string, total, shown int, next *int) {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if strings.TrimSpace(body) == "" {
		lines = nil
	}
	total = len(lines)
	if offset > total {
		offset = total
	}
	end := offset + maxLines
	if end > total {
		end = total
	}
	take := append([]string(nil), lines[offset:end]...)
	truncated := end < total
	if len(strings.Join(take, "\n")) > maxChars {
		for len(take) > 0 && len(strings.Join(take, "\n")) > maxChars {
			take = take[:len(take)-1]
		}
		truncated = true
	}
	shown = len(take)
	trimmed = strings.Join(take, "\n")
	if truncated && shown > 0 {
		n := offset + shown
		next = &n
	}
	if truncated && shown == 0 && offset < total {
		first := lines[offset]
		if len(first) > maxChars {
			first = first[:maxChars]
		}
		return first, total, 1, nil
	}
	return trimmed, total, shown, next
}

var (
	failedWords     = map[string]bool{"crashloopbackoff": true, "error": true, "failed": true, "imagepullbackoff": true}
	inProgressWords = map[string]bool{"pending": true, "containercreating": true, "progressing": true}
	currentWords    = map[string]bool{"running": true, "ready": true, "active": true, "bound": true, "available": true}
)

// healthFrom reads whole fields, lowercased, with surrounding punctuation stripped.
// It splits on whitespace only: a hyphen is part of a Kubernetes name, so splitting on
// it is what turned error-budget-exporter into the word error.
func healthFrom(body string) Health {
	fields := map[string]bool{}
	for _, f := range strings.Fields(body) {
		fields[strings.ToLower(strings.Trim(f, `:,.'"()[]{}`))] = true
	}
	any := func(set map[string]bool) bool {
		for k := range fields {
			if set[k] {
				return true
			}
		}
		return false
	}
	switch {
	case fields["terminating"]:
		return Terminating
	case any(failedWords):
		return Failed
	case any(inProgressWords):
		return InProgress
	case any(currentWords):
		return Current
	}
	return Unknown
}

// ── the verbs ────────────────────────────────────────────────────────────────

// Verbs runs the read verbs through a Runner.
type Verbs struct {
	Run    Runner
	Limits Limits
}

func nsFlag(namespace string, all bool) string {
	switch {
	case all:
		return " --all-namespaces"
	case namespace != "":
		return " -n " + namespace
	}
	return ""
}

func looksEmpty(out string) bool {
	s := strings.ToLower(strings.TrimSpace(out))
	return s == "" || hasPrefix(s, emptyPrefixes)
}

// run is the shared verb tail: assert read only, execute, normalise, bound.
func (v Verbs) run(ctx context.Context, verb, command, target string) (Result, error) {
	if !IsReadOnly(command) {
		return Result{}, &ContractError{fmt.Sprintf("%s produced a non-read-only command: %q", verb, command)}
	}
	raw := v.Run(ctx, command, "")
	if ClassifyOutput(raw) != OK {
		return Result{Verb: verb, Target: target, Command: command, Error: strings.TrimSpace(raw)}, nil
	}
	if looksEmpty(raw) {
		return Result{Verb: verb, Target: target, Command: command, OK: true, Empty: true}, nil
	}
	body, total, shown, cursor := Window(NormalizeKRM(raw), v.Limits.lines(), v.Limits.chars(), 0)
	return Result{Verb: verb, Target: target, Command: command, OK: true, Body: body, TotalLines: total, Shown: shown, Cursor: cursor}, nil
}

// Inspect describes one object, normalised and bounded.
func (v Verbs) Inspect(ctx context.Context, kind, name, namespace, view string) (Result, error) {
	cmd := fmt.Sprintf("kubectl describe %s %s%s", kind, name, nsFlag(namespace, false))
	if view == "full" {
		cmd = fmt.Sprintf("kubectl get %s %s%s -o yaml", kind, name, nsFlag(namespace, false))
	}
	target := kind + "/" + name
	if namespace != "" {
		target += " in " + namespace
	}
	r, err := v.run(ctx, "inspect", cmd, target)
	if err == nil && r.OK && !r.Empty {
		r.Health = healthFrom(r.Body)
	}
	return r, err
}

// Search lists objects by kind, namespace and selector, names first.
func (v Verbs) Search(ctx context.Context, kinds []string, namespace string, all bool, selector string, limit int) (Result, error) {
	if len(kinds) == 0 {
		return Result{Verb: "search", Target: "(no kinds)", Error: "search needs at least one kind."}, nil
	}
	sel := ""
	if selector != "" {
		sel = " -l " + selector
	}
	k := strings.Join(kinds, ",")
	target := k
	switch {
	case all:
		target += " (all namespaces)"
	case namespace != "":
		target += " in " + namespace
	}
	lim := v.Limits
	if limit > 0 && limit < lim.lines() {
		lim.MaxLines = limit
	}
	return Verbs{Run: v.Run, Limits: lim}.run(ctx, "search", fmt.Sprintf("kubectl get %s%s%s -o wide", k, nsFlag(namespace, all), sel), target)
}

// Logs reads pod logs, bounded, with dead pod evidence via previous.
func (v Verbs) Logs(ctx context.Context, namespace, pod, selector, container string, lines int, since string, previous bool) (Result, error) {
	if pod == "" && selector == "" {
		return Result{Verb: "logs", Target: "logs in " + namespace, Error: "logs requires either 'pod' or 'selector'."}, nil
	}
	tail := lines
	if tail < 1 || tail > v.Limits.lines() {
		tail = v.Limits.lines()
	}
	ref := pod
	if pod == "" {
		ref = "-l " + selector
	}
	parts := []string{fmt.Sprintf("kubectl logs %s -n %s --tail=%d", ref, namespace, tail)}
	if container != "" {
		parts = append(parts, "-c "+container)
	}
	if since != "" {
		parts = append(parts, "--since="+since)
	}
	if previous {
		parts = append(parts, "--previous")
	}
	target := fmt.Sprintf("logs %s in %s", ref, namespace)
	if previous {
		target += " (previous)"
	}
	return v.run(ctx, "logs", strings.Join(parts, " "), target)
}

// DiffChange diffs an object against live state or a prior revision. against=git is a
// declared non goal: declined, never a silent no op.
func (v Verbs) DiffChange(ctx context.Context, against, kind, name, namespace, manifest string, revision int) (Result, error) {
	target := kind + "/" + name
	if namespace != "" {
		target += " in " + namespace
	}
	switch against {
	case "git":
		return Result{Verb: "diff_change", Target: target, Error: "against=git is unsupported (GitOps diff deferred)."}, nil
	case "previous":
		rev := ""
		if revision > 0 {
			rev = fmt.Sprintf(" --revision=%d", revision)
		}
		return v.run(ctx, "diff_change", fmt.Sprintf("kubectl rollout history %s/%s%s%s", kind, name, nsFlag(namespace, false), rev), target+" (rollout history)")
	case "live":
		if manifest == "" {
			return Result{Verb: "diff_change", Target: target, Error: "against=live requires a 'manifest' to diff against the cluster."}, nil
		}
		const cmd = "kubectl diff -f -"
		if !IsReadOnly(cmd) {
			return Result{}, &ContractError{"diff_change built a non-read-only command"}
		}
		raw := v.Run(ctx, cmd, manifest)
		t := target + " (live diff)"
		if looksEmpty(raw) {
			return Result{Verb: "diff_change", Target: t, Command: cmd, OK: true, Empty: true}, nil
		}
		body, total, shown, cursor := Window(raw, v.Limits.lines(), v.Limits.chars(), 0)
		return Result{Verb: "diff_change", Target: t, Command: cmd, OK: true, Body: body, TotalLines: total, Shown: shown, Cursor: cursor}, nil
	}
	return Result{Verb: "diff_change", Target: target, Error: fmt.Sprintf("against must be live, previous or git, not %q.", against)}, nil
}

// Specs describes the verbs to a model as tools.
func Specs() []llm.ToolSpec {
	str := map[string]any{"type": "string"}
	obj := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	return []llm.ToolSpec{
		{Name: "inspect", Description: "Inspect one object (normalised, bounded). Read-only.",
			Parameters: obj(map[string]any{"kind": str, "name": str, "namespace": str, "view": map[string]any{"type": "string", "enum": []string{"summary", "status", "full"}}}, "kind", "name")},
		{Name: "search", Description: "List objects by kind, namespace and selector, names first. Read-only. Matching is by label, so an empty result does not prove an object is absent.",
			Parameters: obj(map[string]any{"kinds": map[string]any{"type": "array", "items": str}, "namespace": str, "all_namespaces": map[string]any{"type": "boolean"}, "selector": str, "limit": map[string]any{"type": "integer"}}, "kinds")},
		{Name: "logs", Description: "Read pod logs (bounded; previous=true reads the last terminated container). Read-only.",
			Parameters: obj(map[string]any{"namespace": str, "pod": str, "selector": str, "container": str, "lines": map[string]any{"type": "integer"}, "since": str, "previous": map[string]any{"type": "boolean"}}, "namespace")},
		{Name: "diff_change", Description: "Diff an object against live state or a prior revision. Read-only.",
			Parameters: obj(map[string]any{"against": map[string]any{"type": "string", "enum": []string{"live", "previous", "git"}}, "kind": str, "name": str, "namespace": str, "manifest": str, "revision": map[string]any{"type": "integer"}}, "against", "kind", "name")},
	}
}

func argStr(a map[string]any, k string) string {
	s, _ := a[k].(string)
	return s
}

func argInt(a map[string]any, k string) int {
	switch n := a[k].(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func argBool(a map[string]any, k string) bool { b, _ := a[k].(bool); return b }

// Call dispatches one model tool call by name and returns the rendered text. A verb
// outside the allowlist is refused, and a contract error is reported and not raised.
func (v Verbs) Call(ctx context.Context, name string, args map[string]any) string {
	var r Result
	var err error
	switch name {
	case "inspect":
		r, err = v.Inspect(ctx, argStr(args, "kind"), argStr(args, "name"), argStr(args, "namespace"), argStr(args, "view"))
	case "search":
		var kinds []string
		if xs, ok := args["kinds"].([]any); ok {
			for _, x := range xs {
				if s, ok := x.(string); ok {
					kinds = append(kinds, s)
				}
			}
		}
		r, err = v.Search(ctx, kinds, argStr(args, "namespace"), argBool(args, "all_namespaces"), argStr(args, "selector"), argInt(args, "limit"))
	case "logs":
		r, err = v.Logs(ctx, argStr(args, "namespace"), argStr(args, "pod"), argStr(args, "selector"), argStr(args, "container"),
			argInt(args, "lines"), argStr(args, "since"), argBool(args, "previous"))
	case "diff_change":
		r, err = v.DiffChange(ctx, argStr(args, "against"), argStr(args, "kind"), argStr(args, "name"), argStr(args, "namespace"), argStr(args, "manifest"), argInt(args, "revision"))
	default:
		return fmt.Sprintf("[blocked] %q is not a read-only verb", name)
	}
	if err != nil {
		return "[aci:" + name + "] internal error: " + err.Error()
	}
	return r.Render()
}
