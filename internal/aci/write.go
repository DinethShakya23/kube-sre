package aci

import (
	"context"
	"fmt"
	"strings"

	"github.com/DinethShakya23/kube-sre/internal/config"
)

// ── the mutation chokepoint ──────────────────────────────────────────────────
//
// A proposed cluster mutation is stamped with a rollback class (how reversible it is)
// and routed through one write authority decision that composes the blast radius gate,
// the class's earned rung and reversibility, before anything executes.
//
// This is a designed destination and not the live brake: the A3 path today goes
// through the watchtower (ladder, allowlist and the auto write gate), and earned_rung
// arrives as its L2 default because nothing computes a rung for this signature.

const (
	VersionedWorkload = "versioned-workload" // revert via a controller rollout undo
	DeclarativeRevert = "declarative-revert" // revert by re-applying the prior manifest
	Irreversible      = "irreversible"       // no safe automatic revert, never auto
)

var (
	versionedVerbs     = map[string]bool{"scale": true, "rollout": true, "set": true, "autoscale": true}
	declarativeVerbs   = map[string]bool{"apply": true, "patch": true, "edit": true, "replace": true, "label": true, "annotate": true}
	irreversibleTarget = []string{"pvc", "persistentvolumeclaim", "pv", "persistentvolume", "namespace", "ns", "crd", "customresourcedefinition", "statefulset", "sts"}
)

// ClassifyRollback classifies a mutating command by how safely it can be reverted.
// Unparseable and unknown mutations are treated as unsafe.
func ClassifyRollback(command string) string {
	toks := strings.Fields(strings.TrimSpace(command))
	if len(toks) > 0 && toks[0] == "kubectl" {
		toks = toks[1:]
	}
	if len(toks) == 0 {
		return Irreversible
	}
	verb, target := strings.ToLower(toks[0]), strings.ToLower(strings.Join(toks[1:], " "))
	switch {
	case verb == "delete":
		for _, t := range irreversibleTarget {
			if strings.Contains(target, t) {
				return Irreversible
			}
		}
		return VersionedWorkload
	case versionedVerbs[verb]:
		return VersionedWorkload
	case declarativeVerbs[verb]:
		return DeclarativeRevert
	}
	return Irreversible
}

// Proposal is the write authority decision for one mutation.
type Proposal struct {
	Command, RollbackClass string
	Decision               string // auto | approve | deny
	Reason                 string
}

// Gate is the blast radius and spend gate: the kill switch, the change freeze and the
// spend cap. It is a function so this package does not depend on the autonomy one.
type Gate func() (allow bool, reason string)

// DecideWrite composes the decision. A budget, kill switch or freeze denial denies. An
// irreversible mutation always needs a human, whatever the rung. Otherwise it is auto
// only if the class has earned L4 for this action, else approve.
func DecideWrite(gate Gate, command, earnedRung string) Proposal {
	rc := ClassifyRollback(command)
	if allow, reason := gate(); !allow {
		return Proposal{command, rc, "deny", reason}
	}
	switch {
	case rc == Irreversible:
		return Proposal{command, rc, "approve", "irreversible — human approval required"}
	case earnedRung == "L4":
		return Proposal{command, rc, "auto", "earned L4 for " + rc}
	}
	return Proposal{command, rc, "approve", fmt.Sprintf("rung %s < L4 — approval required", earnedRung)}
}

var admissionMarkers = []string{"denied the request", "admission webhook", "forbidden", "is invalid", "violat", "not allowed"}

// DryRun is the result of validating a mutation server side.
type DryRun struct {
	OK              bool // the command would apply cleanly
	AdmissionDenied bool // rejected by admission, not just a typo
	Output          string
	Validated       bool
}

