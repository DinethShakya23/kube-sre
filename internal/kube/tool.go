// Package kube is run_kubectl: the single execution surface for Kubernetes
// operations. Safety layers, in order:
//
//  1. shell metacharacters are refused (there is no shell, but the model should
//     not be writing them);
//  2. stdin YAML is validated before the cluster is touched;
//  3. connection and identity flags are refused: which cluster, and as whom, are
//     fixed by the deployment;
//  4. rejected verbs, then the role check (readonly, operator, admin, superadmin);
//  5. protected namespaces and resource types, read from argv and the manifest;
//  6. risk classification: writes need human approval, and some ask even on an
//     auto approve session;
//  7. execution with no shell, `| grep` emulated in Go, namespace filters on the
//     output, an output cap.
package kube

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/DinethShakya23/kube-sre/internal/nsguard"
	"github.com/DinethShakya23/kube-sre/internal/policy"
	"github.com/DinethShakya23/kube-sre/internal/redact"
)

const MaxOutput = 8000

// Recorder receives rollback points for the flight recorder.
type Recorder interface {
	Record(episode, kind string, payload map[string]any)
}

// Config is what a Tool needs from the deployment.
type Config struct {
	Bin                string
	Kubeconfig         string
	Timeout            time.Duration
	DestructiveTimeout time.Duration
	BlockedNamespaces  nsguard.Blocklist
	BlockedResources   map[string]bool
	ErrorHints         bool
	Recorder           Recorder
}

type Tool struct {
	cfg     Config
	blocked map[string]bool // BlockedResources expanded through the spelling rules
}

