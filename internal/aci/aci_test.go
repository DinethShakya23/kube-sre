package aci

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/llm"
)

var ctx = context.Background()

func cfgOf(env map[string]string) *config.Config {
	return config.Load(func(k string) string { return env[k] })
}

type fake struct {
	calls []string
	stdin []string
	reply func(cmd string) string
}

func (f *fake) run(_ context.Context, cmd, stdin string) string {
	f.calls = append(f.calls, cmd)
	f.stdin = append(f.stdin, stdin)
	if f.reply != nil {
		return f.reply(cmd)
	}
	return "ok"
}

func reply(s string) *fake { return &fake{reply: func(string) string { return s }} }

// gateOf is a gate driven by the same two settings the real budget reads.
func gateOf(env map[string]string) Gate {
	c := cfgOf(env)
	return func() (bool, string) {
		switch {
		case c.V5KillSwitch:
			return false, "kill switch engaged"
		case c.V5ChangeFreeze:
			return false, "change freeze in effect"
		}
		return true, ""
	}
}

func TestClassifyOutputMatchesLinePrefixesNotSubstrings(t *testing.T) {
	cases := map[string]string{
		"":                                       FailedO,
		"(no output)":                            FailedO,
		"[Protected] Access to kube-system":      Refused,
		"[Permission denied] role readonly":      Refused,
		"[Unsupported] verb":                     Refused,
		"error: the server doesn't have":         FailedO,
		"Error from server (NotFound)":           FailedO,
		"[kubectl exited 1] boom":                FailedO,
		"Unable to connect to the server: x":     FailedO,
		"NAME  READY\nerror-budget-exporter 1/1": OK,
		"deployment.apps/web scaled":             OK,
		"note: error: not at line start\nfine":   OK,
	}
	for in, want := range cases {
		if got := ClassifyOutput(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
	if ClassifyOutput("\n\n[Protected] x") != Refused {
		t.Error("leading blank lines are skipped")
	}
}

func TestReachedClusterIsFalseForRefusalsAndUnreachable(t *testing.T) {
	for _, s := range []string{"", "(no output)", "[Protected] x", "[kubectl exited 1] The connection to the server was refused", "[Blocked] x"} {
		if ReachedCluster(s) {
			t.Errorf("%q never reached the server", s)
		}
	}
	for _, s := range []string{"deployment.apps/web created (server dry run)", "Error from server (Forbidden): denied"} {
		if !ReachedCluster(s) {
			t.Errorf("%q did reach the server", s)
		}
	}
}

func TestIsReadOnlyDelegatesAndFailsClosed(t *testing.T) {
	for cmd, want := range map[string]bool{
		"kubectl get pods -n shop":              true,
		"kubectl describe pod web":              true,
		"kubectl logs web --tail=10":            true,
		"kubectl diff -f -":                     true,
		"kubectl rollout history deploy/web":    true,
		"kubectl rollout status deploy/web":     true,
		"kubectl rollout restart deploy/web":    false,
		"kubectl rollout undo deploy/web":       false,
		"kubectl delete pod web":                false,
		"kubectl scale deploy web --replicas=1": false,
		"kubectl frobnicate":                    false,
		"":                                      false,
		"get pods":                              true,
		"kubectl 'unterminated":                 false,
	} {
		if got := IsReadOnly(cmd); got != want {
			t.Errorf("%q: %v", cmd, got)
		}
	}
}

func TestNormalizeKRMStripsServerNoise(t *testing.T) {
	in := "apiVersion: v1\nmetadata:\n  name: web\n  uid: abc\n  resourceVersion: \"9\"\n  managedFields:\n  - manager: kubectl\n    time: x\n  annotations:\n    kubectl.kubernetes.io/last-applied-configuration: |\n      {}\n  labels:\n    app: web\nspec:\n  replicas: 2"
	got := NormalizeKRM(in)
	for _, gone := range []string{"uid:", "resourceVersion", "managedFields", "manager: kubectl", "last-applied"} {
		if strings.Contains(got, gone) {
			t.Errorf("%s survived:\n%s", gone, got)
		}
	}
	for _, kept := range []string{"name: web", "labels:", "app: web", "replicas: 2"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%s was lost:\n%s", kept, got)
		}
	}
}

func TestWindowNeverSplitsALineAndGivesACursor(t *testing.T) {
	body := strings.Repeat("0123456789\n", 10)
	got, total, shown, next := Window(body, 4, 1000, 0)
	if total != 10 || shown != 4 || next == nil || *next != 4 || strings.Count(got, "\n") != 3 {
		t.Errorf("%q %d %d %v", got, total, shown, next)
	}
	got, _, shown, next = Window(body, 100, 25, 0)
	if shown != 2 || next == nil || *next != 2 || len(got) > 25 {
		t.Errorf("char cap: %q %d %v", got, shown, next)
	}
	if _, _, shown, next = Window(body, 4, 1000, 8); shown != 2 || next != nil {
		t.Errorf("last page: %d %v", shown, next)
	}
	long := strings.Repeat("x", 100)
	if got, _, shown, next = Window(long+"\nshort", 5, 10, 0); shown != 1 || next != nil || len(got) != 10 {
		t.Errorf("a single oversized line is cut, not lost: %q %d", got, shown)
	}
	if _, total, shown, next = Window("", 5, 10, 0); total != 0 || shown != 0 || next != nil {
		t.Errorf("%d %d", total, shown)
	}
}

func TestHealthReadsWholeFieldsOnly(t *testing.T) {
	for body, want := range map[string]Health{
		"web-1 Running":                    Current,
		"web-1 CrashLoopBackOff":           Failed,
		"web-1 Pending":                    InProgress,
		"web-1 Terminating":                Terminating,
		"error-budget-exporter Running":    Current,
		"Status: Failed (Reason: x)":       Failed,
		"nothing recognisable":             Unknown,
		"Terminating and CrashLoopBackOff": Terminating,
	} {
		if got := healthFrom(body); got != want {
			t.Errorf("%q: %s, want %s", body, got, want)
		}
	}
}

func verbs(f *fake) Verbs { return Verbs{Run: f.run, Limits: Limits{MaxLines: 5, MaxChars: 500}} }

func TestVerbsBuildReadOnlyCommands(t *testing.T) {
	f := reply("NAME READY STATUS\nweb-1 1/1 Running")
	v := verbs(f)
	r, err := v.Inspect(ctx, "deployment", "web", "shop", "")
	if err != nil || f.calls[0] != "kubectl describe deployment web -n shop" || r.Health != Current || !strings.HasPrefix(r.Render(), "[aci:inspect] deployment/web in shop — health=Current") {
		t.Errorf("%v %v %s", err, f.calls, r.Render())
	}
	v.Inspect(ctx, "pod", "web-1", "", "full")
	v.Search(ctx, []string{"pods", "svc"}, "", true, "app=web", 0)
	v.Logs(ctx, "shop", "web-1", "", "app", 50, "5m", true)
	v.Logs(ctx, "shop", "", "app=web", "", 0, "", false)
	v.DiffChange(ctx, "previous", "deploy", "web", "shop", "", 3)
	want := []string{
		"kubectl get pod web-1 -o yaml",
		"kubectl get pods,svc --all-namespaces -l app=web -o wide",
		"kubectl logs web-1 -n shop --tail=5 -c app --since=5m --previous",
		"kubectl logs -l app=web -n shop --tail=5",
		"kubectl rollout history deploy/web -n shop --revision=3",
	}
	if strings.Join(f.calls[1:], "\n") != strings.Join(want, "\n") {
		t.Errorf("\n%s", strings.Join(f.calls[1:], "\n"))
	}
}

func TestVerbOutputIsNeverSilentAndRefusalsAreNotContent(t *testing.T) {
	r, _ := verbs(reply("No resources found in shop namespace.")).Search(ctx, []string{"pods"}, "shop", false, "", 0)
	if !r.Empty || !strings.Contains(r.Render(), "ran successfully, no matching results") {
		t.Errorf("%s", r.Render())
	}
	r, _ = verbs(reply("[Protected] Access to namespace 'kube-system' is not permitted")).Search(ctx, []string{"pods"}, "kube-system", false, "", 0)
	if r.OK || !strings.Contains(r.Render(), "FAILED") || !strings.Contains(r.Render(), "[Protected]") {
		t.Errorf("a refused read is a failure, not an empty result: %s", r.Render())
	}
	r, _ = verbs(reply(strings.Repeat("row\n", 50))).Search(ctx, []string{"pods"}, "", false, "", 0)
	if !strings.Contains(r.Render(), "showing 5/50 lines (next offset 5)") {
		t.Errorf("%s", r.Render())
	}
	if r, _ = verbs(reply("x")).Logs(ctx, "shop", "", "", "", 0, "", false); !strings.Contains(r.Render(), "requires either 'pod' or 'selector'") {
		t.Errorf("%s", r.Render())
	}
	if r, _ = verbs(reply("x")).Search(ctx, nil, "", false, "", 0); r.Error == "" {
		t.Error("no kinds")
	}
}

func TestDiffChange(t *testing.T) {
	f := reply("--- live\n+++ new\n-replicas: 1\n+replicas: 2")
	r, _ := verbs(f).DiffChange(ctx, "live", "deploy", "web", "shop", "kind: Deployment", 0)
	if !r.OK || f.calls[0] != "kubectl diff -f -" || f.stdin[0] != "kind: Deployment" || !strings.Contains(r.Render(), "(live diff)") {
		t.Errorf("%v %v %s", f.calls, f.stdin, r.Render())
	}
	if r, _ = verbs(f).DiffChange(ctx, "git", "deploy", "web", "", "", 0); !strings.Contains(r.Render(), "against=git is unsupported") {
		t.Error("git is declined, never silently ignored")
	}
	if r, _ = verbs(f).DiffChange(ctx, "live", "deploy", "web", "", "", 0); !strings.Contains(r.Render(), "requires a 'manifest'") {
		t.Errorf("%s", r.Render())
	}
	if r, _ = verbs(f).DiffChange(ctx, "sideways", "deploy", "web", "", "", 0); r.Error == "" {
		t.Error("unknown mode")
	}
	if r, _ = verbs(reply("")).DiffChange(ctx, "live", "deploy", "web", "", "kind: x", 0); r.Empty {
		// an empty run classifies as failed output only for the get path; a diff with no output is "no differences"
	}
}

func TestCallDispatchIsAllowlistedAndSpecsMatch(t *testing.T) {
	v := verbs(reply("NAME\nweb-1 Running"))
	if out := v.Call(ctx, "delete_everything", nil); !strings.HasPrefix(out, "[blocked]") {
		t.Errorf("%s", out)
	}
	if out := v.Call(ctx, "search", map[string]any{"kinds": []any{"pods"}, "namespace": "shop"}); !strings.Contains(out, "[aci:search] pods in shop") {
		t.Errorf("%s", out)
	}
	if out := v.Call(ctx, "logs", map[string]any{"namespace": "shop", "pod": "web-1", "lines": float64(20), "previous": true}); !strings.Contains(out, "(previous)") {
		t.Errorf("%s", out)
	}
	specs := Specs()
	allow := ReadVerbAllowlist()
	if len(specs) != 4 || len(allow) != 4 {
		t.Fatalf("%d %d", len(specs), len(allow))
	}
	for _, s := range specs {
		if !allow[s.Name] {
			t.Errorf("%s is not on the allowlist", s.Name)
		}
	}
}

func TestClassifyRollback(t *testing.T) {
	for cmd, want := range map[string]string{
		"kubectl scale deploy web --replicas=2": VersionedWorkload,
		"rollout restart deploy/web":            VersionedWorkload,
		"set image deploy/web web=v2":           VersionedWorkload,
		"kubectl delete pod web-1":              VersionedWorkload,
		"delete namespace shop":                 Irreversible,
		"delete pvc data":                       Irreversible,
		"delete statefulset db":                 Irreversible,
		"apply -f x.yaml":                       DeclarativeRevert,
		"patch deploy web -p {}":                DeclarativeRevert,
		"label pod web a=b":                     DeclarativeRevert,
		"frobnicate":                            Irreversible,
		"":                                      Irreversible,
	} {
		if got := ClassifyRollback(cmd); got != want {
			t.Errorf("%q: %s, want %s", cmd, got, want)
		}
	}
}

func TestDecideWrite(t *testing.T) {
	open := gateOf(nil)
	if p := DecideWrite(open, "delete namespace shop", "L4"); p.Decision != "approve" || !strings.Contains(p.Reason, "irreversible") {
		t.Errorf("irreversible is never auto, whatever the rung: %+v", p)
	}
	if p := DecideWrite(open, "scale deploy web --replicas=2", "L4"); p.Decision != "auto" {
		t.Errorf("%+v", p)
	}
	if p := DecideWrite(open, "scale deploy web --replicas=2", "L2"); p.Decision != "approve" || !strings.Contains(p.Reason, "rung L2 < L4") {
		t.Errorf("%+v", p)
	}
	stopped := gateOf(map[string]string{"KI_V5_KILL_SWITCH": "true"})
	if p := DecideWrite(stopped, "scale deploy web --replicas=2", "L4"); p.Decision != "deny" || p.Reason != "kill switch engaged" {
		t.Errorf("a brake denies even an earned L4: %+v", p)
	}
}

func TestServerDryRunIsForcedOnWholeTokens(t *testing.T) {
	for in, want := range map[string]string{
		"apply -f x.yaml":                  "apply -f x.yaml --dry-run=server",
		"apply -f x.yaml --dry-run=client": "apply -f x.yaml --dry-run=server",
		"apply -f x.yaml --dry-run=none":   "apply -f x.yaml --dry-run=server",
		"apply -f x.yaml --dry-run":        "apply -f x.yaml --dry-run=server",
		"apply --dry-run client -f x.yaml": "apply -f x.yaml --dry-run=server",
		"apply -f x.yaml --dry-run=server": "apply -f x.yaml --dry-run=server",
		"label deploy/web team=--dry-run":  "label deploy/web team=--dry-run --dry-run=server",
	} {
		if got := withServerDryRun(in); got != want {
			t.Errorf("%q\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestValidateMutationSeparatesRejectedFromNotValidated(t *testing.T) {
	ok := ValidateMutation(ctx, reply("deployment.apps/web configured (server dry run)").run, "apply -f x")
	if !ok.OK || !ok.Validated || ok.AdmissionDenied {
		t.Errorf("%+v", ok)
	}
	denied := ValidateMutation(ctx, reply(`Error from server (Forbidden): admission webhook "policy" denied the request`).run, "apply -f x")
	if denied.OK || !denied.AdmissionDenied || !denied.Validated {
		t.Errorf("%+v", denied)
	}
	invalid := ValidateMutation(ctx, reply("Error from server (BadRequest): spec.replicas is invalid").run, "apply -f x")
	if invalid.OK || !invalid.AdmissionDenied {
		t.Errorf("%+v", invalid)
	}
	for _, out := range []string{"[Protected] no", "[kubectl exited 1] The connection to the server localhost:8080 was refused", ""} {
		nv := ValidateMutation(ctx, reply(out).run, "apply -f x")
		if nv.Validated || nv.OK || !strings.HasPrefix(nv.Output, "not validated:") {
			t.Errorf("%q must not read as validated: %+v", out, nv)
		}
	}
	f := reply("configured")
	ValidateMutation(ctx, f.run, "apply -f x.yaml --dry-run=client")
	if f.calls[0] != "apply -f x.yaml --dry-run=server" {
		t.Errorf("%v", f.calls)
	}
}

func TestPlanMutationOnlyValidatesAuthorisedWrites(t *testing.T) {
	f := reply("scaled (server dry run)")
	stopped := gateOf(map[string]string{"KI_V5_CHANGE_FREEZE": "true"})
	p, dr := PlanMutation(ctx, f.run, stopped, "scale deploy web --replicas=2", "L4")
	if p.Decision != "deny" || dr != nil || len(f.calls) != 0 {
		t.Errorf("a denied write is never even validated: %+v %v %v", p, dr, f.calls)
	}
	open := gateOf(nil)
	p, dr = PlanMutation(ctx, f.run, open, "scale deploy web --replicas=2", "L4")
	if p.Decision != "auto" || dr == nil || !dr.OK {
		t.Errorf("%+v %+v", p, dr)
	}
	p, dr = PlanMutation(ctx, reply("[Protected] x").run, open, "scale deploy web --replicas=2", "L4")
	if p.Decision != "approve" || dr.Validated || !strings.Contains(p.Reason, "server-side dry-run never ran") {
		t.Errorf("auto is downgraded when there is no evidence the server would accept it: %+v", p)
	}
}

func TestDeploymentReadyOracleKeepsUnreadableApartFromNotReady(t *testing.T) {
	for _, tc := range []struct {
		out       string
		met, eval bool
		detail    string
	}{
		{"NAME READY UP-TO-DATE\nweb 3/3 3", true, true, "3/3 ready"},
		{"NAME READY UP-TO-DATE\nweb 1/3 1", false, true, "1/3 ready"},
		{"NAME READY UP-TO-DATE\nweb 0/0 0", false, true, "0/0 ready"},
		{"NAME READY UP-TO-DATE\nother 1/1 1", false, true, "not found"},
		{"[Protected] Access to namespace 'kube-system' is not permitted", false, false, "could not read"},
		{"[kubectl exited 1] The connection to the server was refused", false, false, "could not read"},
		{"", false, false, "could not read"},
	} {
		got := DeploymentReady(ctx, reply(tc.out).run, "web", "prod")
		if got.Met != tc.met || got.Evaluated != tc.eval || !strings.Contains(got.Detail, tc.detail) {
			t.Errorf("%q: %+v", tc.out, got)
		}
	}
	if r, d, ok := ParseReadyColumn("web 2/5 x", "web"); !ok || r != 2 || d != 5 {
		t.Error("parse")
	}
	if _, _, ok := ParseReadyColumn("web x/y", "web"); ok {
		t.Error("non numeric")
	}
}

func oracle(met, evaluated bool) func() Postcondition {
	return func() Postcondition { return Postcondition{Met: met, Evaluated: evaluated} }
}

func applier(script map[string]string) (ApplyFn, *[]string) {
	var ran []string
	return func(c string) string {
		ran = append(ran, c)
		return script[c]
	}, &ran
}

func TestExecuteTransactionalEveryStatus(t *testing.T) {
	cases := []struct {
		name   string
		script map[string]string
		oracle func() Postcondition
		rb     string
		want   string
		ran    int
	}{
		{"committed", map[string]string{"fix": "scaled"}, oracle(true, true), "undo", Committed, 1},
		{"refused: nothing to roll back", map[string]string{"fix": "[Protected] no"}, oracle(false, true), "undo", ApplyRefused, 1},
		{"kubectl rejected", map[string]string{"fix": "Error from server (Forbidden)"}, oracle(false, true), "undo", ApplyFailed, 1},
		{"no output is a failure to apply", map[string]string{"fix": ""}, oracle(true, true), "", ApplyFailed, 1},
		{"oracle blind: escalate, no rollback", map[string]string{"fix": "scaled"}, oracle(false, false), "undo", VerifyInconclusive, 1},
		{"failed, nothing to roll back with", map[string]string{"fix": "scaled"}, oracle(false, true), "", VerifyFailedNoRollback, 1},
		{"rolled back and confirmed", map[string]string{"fix": "scaled", "undo": "rolled back"}, oracle(false, true), "undo", RolledBack, 2},
		{"rollback refused", map[string]string{"fix": "scaled", "undo": "[Protected] no"}, oracle(false, true), "undo", RollbackRefused, 2},
		{"rollback rejected", map[string]string{"fix": "scaled", "undo": "[kubectl exited 1] Error from server (Forbidden)"}, oracle(false, true), "undo", RollbackFailed, 2},
		{"rollback silent", map[string]string{"fix": "scaled", "undo": "(no output)"}, oracle(false, true), "undo", RollbackUnconfirmed, 2},
		{"rollback unreachable", map[string]string{"fix": "scaled", "undo": "Unable to connect to the server"}, oracle(false, true), "undo", RollbackFailed, 2},
	}
	for _, c := range cases {
		apply, ran := applier(c.script)
		got := ExecuteTransactional("fix", c.oracle, c.rb, apply)
		if got.Status != c.want || len(*ran) != c.ran {
			t.Errorf("%s: %s after %v, want %s", c.name, got.Status, *ran, c.want)
		}
	}
	apply, _ := applier(map[string]string{"fix": "scaled", "undo": "reverted"})
	if got := ExecuteTransactional("fix", oracle(false, true), "undo", apply); got.RollbackOutput != "reverted" || got.Postcondition == nil || got.ApplyOutput != "scaled" {
		t.Errorf("%+v", got)
	}
}

func TestSandboxFailsClosed(t *testing.T) {
	sb := Sandbox{Cfg: cfgOf(nil)}
	var seen, id string
	run := func(c, i string) string { seen, id = c, i; return "ran" }

	out, err := sb.RunAs("get pods -n shop", RoleReadOnly, run)
	if err != nil || out != "ran" || seen != "get pods -n shop --as=system:serviceaccount:kube-sre:ki-readonly" || id != "--as=system:serviceaccount:kube-sre:ki-readonly" {
		t.Errorf("%v %q %q", err, out, seen)
	}
	sb.RunAs("scale deploy web --replicas=1", RoleNamespaceWrite, run)
	if !strings.HasSuffix(seen, "serviceaccount:kube-sre:ki-writer") {
		t.Errorf("%s", seen)
	}
	sb.RunAs("get pods", RoleNeverAdmin, run)
	if !strings.HasSuffix(seen, "ki-readonly") {
		t.Errorf("never-cluster-admin reuses the read only account: %s", seen)
	}

	seen = ""
	for _, role := range []string{"readonly", "operator", "typo", ""} {
		if _, err := sb.RunAs("delete deployment web -n prod", role, run); err == nil || !strings.Contains(err.Error(), "nothing was run") {
			t.Errorf("role %q must refuse: %v", role, err)
		}
	}
	for _, cmd := range []string{"get pods --as-group=system:masters", "get pods --as=admin", "get pods --as-uid=0"} {
		if _, err := sb.RunAs(cmd, RoleReadOnly, run); err == nil || !strings.Contains(err.Error(), "sets its own identity") {
			t.Errorf("%q: %v", cmd, err)
		}
	}
	if seen != "" {
		t.Errorf("nothing may run on a refusal, ran %q", seen)
	}
	if sb.ImpersonationArgs("readonly") != "" {
		t.Error("an unknown role has no flags")
	}
	custom := Sandbox{Cfg: cfgOf(map[string]string{"KI_V5_SANDBOX_SA_NAMESPACE": "ops", "KI_V5_SANDBOX_READONLY_SA": "ro"})}
	if custom.ImpersonationArgs(RoleReadOnly) != "--as=system:serviceaccount:ops:ro" {
		t.Errorf("%s", custom.ImpersonationArgs(RoleReadOnly))
	}
}

func TestUnifiedDiff(t *testing.T) {
	orig := "apiVersion: v1\nkind: Pod\nspec:\n  containers:\n  - name: a\n    securityContext:\n      privileged: true\n    image: x\n"
	fixed := strings.Replace(orig, "privileged: true", "privileged: false", 1)
	d := UnifiedDiff("pod.yaml", orig, fixed)
	for _, want := range []string{"--- a/pod.yaml\n+++ b/pod.yaml\n", "@@ -", "-      privileged: true\n", "+      privileged: false\n", "   containers:\n"} {
		if !strings.Contains(d, want) {
			t.Errorf("missing %q in\n%s", want, d)
		}
	}
	pr := MakeFixPR("pod.yaml", orig, fixed, "", "drop privileged", "")
	if pr.IsNoop() || pr.AddedLines() != 1 || pr.RemovedLines() != 1 || pr.Title != "fix: pod.yaml" || pr.RollbackClass != DeclarativeRevert {
		t.Errorf("%+v", pr)
	}
	if d := UnifiedDiff("p", orig, strings.TrimRight(orig, "\n")); d != "" {
		t.Errorf("a trailing newline is not a change to a manifest:\n%s", d)
	}
	if UnifiedDiff("p", orig, orig) != "" || UnifiedDiff("p", "", "") != "" {
		t.Error("identical is empty")
	}
	if d := UnifiedDiff("p", "", "kind: Pod\n"); !strings.Contains(d, "+kind: Pod") || !strings.Contains(d, "@@ -0,0 +1 @@") {
		t.Errorf("%s", d)
	}
	far := strings.Repeat("keep\n", 30)
	twoHunks := UnifiedDiff("p", "a\n"+far+"b\n", "A\n"+far+"B\n")
	if strings.Count(twoHunks, "@@ -") != 2 {
		t.Errorf("distant edits are separate hunks:\n%s", twoHunks)
	}
}

type reviewer string

func (r reviewer) Chat(context.Context, []llm.Message, []llm.ToolSpec, llm.Options) (*llm.Response, error) {
	return &llm.Response{Message: llm.Message{Content: string(r)}}, nil
}

type broken struct{}

func (broken) Chat(context.Context, []llm.Message, []llm.ToolSpec, llm.Options) (*llm.Response, error) {
	return nil, errors.New("model down")
}

func TestProposeFixSaysWhenItDidNotRun(t *testing.T) {
	m := "kind: Pod\nspec: {}\n"
	got := ProposeFix(ctx, reviewer("```yaml\nkind: Pod\nspec:\n  hostNetwork: false\n```"), m, "hostNetwork")
	if !got.Repaired || strings.Contains(got.Manifest, "```") || !strings.Contains(got.Manifest, "hostNetwork: false") {
		t.Errorf("%+v", got)
	}
	for name, model := range map[string]llm.Model{"empty": reviewer("  "), "prose": reviewer("I cannot help with that."), "error": broken{}} {
		g := ProposeFix(ctx, model, m, "v")
		if g.Repaired || g.Manifest != m || g.Reason == "" {
			t.Errorf("%s: %+v", name, g)
		}
	}
}

func TestOpenPRNeverMistakesAFailedRepairForACompliantManifest(t *testing.T) {
	var ran [][]string
	run := func(script map[string]int) CmdRunner {
		return func(argv []string) (int, string) {
			ran = append(ran, argv)
			return script[argv[0]], "out from " + argv[0] + "\nsecond line"
		}
	}
	fix := MakeFixPR("p.yaml", "kind: Pod\na: 1\n", "kind: Pod\na: 2\n", "Fix", "why", "")

	res := OpenPR(fix, "/repo", "fix/x", "", "", run(nil))
	if !res.Pushed || !res.Opened || len(ran) != 2 || strings.Join(ran[0], " ") != "git -C /repo push -u origin fix/x" || ran[1][0] != "gh" || ran[1][len(ran[1])-3] != "main" {
		t.Errorf("%+v %v", res, ran)
	}
	ran = nil
	if res = OpenPR(fix, "/repo", "b", "", "", run(map[string]int{"gh": 1})); !res.Pushed || res.Opened || !strings.Contains(res.Detail, "open a PR against main manually") || !strings.Contains(res.Detail, "out from gh") {
		t.Errorf("%+v", res)
	}
	if res = OpenPR(fix, "/repo", "b", "", "", run(map[string]int{"git": 1})); res.Pushed || !strings.HasPrefix(res.Detail, "branch push failed") {
		t.Errorf("%+v", res)
	}

	ran = nil
	compliant := MakeFixPR("p.yaml", "kind: Pod\n", "kind: Pod\n", "", "", "")
	if res = OpenPR(compliant, "/r", "b", "", "", run(nil)); res.Pushed || len(ran) != 0 || !strings.Contains(res.Detail, "the manifest needed no edit") {
		t.Errorf("%+v", res)
	}
	failed := MakeFixPR("p.yaml", "kind: Pod\n", "kind: Pod\n", "", "", "the model returned an empty response")
	res = OpenPR(failed, "/r", "b", "", "", run(nil))
	if res.Pushed || len(ran) != 0 || !strings.Contains(res.Detail, "UNRESOLVED") || !strings.Contains(res.Detail, "NOT a statement that the manifest complies") || strings.Contains(res.Detail, "needed no edit") {
		t.Errorf("a repair that never ran must not read as 'nothing to do': %+v", res)
	}
}
