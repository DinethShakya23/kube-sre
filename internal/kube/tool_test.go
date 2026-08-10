package kube

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/nsguard"
)

type rec struct{ events []map[string]any }

func (r *rec) Record(episode, kind string, payload map[string]any) {
	p := map[string]any{"_episode": episode, "_kind": kind}
	for k, v := range payload {
		p[k] = v
	}
	r.events = append(r.events, p)
}

// fake builds a Tool whose kubectl is a shell script. Every call appends its
// arguments to the log file so tests can see what actually reached kubectl.
func fake(t *testing.T, body string) (*Tool, string, *rec) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n" + body + "\n"
	bin := filepath.Join(dir, "kubectl")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &rec{}
	tool := NewTool(Config{
		Bin:               bin,
		Timeout:           5 * time.Second,
		BlockedNamespaces: nsguard.Blocklist{"kube-system": true, "kube-sre": true, "monitoring": true},
		BlockedResources:  map[string]bool{"secret": true, "secrets": true, "serviceaccount": true, "serviceaccounts": true},
		ErrorHints:        true,
		Recorder:          r,
	})
	return tool, log, r
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func run(t *testing.T, tool *Tool, cmd, stdin string, c Call) string {
	t.Helper()
	out, err := tool.Run(context.Background(), cmd, stdin, c, func(Approval) bool { return true })
	if err != nil {
		return "ERR: " + err.Error()
	}
	return out
}

func TestReadRunsAndOutputPassesThrough(t *testing.T) {
	tool, log, _ := fake(t, `echo "NAME READY"; echo "web 1/1"`)
	out := run(t, tool, "get pods -n shop", "", Call{})
	if out != "NAME READY\nweb 1/1\n" {
		t.Errorf("%q", out)
	}
	if c := calls(t, log); len(c) != 1 || c[0] != "get pods -n shop" {
		t.Errorf("kubectl saw %q", c)
	}
	// a leading kubectl is optional, and a doubled one is not stripped
	if out := run(t, tool, "kubectl get pods", "", Call{}); !strings.Contains(out, "web") {
		t.Errorf("%q", out)
	}
}

func TestReadonlyRoleCannotWriteBySpelling(t *testing.T) {
	tool, log, _ := fake(t, `echo ok`)
	denied := []string{
		"delete pod x -n shop", "-n shop delete pod x", "label pod x a=b -n shop", "annotate pod x a=b -n shop",
		"rollout restart deploy/x -n shop", "rollout undo deploy/x -n shop", "cp a b", "debug node/x", "expose deploy x",
		"autoscale deploy x", "port-forward pod/x 80", "attach x", "auth reconcile -f -", "scale deploy x --replicas=3 -n shop",
		"--warnings-as-errors delete pod x -n shop", "get pods delete", "apply -f -",
	}
	for _, c := range denied {
		out := run(t, tool, c, "", Call{Role: "readonly"})
		if !strings.HasPrefix(out, "[Permission Denied]") && !strings.HasPrefix(out, "ERR") {
			t.Errorf("readonly ran %q: %s", c, out)
		}
	}
	if got := calls(t, log); len(got) != 0 {
		t.Errorf("nothing may reach kubectl: %v", got)
	}
	for _, c := range []string{"get pods -n shop", "rollout status deploy/x -n shop", "auth can-i list pods", "logs web -n shop", "diff -f -"} {
		if out := run(t, tool, c, "kind: Pod\nmetadata: {name: x}\n", Call{Role: "readonly"}); strings.HasPrefix(out, "[Permission Denied]") {
			t.Errorf("readonly should read %q: %s", c, out)
		}
	}
}

func TestOperatorCannotDoHighRisk(t *testing.T) {
	tool, log, _ := fake(t, `echo ok`)
	for _, c := range []string{"delete pod x -n shop", "drain node1", "replace -f -", "taint nodes n a=b:NoSchedule", "cp a b", "debug x"} {
		out := run(t, tool, c, "kind: Pod\nmetadata: {name: x}\n", Call{Role: "operator"})
		if !strings.HasPrefix(out, "[Permission Denied]") || !strings.Contains(out, "admin API key") {
			t.Errorf("operator ran %q: %s", c, out)
		}
	}
	if got := calls(t, log); len(got) != 0 {
		t.Errorf("nothing may reach kubectl: %v", got)
	}
	if out := run(t, tool, "scale deploy x --replicas=2 -n shop", "", Call{Role: "operator"}); !strings.Contains(out, "ok") {
		t.Errorf("operator may do medium risk with approval: %s", out)
	}
}