func NewTool(cfg Config) *Tool {
	if cfg.Bin == "" {
		cfg.Bin = "kubectl"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.DestructiveTimeout <= 0 {
		cfg.DestructiveTimeout = 60 * time.Second
	}
	t := &Tool{cfg: cfg, blocked: map[string]bool{}}
	// Both sides of the comparison get the same rules: the typed token and the
	// configured entry, so it no longer matters which spelling an operator wrote.
	for entry := range cfg.BlockedResources {
		for s := range ResourceSpellings(entry) {
			t.blocked[s] = true
		}
	}
	return t
}

// Call is what the run carries alongside the command. It is set by the server,
// never by the model.
type Call struct {
	Role       string // readonly | operator | admin | superadmin; empty means admin
	HitlBypass bool   // auto approve session
	// SandboxIdentity is the one impersonation token the application placed on
	// this command. It is an exact match: a token that merely looks like it is not
	// enough, and any second identity flag alongside it still fails.
	SandboxIdentity string
	SessionID       string
}

// Approval is what a human is asked to approve.
type Approval struct {
	Type          string `json:"type"`
	Command       string `json:"command"`
	Stdin         string `json:"stdin"`
	RiskLevel     string `json:"risk_level"`
	AlwaysConfirm bool   `json:"always_confirm"`
	HumanSummary  string `json:"human_summary"`
}

// Plan is a command that has passed every gate up to execution.
type Plan struct {
	Cmd       string
	Args      []string
	Pipes     []string
	Stdin     string
	Verb      string
	HasDryRun bool
	// Refusal, when set, is the answer to hand back; the command must not run.
	Refusal string
	// Approval, when set, means a human must approve before Execute.
	Approval *Approval
}

// UserError is a problem with the command the model wrote. It is shown to the
// model as the tool's answer so it can fix the command.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func userErr(format string, a ...any) error { return &UserError{fmt.Sprintf(format, a...)} }

func normalise(command string) string {
	cmd := strings.TrimSpace(command)
	if !strings.HasPrefix(cmd, "kubectl") {
		cmd = "kubectl " + cmd
	}
	return cmd
}

// Plan runs every gate up to execution. The returned plan has Refusal set,
// Approval set, or neither, in which case Execute may run it.
func (t *Tool) Plan(command, stdin string, c Call) (*Plan, error) {
	parts := splitPipes(command)
	cmd := normalise(strings.TrimSpace(parts[0]))
	var pipes []string
	for _, p := range parts[1:] {
		pipes = append(pipes, strings.TrimSpace(p))
	}

	// Non grep pipes fail before anything runs.
	for _, seg := range pipes {
		var toks []string
		if seg != "" {
			var err error
			if toks, err = Split(seg); err != nil {
				return nil, userErr("%v", err)
			}
		}
		if len(toks) > 0 && toks[0] != "grep" {
			return nil, userErr("Pipe segment %q contains disallowed shell characters or unsupported command. "+
				"Only 'grep' is allowed after '|'.", seg)
		}
	}

	// stdin goes straight to the process with no shell, so metacharacters in YAML
	// are harmless and are not checked.
	if shellMeta.MatchString(cmd) {
		return nil, userErr("Command contains disallowed shell characters: %q. Use plain kubectl syntax only. "+
			"Tip: -o jsonpath='...' with {\"\\n\"} separators is supported; use -o json for complex extraction.", cmd)
	}
	for _, seg := range pipes {
		if shellMeta.MatchString(seg) {
			return nil, userErr("Pipe segment contains disallowed shell characters: %q.", seg)
		}
	}

	if stdin != "" {
		if err := ValidateStdinYAML(stdin); err != nil {
			return nil, userErr("%v", err)
		}
	}

	args, err := Split(cmd)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "closing quotation") {
			return nil, userErr("Could not parse command (unclosed quote): %q. The jsonpath expression appears "+
				"to be truncated. Use -o json instead (-o custom-columns cannot be filtered by namespace).", cmd)
		}
		return nil, userErr("Could not parse command: %v", err)
	}
	p := &Plan{Cmd: cmd, Args: args, Pipes: pipes, Stdin: stdin}

	// Connection and identity are the deployment's, not the caller's.
	if flag := ConnectionFlagIn(args); flag != "" {
		authorised := ""
		if strings.HasPrefix(c.SandboxIdentity, "--as=") {
			authorised = c.SandboxIdentity
		}
		if authorised == "" || !identityIsAuthorised(args, authorised) {
			slog.Warn("run_kubectl refused a connection or identity override", "flag", flag, "cmd", cmd)
			p.Refusal = fmt.Sprintf("[Protected] '%s' is not permitted. Which cluster this connects to, and the identity "+
				"it uses, are fixed by the deployment, so they are not part of a query. Ask the question without it.", flag)
			return p, nil
		}
	}

	verb := ExtractVerb(args)
	p.Verb = verb

	if rejectedVerbs[verb] {
		p.Refusal = fmt.Sprintf("[Unsupported] 'kubectl %s' needs an interactive terminal, which is not available. "+
			"Use 'kubectl patch' or 'kubectl apply -f -' with stdin instead.", verb)
		return p, nil
	}
	if op := operandAfterVerb(args); rejectedSubcommands[verb][op] {
		p.Refusal = fmt.Sprintf("[Protected] 'kubectl %s %s' returns the contents of every namespace in one payload, "+
			"including the infrastructure namespaces this deployment withholds, and it has no per object shape that "+
			"can be filtered. Query the namespace you need with -n <namespace>.", verb, op)
		return p, nil
	}

	// Role check.
	//   readonly   : all writes blocked
	//   operator   : medium risk allowed (approval gated); high risk blocked
	//   admin      : everything allowed (approval gated); infra namespaces blocked, reads too
	//   superadmin : as admin, but no namespace restrictions at all, and never the resource block
	role := c.Role
	if role == "" {
		role = "admin"
	}
	// Any destructive verb present counts, not only the parsed one: if a command
	// shape ever slips past ExtractVerb again, it fails closed here.
	present := DestructiveVerbsIn(args)
	if role == "readonly" && (DestructiveVerbs[verb] || len(present) > 0 || IsWrite(verb, args)) {
		denied := verb
		if !(DestructiveVerbs[verb] || IsWrite(verb, args)) {
			denied = sortedKeys(present)[0]
		}
		p.Refusal = fmt.Sprintf("[Permission Denied] Your API key has read-only access. "+
			"The '%s' operation requires an operator or admin API key.", denied)
		return p, nil
	}
	high := map[string]bool{}
	for v := range present {
		if highRisk[v] {
			high[v] = true
		}
	}
	if role == "operator" && (highRisk[verb] || len(high) > 0) {
		denied := verb
		if !highRisk[verb] {
			denied = sortedKeys(high)[0]
		}
		p.Refusal = fmt.Sprintf("[Permission Denied] Your API key has operator access. "+
			"The '%s' operation requires an admin API key.", denied)
		return p, nil
	}

	// Manifest sources the tool cannot read: a path or URL puts the manifest
	// where the gates cannot reach and the approval prompt shows nothing to review.
	if ext := externalManifestSource(verb, args); ext != nil {
		p.Refusal = fmt.Sprintf("[Unsupported] Reading a manifest from %q is not permitted. It cannot be inspected, so the "+
			"protected namespace and protected resource checks would be skipped and the approval prompt would show you "+
			"nothing to review. Use 'kubectl %s -f -' and pass the YAML as stdin instead.", *ext, verb)
		return p, nil
	}

	// Protected namespace and resource check. It runs before approval so nobody
	// gets a prompt for a command that would expose internal credentials.
	if msg := t.checkProtected(verb, args, stdin); msg != "" {
		if role == "superadmin" && t.blockedResourceHit(verb, args, stdin) == "" {
			msg = "" // superadmin bypasses the namespace block, never the resource block
		}
		if msg != "" {
			slog.Warn("run_kubectl blocked protected access", "cmd", cmd)
			p.Refusal = msg
			return p, nil
		}
	}

	// Risk classification, then the approval gate.
	p.HasDryRun = indexOf(args, "--dry-run=client") >= 0 || indexOf(args, "--dry-run=server") >= 0 || indexOf(args, "--dry-run") >= 0
	if DestructiveVerbs[verb] || len(present) > 0 || IsWrite(verb, args) {
		alwaysConfirm := AlwaysConfirm(verb, args)
		if !p.HasDryRun && (!c.HitlBypass || alwaysConfirm) {
			hidden := sortedKeys(present)
			effective := verb
			if !(DestructiveVerbs[verb] || len(hidden) == 0) {
				effective = hidden[0]
			}
			risk := Classify(effective, args)
			if alwaysConfirm {
				risk = RiskHigh
			}
			if alwaysConfirm && c.HitlBypass {
				slog.Warn("run_kubectl always-confirm overrides auto-approve", "cmd", cmd)
			}
			slog.Info("run_kubectl approval needed", "verb", verb, "risk", risk, "session", c.SessionID)
			p.Approval = &Approval{
				Type: "hitl", Command: cmd, Stdin: stdin, RiskLevel: string(risk),
				AlwaysConfirm: alwaysConfirm, HumanSummary: "About to run: `" + cmd + "`",
			}
		} else if !p.HasDryRun && c.HitlBypass {
			slog.Info("run_kubectl approval bypassed (auto-approve)", "cmd", cmd)
		}
	}
	return p, nil
}

