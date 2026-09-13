package aci

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/llm"
)

// The misconfiguration fix flow, the lowest blast radius write class: a fix emitted as
// a GitOps pull request, declarative revert (revert is "don't merge"), never a live
// mutation. propose repairs the manifest with a model, MakeFixPR packages the diff
// deterministically, and OpenPR pushes the branch and opens the request.

const repairSystem = "You are a Kubernetes security auto-repair. Given a MANIFEST and a policy VIOLATION, return ONLY the corrected manifest as valid YAML — " +
	"change the MINIMUM needed to resolve the violation, preserve everything else exactly. No prose, no explanations, no ``` fences."

func stripFences(text string) string {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, "```") {
		lines := strings.Split(t, "\n")
		if len(lines) > 0 && strings.HasPrefix(lines[0], "```") {
			lines = lines[1:]
		}
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "```" {
			lines = lines[:len(lines)-1]
		}
		t = strings.Join(lines, "\n")
	}
	return strings.TrimSpace(t)
}

// Repair is what the repair step produced, and whether it produced anything at all.
// Failing safe (returning the original) was the whole answer once, so a repair that
// never ran was byte identical to a repair that found nothing to change: a model
// error, an empty response and a refusal in prose all reached the PR opener as "no
// change to propose", the sentence a compliant manifest earns. On a security path
// that reads as "nothing to do" while the violation is untouched.
type Repair struct {
	Manifest string
	Repaired bool
	Reason   string
}

// ProposeFix asks the model for a corrected manifest and returns the original on any failure.
func ProposeFix(ctx context.Context, model llm.Model, manifest, violation string) Repair {
	resp, err := model.Chat(ctx, []llm.Message{{Role: llm.System, Content: repairSystem},
		{Role: llm.User, Content: "MANIFEST:\n" + manifest + "\n\nVIOLATION:\n" + violation}}, nil, llm.Options{})
	if err != nil {
		return Repair{manifest, false, "the repair call raised: " + err.Error()}
	}
	fixed := stripFences(resp.Message.Content)
	switch {
	case fixed == "":
		return Repair{manifest, false, "the model returned an empty response"}
	case !strings.Contains(fixed, "kind:"):
		return Repair{manifest, false, "the model's reply was not a manifest (no `kind:` line) — it was most likely prose"}
	}
	return Repair{Manifest: fixed, Repaired: true}
}

// FixPR is a PR ready diff and its metadata.
type FixPR struct {
	Path, Diff, Title, Rationale string
	RollbackClass                string
	RepairFailedReason           string
}

func (f FixPR) IsNoop() bool { return strings.TrimSpace(f.Diff) == "" }

func (f FixPR) counts() (added, removed int) {
	for _, ln := range strings.Split(f.Diff, "\n") {
		switch {
		case strings.HasPrefix(ln, "+") && !strings.HasPrefix(ln, "+++"):
			added++
		case strings.HasPrefix(ln, "-") && !strings.HasPrefix(ln, "---"):
			removed++
		}
	}
	return
}

func (f FixPR) AddedLines() int   { a, _ := f.counts(); return a }
func (f FixPR) RemovedLines() int { _, r := f.counts(); return r }

// normalised is manifest text with exactly one trailing newline, or empty if there is
// no content. The repair step ends in a trim, so a model that echoed the manifest back,
// its way of saying nothing needs changing, came back one newline shorter and produced
// a real diff: one line removed, the same added, and a pull request titled as a
// security fix whose entire content was a missing newline. A trailing newline is not
// a change to a manifest, and this is a PR opener.
func normalised(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return strings.TrimRight(text, "\n") + "\n"
}

func splitKeep(s string) []string {
	if s == "" {
		return nil
	}
	return strings.SplitAfter(strings.TrimSuffix(s, "\n"), "\n")
}