func TestConnectionAndIdentityOverridesAreRefused(t *testing.T) {
	tool, log, _ := fake(t, `echo ok`)
	for _, f := range []string{"--as=system:masters", "--server=http://evil:8080", "--kubeconfig=/tmp/x", "--context=prod", "-s http://x", "--insecure-skip-tls-verify", "--token=abc"} {
		out := run(t, tool, "get pods -A "+f, "", Call{Role: "admin"})
		if !strings.HasPrefix(out, "[Protected]") {
			t.Errorf("%s: %s", f, out)
		}
	}
	if got := calls(t, log); len(got) != 0 {
		t.Errorf("nothing may reach kubectl: %v", got)
	}
	// the application's own sandbox identity, and only that exact token, gets through
	if out := run(t, tool, "get pods -n shop --as=system:serviceaccount:kube-sre:ro", "", Call{SandboxIdentity: "--as=system:serviceaccount:kube-sre:ro"}); !strings.Contains(out, "ok") {
		t.Errorf("sandbox identity: %s", out)
	}
	if out := run(t, tool, "get pods -n shop --as=system:serviceaccount:kube-sre:ro --as-group=system:masters", "", Call{SandboxIdentity: "--as=system:serviceaccount:kube-sre:ro"}); !strings.HasPrefix(out, "[Protected]") {
		t.Errorf("a second identity flag must fail: %s", out)
	}
	if out := run(t, tool, "get pods --as=someone-else", "", Call{SandboxIdentity: "--as=system:serviceaccount:kube-sre:ro"}); !strings.HasPrefix(out, "[Protected]") {
		t.Errorf("a token that only looks like it: %s", out)
	}
}

func TestProtectedNamespaceInEveryForm(t *testing.T) {
	tool, log, _ := fake(t, `echo ok`)
	cmds := []string{
		"get pods -n kube-system", "get pods -nkube-system", "get pods -n=kube-system", "get pods --namespace kube-system",
		"get pods --namespace=kube-system", "get pods -Rn kube-system", "exec -itn kube-system pod -- sh", "get pods -n KUBE-SYSTEM",
		"delete namespace kube-system", "delete ns/kube-system", "delete ns shop kube-system", "get ns/kube-system", "get ns kube-sre",
		"delete pod x -nkube-system", "logs coredns -n kube-system",
	}
	for _, c := range cmds {
		out := run(t, tool, c, "", Call{Role: "admin"})
		if !strings.HasPrefix(out, "[Protected]") {
			t.Errorf("%q: %s", c, out)
		}
	}
	if got := calls(t, log); len(got) != 0 {
		t.Errorf("nothing may reach kubectl: %v", got)
	}
	// the block applies to reads and to every role but superadmin
	for _, role := range []string{"readonly", "operator", "admin"} {
		if out := run(t, tool, "get pods -n kube-system", "", Call{Role: role}); !strings.HasPrefix(out, "[Protected]") {
			t.Errorf("%s: %s", role, out)
		}
	}
	if out := run(t, tool, "get pods -n kube-system", "", Call{Role: "superadmin"}); !strings.Contains(out, "ok") {
		t.Errorf("superadmin bypasses the namespace block: %s", out)
	}
}