// Run plans and executes in one call. approve is asked when a human must decide;
// a nil approve denies. It exists for the REPL and tests; the server drives Plan
// and Execute itself because approval spans two HTTP requests.
func (t *Tool) Run(ctx context.Context, command, stdin string, c Call, approve func(Approval) bool) (string, error) {
	p, err := t.Plan(command, stdin, c)
	if err != nil {
		return "", err
	}
	if p.Refusal != "" {
		return p.Refusal, nil
	}
	if p.Approval != nil && (approve == nil || !approve(*p.Approval)) {
		return "Action cancelled by user.", nil
	}
	return t.Execute(ctx, p, c)
}

func (t *Tool) blockedResourceHit(verb string, args []string, stdin string) string {
	// `get pods,secrets` names two types in one token, and each is checked.
	resource := extractResourceType(verb, args)
	for _, one := range strings.Split(resource, ",") {
		if intersects(ResourceSpellings(one), t.blocked) {
			return one
		}
	}
	for _, kind := range sortedKeys(manifestKinds(stdin)) {
		if intersects(ResourceSpellings(kind), t.blocked) {
			return kind
		}
	}
	return ""
}

// allNamespacesHit refuses a mutation that targets every namespace at once. The
// protected namespace check asks which namespace a command names, and a command
// naming all of them names none in particular. Reads are not refused, because
// `get pods -A` is how an agent sees the shape of a cluster; their output is
// filtered instead.
func allNamespacesHit(verb string, args []string) string {
	if !isAllNamespaces(args) || !IsWrite(verb, args) {
		return ""
	}
	return "[Protected] A cluster-wide mutation (--all-namespaces) is not permitted: it would reach infrastructure " +
		"namespaces that are individually blocked. Narrow it with -n <namespace>."
}