// UnifiedDiff is a git style unified diff of two manifests, ignoring trailing newline drift.
func UnifiedDiff(path, original, fixed string) string {
	a, b := splitKeep(normalised(original)), splitKeep(normalised(fixed))
	for i := range a {
		a[i] = strings.TrimSuffix(a[i], "\n")
	}
	for i := range b {
		b[i] = strings.TrimSuffix(b[i], "\n")
	}
	ops := diffOps(a, b)
	changed := false
	for _, o := range ops {
		changed = changed || o.kind != ' '
	}
	if !changed {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", path, path)
	const ctxLines = 3
	n := len(ops)
	for i := 0; i < n; {
		for i < n && ops[i].kind == ' ' {
			i++
		}
		if i >= n {
			break
		}
		start := i - ctxLines
		if start < 0 {
			start = 0
		}
		end, last := i, i
		for end < n {
			if ops[end].kind != ' ' {
				last = end
			}
			if end-last > 2*ctxLines {
				break
			}
			end++
		}
		end = last + ctxLines + 1
		if end > n {
			end = n
		}
		aStart, bStart, aLen, bLen := 1, 1, 0, 0
		for k := 0; k < start; k++ {
			if ops[k].kind != '+' {
				aStart++
			}
			if ops[k].kind != '-' {
				bStart++
			}
		}
		for k := start; k < end; k++ {
			if ops[k].kind != '+' {
				aLen++
			}
			if ops[k].kind != '-' {
				bLen++
			}
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", rng(aStart, aLen), rng(bStart, bLen))
		for k := start; k < end; k++ {
			fmt.Fprintf(&out, "%c%s\n", ops[k].kind, ops[k].text)
		}
		i = end
	}
	return out.String()
}

func rng(start, length int) string {
	switch length {
	case 0:
		return fmt.Sprintf("%d,0", start-1)
	case 1:
		return fmt.Sprint(start)
	}
	return fmt.Sprintf("%d,%d", start, length)
}

type diffOp struct {
	kind byte // ' ', '+' or '-'
	text string
}

// diffOps is a line diff by longest common subsequence.
func diffOps(a, b []string) []diffOp {
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < len(b); j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

// MakeFixPR packages a fix as a PR ready diff. A no op change is IsNoop. Pass
// repairFailedReason (from Repair.Reason) when the repair step produced nothing, so an
// empty diff can be told apart from a manifest that needed no change.
func MakeFixPR(path, original, fixed, title, rationale, repairFailedReason string) FixPR {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "fix: " + path
	}
	return FixPR{Path: path, Diff: UnifiedDiff(path, original, fixed), Title: title, Rationale: strings.TrimSpace(rationale),
		RollbackClass: DeclarativeRevert, RepairFailedReason: strings.TrimSpace(repairFailedReason)}
}

// CmdRunner runs a command and returns its exit code and combined output.
type CmdRunner func(argv []string) (int, string)

const cmdTimeout = 60 * time.Second

// DefaultCmdRunner runs a real command with a timeout.
func DefaultCmdRunner(argv []string) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	if ctx.Err() != nil {
		return 124, fmt.Sprintf("timed out after %s: %s", cmdTimeout, strings.Join(argv[:min(3, len(argv))], " "))
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), strings.TrimSpace(string(out))
		}
		return 127, err.Error()
	}
	return 0, strings.TrimSpace(string(out))
}

// PRResult is what opening a pull request did.
type PRResult struct {
	Pushed, Opened bool
	Detail         string
}

// OpenPR pushes the branch and opens a PR for the fix. It degrades gracefully: without
// the gh CLI it still pushes the branch and returns what a one click manual PR needs,
// so the change never silently fails to surface. A no op fix opens nothing, and says why.
func OpenPR(fix FixPR, repoDir, branch, base, remote string, run CmdRunner) PRResult {
	if base == "" {
		base = "main"
	}
	if remote == "" {
		remote = "origin"
	}
	if run == nil {
		run = DefaultCmdRunner
	}
	if fix.IsNoop() {
		if fix.RepairFailedReason != "" {
			return PRResult{Detail: "no fix was produced, so the violation is UNRESOLVED — " + fix.RepairFailedReason + ". This is NOT a statement that the manifest complies."}
		}
		return PRResult{Detail: "no change to propose — the repair step ran and the manifest needed no edit"}
	}
	if rc, out := run([]string{"git", "-C", repoDir, "push", "-u", remote, branch}); rc != 0 {
		return PRResult{Detail: "branch push failed: " + out}
	}
	rc, out := run([]string{"gh", "pr", "create", "--title", fix.Title, "--body", fix.Rationale, "--base", base, "--head", branch})
	if rc == 0 {
		return PRResult{Pushed: true, Opened: true, Detail: strings.TrimSpace(out)}
	}
	first := "not found"
	if out != "" {
		first = strings.Split(out, "\n")[0]
	}
	return PRResult{Pushed: true, Detail: fmt.Sprintf("branch pushed to %s/%s; open a PR against %s manually (gh unavailable: %s)", remote, branch, base, first)}
}