func TestCredentialResourcesAreBlockedInEverySpellingForEveryRole(t *testing.T) {
	tool, log, _ := fake(t, `echo secret-data`)
	cmds := []string{
		"get secrets -n shop", "get secret db -n shop", "get sa -n shop", "get serviceaccounts", "get serviceaccount default -n shop",
		"-n shop get secrets", "get secrets.v1. -n shop", "get secret/db -n shop", "describe secret db -n shop", "delete secret db -n shop",
		"--warnings-as-errors get secrets -n shop", "get SECRETS -n shop", "get secrets,pods -n shop",
	}
	for _, c := range cmds {
		for _, role := range []string{"readonly", "admin", "superadmin"} {
			out := run(t, tool, c, "", Call{Role: role})
			if strings.Contains(out, "secret-data") || !(strings.HasPrefix(out, "[Protected]") || strings.HasPrefix(out, "[Permission Denied]")) {
				t.Errorf("%s %q: %s", role, c, out)
			}
		}
	}
	if got := calls(t, log); len(got) != 0 {
		t.Errorf("nothing may reach kubectl: %v", got)
	}
}

func TestBlocklistFoldsCaseAndNumber(t *testing.T) {
	tool, _, _ := fake(t, `echo ok`)
	tool2 := NewTool(Config{Bin: tool.cfg.Bin, BlockedResources: map[string]bool{"ConfigMap": true, "ingress": true}})
	for _, c := range []string{"get configmaps -n shop", "get cm-not -n shop"} {
		_ = c
	}
	for _, c := range []string{"get configmaps -n shop", "get configmap x -n shop", "get ingresses -n shop", "get ingress -n shop"} {
		if out := run(t, tool2, c, "", Call{}); !strings.HasPrefix(out, "[Protected]") {
			t.Errorf("%q: %s", c, out)
		}
	}
}

func TestManifestOnStdinIsInspected(t *testing.T) {
	tool, log, _ := fake(t, `echo applied`)
	secret := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: x\n  namespace: shop\n"
	if out := run(t, tool, "apply -f -", secret, Call{Role: "admin"}); !strings.HasPrefix(out, "[Protected]") {
		t.Errorf("a Secret in the manifest: %s", out)
	}
	if out := run(t, tool, "apply -f -", secret, Call{Role: "superadmin"}); !strings.HasPrefix(out, "[Protected]") {
		t.Errorf("superadmin never bypasses the resource block: %s", out)
	}
	pod := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: x\n  namespace: kube-system\n"
	if out := run(t, tool, "apply -f -", pod, Call{Role: "admin"}); !strings.HasPrefix(out, "[Protected]") {
		t.Errorf("a protected namespace in the manifest: %s", out)
	}
	list := "kind: List\nitems:\n- kind: Pod\n  metadata: {name: a, namespace: shop}\n- kind: Secret\n  metadata: {name: b}\n"
	if out := run(t, tool, "apply -f -", list, Call{Role: "admin"}); !strings.HasPrefix(out, "[Protected]") {
		t.Errorf("the items of a List are read too: %s", out)
	}
	ok := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n  namespace: shop\n"
	if out := run(t, tool, "apply -f -", ok, Call{Role: "admin"}); !strings.Contains(out, "applied") {
		t.Errorf("an ordinary manifest applies: %s", out)
	}
	if out := run(t, tool, "apply -f -", pod, Call{Role: "superadmin"}); !strings.Contains(out, "applied") {
		t.Errorf("superadmin may apply into a protected namespace: %s", out)
	}
	_ = log
}

func TestBadStdinAndBadCommands(t *testing.T) {
	tool, log, _ := fake(t, `echo ok`)
	cases := map[string]string{
		"get pods; rm -rf /":        "disallowed shell characters",
		"get pods `id`":             "disallowed shell characters",
		"get pods $(id)":            "disallowed shell characters",
		"get pods > /tmp/x":         "disallowed shell characters",
		"get pods | awk '{print}'":  "Only 'grep'",
		"get pods | grep x; ls":     "disallowed shell characters",
		`get pods -o "jsonpath={.a`: "unclosed quote",
	}
	for cmd, want := range cases {
		out := run(t, tool, cmd, "", Call{})
		if !strings.HasPrefix(out, "ERR:") || !strings.Contains(out, want) {
			t.Errorf("%q: got %q want %q", cmd, out, want)
		}
	}
	if out := run(t, tool, "apply -f -", "a: [unclosed", Call{}); !strings.Contains(out, "Invalid YAML") {
		t.Errorf("bad yaml: %s", out)
	}
	if out := run(t, tool, "apply -f -", "---\n", Call{}); !strings.Contains(out, "empty or null") {
		t.Errorf("null yaml: %s", out)
	}
	if got := calls(t, log); len(got) != 0 {
		t.Errorf("nothing may reach kubectl: %v", got)
	}
	// An unsupported grep flag is only found once the pipe runs, after kubectl.
	if out := run(t, tool, "get pods | grep -P x", "", Call{}); !strings.Contains(out, "does not implement") {
		t.Errorf("grep -P: %s", out)
	}
}