// checkProtected returns a refusal if the command targets a protected namespace
// or resource type. Both checks read the manifest as well as the command line:
// `apply -f -` puts the resource in `kind:` and the namespace in
// `metadata.namespace:`, so a check that only parses argv sees neither.
func (t *Tool) checkProtected(verb string, args []string, stdin string) string {
	if hit := t.blockedResourceHit(verb, args, stdin); hit != "" {
		return fmt.Sprintf("[Protected] Access to '%s' is not permitted through kube-sre. "+
			"Kubernetes Secrets and ServiceAccount tokens are shielded from inspection to protect cluster credentials.", hit)
	}
	if msg := allNamespacesHit(verb, args); msg != "" {
		return msg
	}
	candidates := []string{extractNamespace(args, verb)}
	candidates = append(candidates, targetedNamespaces(verb, args)...)
	candidates = append(candidates, sortedKeys(manifestNamespaces(stdin))...)
	for _, ns := range candidates {
		// Case folded on both sides: the value here comes from a model written
		// command line or a manifest, not from a validated API object.
		if ns != "" && t.cfg.BlockedNamespaces[strings.ToLower(strings.TrimSpace(ns))] {
			return fmt.Sprintf("[Protected] Access to namespace '%s' is not permitted. This is an infrastructure namespace.", ns)
		}
	}
	return ""
}

func (t *Tool) env() []string {
	kc := t.cfg.Kubeconfig
	if strings.HasPrefix(kc, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			kc = filepath.Join(home, kc[2:])
		}
	}
	return append(os.Environ(), "KUBECONFIG="+kc)
}

// Execute runs a plan that has passed its gates. The caller must have obtained
// approval if the plan asked for it.
func (t *Tool) Execute(ctx context.Context, p *Plan, c Call) (string, error) {
	args, verb := p.Args, p.Verb
	write := IsWrite(verb, args)

	// Safety sandwich: arm a rollback point before any mutation, so every
	// destructive action is undoable from the audit trail. The test is the same
	// one the approval gate uses, so `rollout restart` and any verb this build
	// does not know arm one too.
	if write && !p.HasDryRun {
		t.captureRollbackPoint(ctx, verb, args, p.Stdin, c)
	}

	timeout := t.cfg.Timeout
	if Classify(verb, args) == RiskHigh {
		timeout = t.cfg.DestructiveTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, t.cfg.Bin, args[1:]...)
	cmd.Env = t.env()
	if p.Stdin != "" {
		cmd.Stdin = strings.NewReader(p.Stdin)
	}
	var stdoutB, stderrB bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdoutB, &stderrB
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee):
			code = ee.ExitCode()
		case errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist):
			return policy.MarkUnavailable("[Error] kubectl is not installed or not found in PATH. "+
				"Install it from https://kubernetes.io/docs/tasks/tools/ or run 'kube-sre kind-setup' to provision a local cluster.",
				"kubectl is not on PATH."), nil
		case cctx.Err() != nil:
			return "", userErr("kubectl timed out after %s", timeout)
		default:
			return "", err
		}
		if cctx.Err() != nil {
			return "", userErr("kubectl timed out after %s", timeout)
		}
	}

	stdout, stderr := stdoutB.String(), stderrB.String()
	// Decode free form content lossily to survive non-ASCII log lines; identifiers
	// and structured output are strict, to avoid plausible wrong answers.
	if verb == "logs" || verb == "describe" || verb == "get" {
		stdout, stderr = strings.ToValidUTF8(stdout, "\uFFFD"), strings.ToValidUTF8(stderr, "\uFFFD")
	} else if !utf8.ValidString(stdout) || !utf8.ValidString(stderr) {
		return "", userErr("kubectl output was not valid UTF-8")
	}
	// Whether kubectl printed anything, decided before the emulator runs: grep
	// turns an empty string into "(no matching lines)", and reporting that as
	// output kubectl produced would be a claim about the cluster made by our own grep.
	printed := stdout != ""
	slog.Debug("run_kubectl finished", "exit", code, "stdout", len(stdout), "stderr", len(stderr), "cmd", p.Cmd)

	// Pipes apply to stdout only, as in a real shell. Grepping a failed command's
	// error away would answer "no matching lines", byte for byte what a healthy
	// empty listing says.
	if len(p.Pipes) > 0 {
		if stdout, err = applyPipes(stdout, p.Pipes); err != nil {
			return "", userErr("%v", err)
		}
	}
	// Listing filters parse a listing, so they see stdout only, never an error.
	stdout = FilterNamespaceOutput(verb, args, stdout, t.cfg.BlockedNamespaces)
	stdout = FilterAllNamespacesOutput(verb, args, stdout, t.cfg.BlockedNamespaces)

	var output string
	if code != 0 && !ExitIsAnAnswer(verb, args, code) {
		// A non-zero exit is stated, not implied: dropping stderr whenever stdout
		// had anything would hide a partial failure such as one forbidden namespace.
		detail := strings.TrimSpace(stderr)
		if detail == "" {
			detail = "(kubectl wrote nothing to stderr)"
		}
		name := ""
		if t.cfg.ErrorHints {
			detail, name = Annotate(detail)
			if name != "" {
				slog.Info("kubectl error interpreted", "pattern", name, "exit_code", code, "cmd", p.Cmd)
			}
		}
		output = fmt.Sprintf("[kubectl exited %d] %s", code, detail)
		if IsTerminal(name) {
			// The apiserver is not answering, so a second call cannot get past it.
			output += "\n" + policy.UnavailableNotice("The cluster is not reachable from here.")
		}
		if printed && strings.TrimSpace(stdout) != "" {
			output += "\n\nkubectl also produced this output before or alongside the error. It may be partial, " +
				"and absence from it is NOT evidence:\n" + stdout
		}
	} else {
		switch {
		case stdout != "":
			output = stdout
		case stderr != "":
			output = stderr
		default:
			output = "(no output)"
		}
	}

	if r := []rune(output); len(r) > MaxOutput {
		omitted := len(r) - MaxOutput
		output = string(r[:MaxOutput]) + "\n\n" + policy.TruncationMarker(omitted, "chars",
			"output was cut short. Inform the user that the list is incomplete and suggest narrowing with "+
				"--tail, -n <namespace>, or -l <label> flags.")
		slog.Debug("run_kubectl output truncated", "omitted", omitted)
	}
	return output, nil
}