// withServerDryRun forces --dry-run=server on a command, whatever dry-run flag it
// already carries. A substring test left the command untouched in cases where the
// server side validation this exists for would never happen: --dry-run=none (the
// documented default, so not a dry run at all), a bare --dry-run (client side, so
// admission is never consulted), --dry-run=client, and the string appearing inside an
// unrelated value. So it matches whole tokens and rewrites anything that is not
// already --dry-run=server.
func withServerDryRun(command string) string {
	toks := strings.Fields(command)
	var out []string
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t == "--dry-run":
			if i+1 < len(toks) && (toks[i+1] == "client" || toks[i+1] == "server" || toks[i+1] == "none") {
				i++
			}
		case strings.HasPrefix(t, "--dry-run="):
		default:
			out = append(out, t)
		}
	}
	return strings.Join(append(out, "--dry-run=server"), " ")
}

// ValidateMutation checks a mutation against the live API server and admission chain
// with --dry-run=server. No cluster change occurs.
func ValidateMutation(ctx context.Context, run Runner, command string) DryRun {
	out := run(ctx, withServerDryRun(command), "")
	if !ReachedCluster(out) {
		first := "(empty)"
		for _, ln := range strings.Split(out, "\n") {
			if strings.TrimSpace(ln) != "" {
				first = strings.TrimSpace(ln)
				break
			}
		}
		return DryRun{Output: "not validated: " + clip(first, 400)}
	}
	low := strings.ToLower(out)
	admission := false
	for _, m := range admissionMarkers {
		admission = admission || strings.Contains(low, m)
	}
	return DryRun{OK: !admission && ClassifyOutput(out) == OK, AdmissionDenied: admission, Output: clip(strings.TrimSpace(out), 2000), Validated: true}
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// PlanMutation is the full chokepoint: authorise, then dry run. The dry run happens
// only when the write is authorised, so a denied write is never even validated against
// the cluster. An auto decision is downgraded to approve when the dry run could not
// run at all: auto is earned against evidence that the API server would accept the
// command, and there is none.
func PlanMutation(ctx context.Context, run Runner, gate Gate, command, earnedRung string) (Proposal, *DryRun) {
	p := DecideWrite(gate, command, earnedRung)
	if p.Decision == "deny" {
		return p, nil
	}
	dr := ValidateMutation(ctx, run, command)
	if p.Decision == "auto" && !dr.Validated {
		p = Proposal{p.Command, p.RollbackClass, "approve", p.Reason + ", but the server-side dry-run never ran — approval required"}
	}
	return p, &dr
}

// ── postconditions ───────────────────────────────────────────────────────────

// Postcondition is a machine checkable health verdict. A mitigation is only
// successful if an oracle confirms it, which turns "I ran a fix" into "the fix worked".
//
// "Not ready" and "I could not look" are two different answers, and this oracle used
// to give the first for both. A read of a protected namespace answers [Protected], and
// the oracle turned that into "deployment not found" about a namespace it never
// looked at. Downstream the executor reads a failed postcondition as a failed
// mitigation and rolls back, so an instrument outage became a live mutation. Hence
// Evaluated: the oracle says when it has no observation at all, and the caller
// escalates instead of rolling back.
type Postcondition struct {
	Met            bool
	Ready, Desired int
	Detail         string
	Evaluated      bool
}

// ParseReadyColumn reads (ready, desired) from a "kubectl get deployment" row.
func ParseReadyColumn(out, name string) (ready, desired int, ok bool) {
	for _, line := range strings.Split(out, "\n") {
		cols := strings.Fields(line)
		if len(cols) >= 2 && cols[0] == name && strings.Contains(cols[1], "/") {
			var r, d int
			if n, _ := fmt.Sscanf(cols[1], "%d/%d", &r, &d); n == 2 {
				return r, d, true
			}
		}
	}
	return 0, 0, false
}

// DeploymentReady is the health oracle: is the deployment fully ready, with desired above zero?
func DeploymentReady(ctx context.Context, run Runner, name, namespace string) Postcondition {
	out := run(ctx, fmt.Sprintf("get deployment %s -n %s", name, namespace), "")
	if ClassifyOutput(out) != OK {
		first := "(empty)"
		for _, ln := range strings.Split(out, "\n") {
			if strings.TrimSpace(ln) != "" {
				first = strings.TrimSpace(ln)
				break
			}
		}
		return Postcondition{Detail: fmt.Sprintf("could not read deployment %q in %q: %s", name, namespace, clip(first, 200))}
	}
	r, d, ok := ParseReadyColumn(out, name)
	if !ok {
		return Postcondition{Evaluated: true, Detail: fmt.Sprintf("deployment %q not found in %q", name, namespace)}
	}
	return Postcondition{Met: d > 0 && r >= d, Ready: r, Desired: d, Detail: fmt.Sprintf("%d/%d ready", r, d), Evaluated: true}
}

// ── the transactional executor ───────────────────────────────────────────────

// Execution statuses.
const (
	Committed              = "committed"
	RolledBack             = "rolled_back"
	ApplyFailed            = "apply_failed"
	ApplyRefused           = "apply_refused"
	VerifyFailedNoRollback = "verify_failed_no_rollback"
	VerifyInconclusive     = "verify_inconclusive"
	RollbackRefused        = "rollback_refused"
	RollbackFailed         = "rollback_failed"
	RollbackUnconfirmed    = "rollback_unconfirmed"
)

// Execution is what a transactional mitigation did.
type Execution struct {
	Status         string
	Postcondition  *Postcondition
	ApplyOutput    string
	RollbackOutput string
}

// ApplyFn runs one command.
type ApplyFn func(command string) string

// ExecuteTransactional applies a command, verifies a postcondition oracle and rolls
// back automatically if it fails: a mitigation either commits or leaves the cluster as
// it was, never a half applied change nobody verified.
//
//   - refused by kube-sre: ApplyRefused, nothing ran and there is nothing to roll back;
//   - rejected by kubectl: ApplyFailed;
//   - the oracle could not evaluate: VerifyInconclusive, escalate, no rollback;
//   - the postcondition holds: Committed;
//   - it fails with a rollback command: run it, and RolledBack only if confirmed,
//     otherwise RollbackRefused, RollbackFailed or RollbackUnconfirmed (all escalate);
//   - it fails with no rollback command: VerifyFailedNoRollback, escalate.
//
// The refused branch is not a nicety. Every safety gate answers with a string, and a
// substring test read all of them as success: the executor then failed the oracle (of
// course, nothing had changed) and issued the rollback against the live cluster,
// undoing something that was never done. And the rollback is read the same way: it used
// to be issued and its result discarded, so a rollback that was refused, rejected,
// unreachable or silent all returned rolled_back, four ways to leave the cluster half
// applied while the audit trail said it had been restored. That is the one status a
// trust plane must be able to believe.
func ExecuteTransactional(command string, oracle func() Postcondition, rollbackCommand string, apply ApplyFn) Execution {
	out := apply(command)
	switch ClassifyOutput(out) {
	case Refused:
		return Execution{Status: ApplyRefused, ApplyOutput: clip(strings.TrimSpace(out), 2000)}
	case FailedO:
		return Execution{Status: ApplyFailed, ApplyOutput: clip(strings.TrimSpace(out), 2000)}
	}
	v := oracle()
	trim := clip(strings.TrimSpace(out), 2000)
	switch {
	case !v.Evaluated:
		return Execution{VerifyInconclusive, &v, trim, ""}
	case v.Met:
		return Execution{Committed, &v, trim, ""}
	case rollbackCommand == "":
		return Execution{VerifyFailedNoRollback, &v, trim, ""}
	}
	rb := apply(rollbackCommand)
	status := RolledBack
	switch ClassifyOutput(rb) {
	case Refused:
		status = RollbackRefused
	case FailedO:
		status = RollbackFailed
		if strings.TrimSpace(rb) == NoOut {
			status = RollbackUnconfirmed
		}
	}
	return Execution{status, &v, trim, clip(strings.TrimSpace(rb), 2000)}
}

// ── the capability sandbox ───────────────────────────────────────────────────
//
// The second approval axis: the agent acts through an impersonated ServiceAccount
// scoped to a capability role, so the cluster's own RBAC and not only the app's policy
// bounds what any action can touch. Even a bug that authorises a write cannot exceed
// the impersonated account's rights.

const (
	RoleReadOnly       = "read-only"
	RoleNamespaceWrite = "namespace-write"
	RoleNeverAdmin     = "never-cluster-admin"
)

var ValidRoles = []string{RoleReadOnly, RoleNamespaceWrite, RoleNeverAdmin}

// SandboxError is a request the sandbox cannot bound. It is never a cluster call.
type SandboxError struct{ Msg string }

func (e *SandboxError) Error() string { return e.Msg }

// Sandbox builds impersonation flags from configuration.
type Sandbox struct{ Cfg *config.Config }

func validRole(role string) bool {
	for _, r := range ValidRoles {
		if r == role {
			return true
		}
	}
	return false
}

// ServiceAccount is the account for a role. never-cluster-admin reuses the read only
// account: its defining property is the absence of cluster-admin, enforced by RBAC.
func (s Sandbox) ServiceAccount(role string) string {
	if role == RoleNamespaceWrite {
		return s.Cfg.V5SandboxWriterSA
	}
	return s.Cfg.V5SandboxReadonlySA
}

// ImpersonationArgs are the kubectl --as flags for a role, empty for an unknown role.
func (s Sandbox) ImpersonationArgs(role string) string {
	if !validRole(role) {
		return ""
	}
	return fmt.Sprintf("--as=system:serviceaccount:%s:%s", s.Cfg.V5SandboxSANamespace, s.ServiceAccount(role))
}

func ownIdentityFlag(command string) string {
	for _, t := range strings.Fields(command) {
		name, _, _ := strings.Cut(t, "=")
		if name == "--as" || name == "--as-group" || name == "--as-uid" {
			return t
		}
	}
	return ""
}

// RunAs runs a command impersonating the role's ServiceAccount, with approval
// bypassed: in the sandbox the account's RBAC is the guard, so an app level prompt is
// redundant. Which is why it fails closed and raises instead of degrading. Both halves
// of that trade must hold: an unknown role, or a command that sets its own identity,
// means the cluster side guard is not there and the app side one has already been
// given up. An unknown role used to run the command unimpersonated with approval
// bypassed. The role vocabulary makes that easy to hit by accident: these roles are
// read-only, namespace-write and never-cluster-admin, while the API key roles are
// readonly, operator, admin and superadmin, so passing "readonly" silently turned the
// sandbox off. And a command may not bring its own identity: --as-group=system:masters
// would defeat never-cluster-admin whatever the --as flag says.
func (s Sandbox) RunAs(command, role string, run func(command, identity string) string) (string, error) {
	if !validRole(role) {
		return "", &SandboxError{fmt.Sprintf("unknown capability role %q — nothing was run. Valid roles are %s. (The API-key roles readonly/operator/admin/superadmin "+
			"are a different vocabulary and are not accepted here.)", role, strings.Join(ValidRoles, ", "))}
	}
	if own := ownIdentityFlag(command); own != "" {
		return "", &SandboxError{fmt.Sprintf("command sets its own identity (%q) — nothing was run. The sandbox decides who a command is; --as-group=system:masters "+
			"would defeat the never-cluster-admin property whatever the --as flag says.", own)}
	}
	flags := s.ImpersonationArgs(role)
	sandboxed := strings.TrimRight(command, " ") + " " + flags
	if !strings.Contains(sandboxed, "--as=") {
		return "", &SandboxError{fmt.Sprintf("impersonation flags were not applied to %q", command)}
	}
	return run(sandboxed, flags), nil
}