func TestRejectedVerbsAndSubcommands(t *testing.T) {
	tool, log, _ := fake(t, `echo ok`)
	if out := run(t, tool, "edit deploy x -n shop", "", Call{}); !strings.HasPrefix(out, "[Unsupported]") {
		t.Errorf("edit: %s", out)
	}
	if out := run(t, tool, "cluster-info dump --all-namespaces", "", Call{}); !strings.HasPrefix(out, "[Protected]") {
		t.Errorf("cluster-info dump: %s", out)
	}
	if out := run(t, tool, "cluster-info", "", Call{}); !strings.Contains(out, "ok") {
		t.Errorf("bare cluster-info is allowed: %s", out)
	}
	if got := calls(t, log); len(got) != 1 {
		t.Errorf("only cluster-info should run: %v", got)
	}
}

func TestExternalManifestSourceIsRefused(t *testing.T) {
	tool, log, _ := fake(t, `echo ok`)
	for _, c := range []string{
		"apply -f https://example.com/m.yaml", "apply -f /tmp/x.yaml", "apply -f=/tmp/x.yaml", "apply -f/tmp/x.yaml",
		"apply --filename /tmp/x.yaml", "apply --filename=/tmp/x.yaml", "apply -k ./dir", "create -f x.yaml",
	} {
		out := run(t, tool, c, "", Call{})
		if !strings.HasPrefix(out, "[Unsupported]") {
			t.Errorf("%q: %s", c, out)
		}
	}
	for _, c := range []string{"logs -f web -n shop", "logs web -f -n shop"} {
		if out := run(t, tool, c, "", Call{}); strings.HasPrefix(out, "[Unsupported]") {
			t.Errorf("-f is --follow on logs: %s", out)
		}
	}
	if out := run(t, tool, "apply -f -", "kind: ConfigMap\nmetadata: {name: x}\n", Call{}); !strings.Contains(out, "ok") {
		t.Errorf("-f - with stdin is the supported form: %s", out)
	}
	_ = log
}

func TestAllNamespacesWriteRefusedReadFiltered(t *testing.T) {
	tool, log, _ := fake(t, `printf 'NAMESPACE NAME\nshop web\nkube-system dns\n'`)
	for _, c := range []string{"delete pods --all-namespaces", "delete pods -A", "scale deploy --all -A --replicas=0"} {
		if out := run(t, tool, c, "", Call{Role: "admin"}); !strings.HasPrefix(out, "[Protected]") {
			t.Errorf("%q: %s", c, out)
		}
	}
	out := run(t, tool, "get pods -A", "", Call{Role: "admin"})
	if strings.Contains(out, "dns") || !strings.Contains(out, "web") || !strings.Contains(out, "withheld") {
		t.Errorf("%s", out)
	}
	if got := calls(t, log); len(got) != 1 {
		t.Errorf("only the read may run: %v", got)
	}
}