const (
	rollbackMaxChars   = 4000
	rollbackMaxTargets = 5
)

func captureNote(before, after string, limit int) string {
	if strings.HasSuffix(after, "[...]") {
		return fmt.Sprintf("truncated at %d chars (object is %d chars)", limit, len([]rune(before)))
	}
	dropped, marks := redact.Count(after)
	var parts []string
	if dropped > 0 {
		parts = append(parts, fmt.Sprintf("%d line(s) dropped", dropped))
	}
	if marks > 0 {
		parts = append(parts, fmt.Sprintf("%d value(s) replaced", marks))
	}
	if len(parts) == 0 {
		return "redacted"
	}
	return "redacted: " + strings.Join(parts, ", ")
}

// rollbackTargets works out which objects a mutation touches, as `get -o yaml`
// argument lists.
func rollbackTargets(verb string, args []string, stdin string) (targets [][]string, parseErr string) {
	if stdin != "" {
		docs, err := decodeAll(stdin)
		for _, d := range docs {
			m, ok := d.(map[string]any)
			if !ok {
				continue
			}
			kind := strings.ToLower(fmt.Sprint(orEmpty(m["kind"])))
			meta, _ := m["metadata"].(map[string]any)
			name, ns := fmt.Sprint(orEmpty(meta["name"])), fmt.Sprint(orEmpty(meta["namespace"]))
			if kind != "" && name != "" {
				target := []string{"get", kind, name, "-o", "yaml"}
				if ns != "" {
					target = append(target, "-n", ns)
				}
				targets = append(targets, target)
			}
		}
		if err != nil {
			// A parse that dies half way leaves targets covering only the documents
			// read so far. Swallowing that would make a partial capture look whole.
			parseErr = clipStr(err.Error(), 200)
		}
		return
	}
	// delete, patch, scale, label with explicit resource args: swap the verb for
	// `get ... -o yaml` and drop value bearing flags.
	var tail []string
	for _, a := range args[1:] {
		if a != verb {
			tail = append(tail, a)
		}
	}
	// `rollout` carries a subcommand before its target, and leaving it in would
	// make `kubectl get restart deployment/api`, which kubectl rejects.
	if verb == "rollout" && len(tail) > 0 {
		subAt := skipFlags(append([]string{"kubectl"}, tail...), 1) - 1
		if subAt >= 0 && subAt < len(tail) {
			tail = append(tail[:subAt], tail[subAt+1:]...)
		}
	}
	var keep []string
	skipNext := false
	for _, tok := range tail {
		if skipNext {
			skipNext = false
			continue
		}
		if tok == "-n" || tok == "--namespace" {
			keep = append(keep, tok)
			continue
		}
		if strings.HasPrefix(tok, "-") {
			if !strings.Contains(tok, "=") && tok != "--all" {
				skipNext = true
			}
			continue
		}
		// `label pod api-1 tier=web` ends in key=value pairs, which are not
		// resource names: a Kubernetes name cannot contain `=`.
		if strings.Contains(tok, "=") && !strings.Contains(tok, "/") {
			continue
		}
		keep = append(keep, tok)
	}
	if len(keep) > 0 {
		targets = append(targets, append(append([]string{"get"}, keep...), "-o", "yaml"))
	}
	return
}

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