func TestApprovalGate(t *testing.T) {
	tool, log, _ := fake(t, `echo done`)

	p, err := tool.Plan("scale deploy web --replicas=3 -n shop", "", Call{Role: "admin"})
	if err != nil || p.Approval == nil || p.Approval.RiskLevel != "medium" || p.Approval.AlwaysConfirm {
		t.Fatalf("scale needs approval: %+v %v", p, err)
	}
	if p.Approval.Command != "kubectl scale deploy web --replicas=3 -n shop" || p.Approval.Type != "hitl" {
		t.Errorf("%+v", p.Approval)
	}
	p, _ = tool.Plan("delete pod x -n shop", "", Call{})
	if p.Approval == nil || p.Approval.RiskLevel != "high" {
		t.Errorf("delete is high: %+v", p.Approval)
	}
	p, _ = tool.Plan("get pods -n shop", "", Call{})
	if p.Approval != nil {
		t.Error("reads need no approval")
	}
	p, _ = tool.Plan("scale deploy web --replicas=3 -n shop", "", Call{HitlBypass: true})
	if p.Approval != nil {
		t.Error("auto approve skips ordinary approval")
	}
	p, _ = tool.Plan("delete namespace shop", "", Call{HitlBypass: true})
	if p.Approval == nil || !p.Approval.AlwaysConfirm || p.Approval.RiskLevel != "high" {
		t.Errorf("always confirm fires through auto approve: %+v", p.Approval)
	}
	p, _ = tool.Plan("delete pod x -n shop --dry-run=server", "", Call{})
	if p.Approval != nil || !p.HasDryRun {
		t.Error("a dry run needs no approval")
	}
	p, _ = tool.Plan("-n shop delete pod x", "", Call{})
	if p.Approval == nil {
		t.Error("flag order must not dodge the gate")
	}
	p, _ = tool.Plan("rollout restart deploy/x -n shop", "", Call{})
	if p.Approval == nil {
		t.Error("rollout restart is a write")
	}

	// a denial never reaches kubectl
	out, _ := tool.Run(context.Background(), "delete pod x -n shop", "", Call{}, func(Approval) bool { return false })
	if out != "Action cancelled by user." {
		t.Errorf("%q", out)
	}
	out, _ = tool.Run(context.Background(), "delete pod x -n shop", "", Call{}, nil)
	if out != "Action cancelled by user." {
		t.Errorf("no approver denies: %q", out)
	}
	for _, c := range calls(t, log) {
		if strings.HasPrefix(c, "delete") {
			t.Errorf("a denied command ran: %s", c)
		}
	}
	out, _ = tool.Run(context.Background(), "scale deploy web --replicas=3 -n shop", "", Call{}, func(a Approval) bool { return a.RiskLevel == "medium" })
	if !strings.Contains(out, "done") {
		t.Errorf("approved commands run: %q", out)
	}
}

func TestNonZeroExitIsStatedWithHintAndPartialOutput(t *testing.T) {
	tool, _, _ := fake(t, `echo "web 1/1"; echo 'Error from server (Forbidden): pods is forbidden in namespace "x"' >&2; exit 1`)
	out := run(t, tool, "get pods -n shop", "", Call{})
	if !strings.HasPrefix(out, "[kubectl exited 1] Error from server (Forbidden)") {
		t.Errorf("%s", out)
	}
	if !strings.Contains(out, "-> permission denied") {
		t.Errorf("the hint is appended: %s", out)
	}
	if !strings.Contains(out, "may be partial") || !strings.Contains(out, "web 1/1") {
		t.Errorf("partial output is kept and labelled: %s", out)
	}
}

func TestGrepDoesNotEatTheError(t *testing.T) {
	tool, _, _ := fake(t, `echo 'Error from server (Forbidden): nope' >&2; exit 1`)
	out := run(t, tool, "get pods | grep Running", "", Call{})
	if strings.Contains(out, "(no matching lines)") || !strings.Contains(out, "Forbidden") {
		t.Errorf("a failure must not read as an empty listing: %s", out)
	}
}

func TestUnreachableClusterSaysRetryingIsPointless(t *testing.T) {
	tool, _, _ := fake(t, `echo 'The connection to the server 127.0.0.1:6443 was refused - did you specify the right host or port?' >&2; exit 1`)
	out := run(t, tool, "get pods", "", Call{})
	if !strings.Contains(out, "[unavailable]") || !strings.Contains(out, "not reachable") {
		t.Errorf("%s", out)
	}
	tool.cfg.ErrorHints = false
	if out := run(t, tool, "get pods", "", Call{}); strings.Contains(out, "->") {
		t.Errorf("hints are off: %s", out)
	}
}

func TestExitOneIsAnAnswerForDiffAndCanI(t *testing.T) {
	tool, _, _ := fake(t, `echo no; exit 1`)
	for _, c := range []string{"auth can-i delete pods", "diff -f -"} {
		out := run(t, tool, c, "kind: Pod\nmetadata: {name: x}\n", Call{})
		if strings.Contains(out, "[kubectl exited") || !strings.Contains(out, "no") {
			t.Errorf("%q: %s", c, out)
		}
	}
	if out := run(t, tool, "get pods", "", Call{}); !strings.Contains(out, "[kubectl exited 1]") {
		t.Errorf("a plain get is still an error: %s", out)
	}
}

func TestOutputIsCappedWithAMarker(t *testing.T) {
	tool, _, _ := fake(t, `i=0; while [ $i -lt 1000 ]; do echo "line number $i of a long listing"; i=$((i+1)); done`)
	out := run(t, tool, "get pods", "", Call{})
	if !strings.Contains(out, "[truncated:") || !strings.Contains(out, "chars omitted") {
		t.Errorf("%s", out[len(out)-200:])
	}
	if n := len([]rune(out)); n > MaxOutput+400 {
		t.Errorf("output is %d chars", n)
	}
}

func TestMissingKubectlIsMarkedUnavailable(t *testing.T) {
	tool := NewTool(Config{Bin: "/nonexistent/kubectl"})
	out := run(t, tool, "get pods", "", Call{})
	if !strings.HasPrefix(out, "[Error] kubectl is not installed") || !strings.Contains(out, "[unavailable]") {
		t.Errorf("%s", out)
	}
}

func TestTimeoutIsAnError(t *testing.T) {
	tool, _, _ := fake(t, `exec sleep 5`)
	tool.cfg.Timeout = 300 * time.Millisecond
	out := run(t, tool, "get pods", "", Call{})
	if !strings.Contains(out, "timed out") {
		t.Errorf("%s", out)
	}
}

func TestNamespaceListingIsFilteredThroughTheTool(t *testing.T) {
	tool, _, _ := fake(t, `printf 'NAME STATUS\ndefault Active\nkube-system Active\nshop Active\n'`)
	out := run(t, tool, "get namespaces", "", Call{Role: "admin"})
	if strings.Contains(out, "kube-system") || !strings.Contains(out, "shop") || !strings.Contains(out, "1 namespace(s) withheld") {
		t.Errorf("%s", out)
	}
	if out := run(t, tool, "get ns -o jsonpath='{.items[*].metadata.name}'", "", Call{Role: "admin"}); !strings.HasPrefix(out, "[Protected]") {
		t.Errorf("jsonpath is refused: %s", out)
	}
}

func TestRollbackPointIsRecordedBeforeAMutation(t *testing.T) {
	tool, log, r := fake(t, `case "$1" in get) echo "kind: Pod"; echo "metadata:"; echo "  name: x";; *) echo changed;; esac`)
	out := run(t, tool, "delete pod x -n shop", "", Call{SessionID: "sess-1"})
	if !strings.Contains(out, "changed") {
		t.Fatalf("%s", out)
	}
	cs := calls(t, log)
	if len(cs) != 2 || !strings.HasPrefix(cs[0], "get pod x -n shop -o yaml") || !strings.HasPrefix(cs[1], "delete pod x") {
		t.Fatalf("the capture must run first: %v", cs)
	}
	if len(r.events) != 1 {
		t.Fatalf("events: %v", r.events)
	}
	ev := r.events[0]
	if ev["_episode"] != "sess-1" || ev["_kind"] != "rollback_point" || ev["restorable"] != true ||
		ev["targets_intended"] != 1 || ev["targets_captured"] != 1 || !strings.HasPrefix(ev["rollback_id"].(string), "rb-") {
		t.Errorf("%v", ev)
	}
	if states := ev["pre_state"].([]string); len(states) != 1 || !strings.Contains(states[0], "name: x") {
		t.Errorf("%v", states)
	}
}

func TestNoRollbackPointForReadsOrDryRuns(t *testing.T) {
	tool, _, r := fake(t, `echo ok`)
	run(t, tool, "get pods -n shop", "", Call{})
	run(t, tool, "delete pod x -n shop --dry-run=client", "", Call{})
	if len(r.events) != 0 {
		t.Errorf("%v", r.events)
	}
}