func clipStr(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// captureRollbackPoint records the pre state of the targeted objects in the
// flight recorder. Best effort; it never fails the command.
//
// A capture is evidence, and it is a restore point only when restorable is true.
// The YAML is redacted because it lands in a database, and capped, and both
// transformations can leave something that must not be piped into
// `kubectl apply -f -`: a Secret loses its `kind:` line to the word "secret", a
// ConfigMap of token shaped values applies cleanly with every value replaced, and
// anything over the cap is cut mid line. So each capture is compared with what
// kubectl produced, and the record says which it is. Redaction is not negotiable;
// claiming restorability that is not there is.
func (t *Tool) captureRollbackPoint(ctx context.Context, verb string, args []string, stdin string, c Call) {
	if t.cfg.Recorder == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("rollback capture failed (non-fatal)", "err", r)
		}
	}()
	targets, parseErr := rollbackTargets(verb, args, stdin)
	if len(targets) == 0 {
		return
	}
	var states []string
	var notes []string
	restorable := true
	// restorable answers two questions: is what we captured faithful (it survived
	// redaction and the cap), and does it cover every object the command touches.
	// A faithful capture of 3 of 7 objects must not read as a full restore point.
	intended := len(targets)
	if parseErr != "" {
		notes = append(notes, fmt.Sprintf("stdin: manifest parse failed (%s), object list is partial", parseErr))
		restorable = false
	}
	for i, target := range targets {
		if i >= rollbackMaxTargets {
			break
		}
		label := strings.Join(target[1:3], " ")
		tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		cmd := exec.CommandContext(tctx, t.cfg.Bin, target...)
		cmd.Env = t.env()
		var so, se bytes.Buffer
		cmd.Stdout, cmd.Stderr = &so, &se
		err := cmd.Run()
		cancel()
		switch {
		case err == nil && so.Len() > 0:
			out := strings.ToValidUTF8(so.String(), "\uFFFD")
			kept := redact.Secrets(out, rollbackMaxChars)
			// Redaction drops the final newline. That alone is not a change to the
			// object, so it must not cost a capture its restorable flag.
			if strings.TrimRight(kept, "\n") != strings.TrimRight(out, "\n") {
				restorable = false
				notes = append(notes, label+": "+captureNote(out, kept, rollbackMaxChars))
			}
			states = append(states, kept)
		default:
			code := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			}
			if err != nil && code == 0 {
				notes = append(notes, fmt.Sprintf("%s: not captured (%T)", label, err))
				continue
			}
			note := fmt.Sprintf("%s: not captured, kubectl exited %d", label, code)
			if lines := strings.Split(strings.TrimSpace(se.String()), "\n"); len(lines) > 0 && lines[0] != "" {
				note += ": " + redact.Secrets(lines[0], 200)
			}
			notes = append(notes, note)
		}
	}
	if len(targets) > rollbackMaxTargets {
		notes = append(notes, fmt.Sprintf("%d further object(s) were never attempted, the capture is capped at %d",
			len(targets)-rollbackMaxTargets, rollbackMaxTargets))
	}
	if len(states) == 0 {
		return
	}
	captured := len(states)
	if captured != intended {
		restorable = false
	}
	session := c.SessionID
	if session == "" {
		session = "-"
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	id := "rb-" + hex.EncodeToString(b)
	t.cfg.Recorder.Record(session, "rollback_point", map[string]any{
		"type": "rollback_point", "rollback_id": id,
		"command":    clipStr(strings.Join(args, " "), 300),
		"pre_state":  states,
		"restorable": restorable, "targets_intended": intended, "targets_captured": captured,
		"capture_notes": notes, "session_id": session,
	})
	if restorable {
		slog.Info("rollback point armed", "id", id, "targets", fmt.Sprintf("%d/%d", captured, intended))
	} else {
		slog.Warn("rollback point recorded but NOT restorable, do not apply it", "id", id,
			"targets", fmt.Sprintf("%d/%d", captured, intended), "notes", strings.Join(notes, "; "))
	}
}