func TestRollbackPointIsNotRestorableWhenRedactionChangedIt(t *testing.T) {
	tool, _, r := fake(t, `case "$1" in get) echo "kind: ConfigMap"; echo "data:"; echo "  password: hunter2";; *) echo changed;; esac`)
	run(t, tool, "patch cm x -n shop -p {}", "", Call{})
	if len(r.events) != 1 || r.events[0]["restorable"] != false {
		t.Fatalf("a redacted capture is evidence, not a restore point: %v", r.events)
	}
	notes := r.events[0]["capture_notes"].([]string)
	if len(notes) == 0 || !strings.Contains(notes[0], "redacted") {
		t.Errorf("the record must say what happened: %v", notes)
	}
	if strings.Contains(r.events[0]["pre_state"].([]string)[0], "hunter2") {
		t.Error("the credential must never be stored")
	}
}

func TestRollbackPointSaysWhenItCoversFewerObjectsThanTheCommand(t *testing.T) {
	tool, _, r := fake(t, `case "$*" in *missing*) echo 'Error from server (NotFound)' >&2; exit 1;; get*) echo "kind: Pod";; *) echo changed;; esac`)
	stdin := "kind: Pod\nmetadata: {name: a, namespace: shop}\n---\nkind: Pod\nmetadata: {name: missing, namespace: shop}\n"
	run(t, tool, "apply -f -", stdin, Call{})
	ev := r.events[0]
	if ev["restorable"] != false || ev["targets_intended"] != 2 || ev["targets_captured"] != 1 {
		t.Errorf("a partial capture must not read as a full restore point: %v", ev)
	}
}

func TestRollbackTargets(t *testing.T) {
	join := func(ts [][]string) string {
		var s []string
		for _, t := range ts {
			s = append(s, strings.Join(t, " "))
		}
		return strings.Join(s, " ; ")
	}
	cases := []struct{ cmd, stdin, want string }{
		{"kubectl delete pod x -n shop", "", "get pod x -n shop -o yaml"},
		{"kubectl label pod api-1 tier=web -n shop", "", "get pod api-1 -n shop -o yaml"},
		{"kubectl rollout restart deployment/api -n shop", "", "get deployment/api -n shop -o yaml"},
		{"kubectl scale deploy web --replicas=3 -n shop", "", "get deploy web -n shop -o yaml"},
		{"kubectl delete pods --all -n shop", "", "get pods -n shop -o yaml"},
		{"kubectl apply -f -", "kind: Deployment\nmetadata: {name: a, namespace: shop}\n---\nkind: Service\nmetadata: {name: b}\n", "get deployment a -o yaml -n shop ; get service b -o yaml"},
	}
	for _, c := range cases {
		a := toks(c.cmd)
		got, perr := rollbackTargets(ExtractVerb(a), a, c.stdin)
		if join(got) != c.want || perr != "" {
			t.Errorf("%q: got %q (%s) want %q", c.cmd, join(got), perr, c.want)
		}
	}
	a := toks("kubectl apply -f -")
	if _, perr := rollbackTargets("apply", a, "kind: Pod\nmetadata: {name: a}\n---\nx: [broken"); perr == "" {
		t.Error("a half parsed manifest must be reported")
	}
}

func TestRollbackCappedAtFiveObjects(t *testing.T) {
	tool, _, r := fake(t, `case "$1" in get) echo "kind: Pod";; *) echo changed;; esac`)
	var docs []string
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		docs = append(docs, "kind: Pod\nmetadata: {name: "+n+", namespace: shop}\n")
	}
	run(t, tool, "apply -f -", strings.Join(docs, "---\n"), Call{})
	ev := r.events[0]
	if ev["targets_intended"] != 7 || ev["targets_captured"] != 5 || ev["restorable"] != false {
		t.Errorf("%v", ev)
	}
	notes := strings.Join(ev["capture_notes"].([]string), ";")
	if !strings.Contains(notes, "2 further object(s) were never attempted") {
		t.Errorf("the cap must be named: %s", notes)
	}
}
