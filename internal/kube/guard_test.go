package kube

import "testing"

func toks(s string) []string {
	x, _ := Split(s)
	return x
}

func TestIsWriteFailsClosed(t *testing.T) {
	reads := []string{
		"kubectl get pods", "kubectl describe pod x", "kubectl logs x", "kubectl top nodes", "kubectl diff -f -",
		"kubectl rollout status deploy/x", "kubectl rollout history deploy/x", "kubectl -n a rollout status deploy/x",
		"kubectl auth can-i list pods", "kubectl auth whoami", "kubectl config view", "kubectl cluster-info",
		"kubectl api-resources", "kubectl version", "kubectl explain pod", "kubectl wait --for=condition=ready pod/x",
	}
	writes := []string{
		"kubectl delete pod x", "kubectl apply -f -", "kubectl rollout restart deploy/x", "kubectl rollout undo deploy/x",
		"kubectl rollout pause deploy/x", "kubectl auth reconcile -f -", "kubectl label pod x a=b", "kubectl annotate pod x a=b",
		"kubectl cp a b", "kubectl debug node/x", "kubectl expose deploy x", "kubectl autoscale deploy x",
		"kubectl port-forward pod/x 80", "kubectl attach x", "kubectl config use-context x", "kubectl certificate approve x",
		"kubectl somefutureverb", "kubectl rollout", "kubectl auth",
	}
	for _, c := range reads {
		a := toks(c)
		if IsWrite(ExtractVerb(a), a) {
			t.Errorf("%q should be a read", c)
		}
	}
	for _, c := range writes {
		a := toks(c)
		if !IsWrite(ExtractVerb(a), a) {
			t.Errorf("%q should be a write", c)
		}
	}
	if IsWrite("", nil) {
		t.Error("no verb is not a write")
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]Risk{
		"kubectl delete pod x": RiskHigh, "kubectl drain n": RiskHigh, "kubectl cp a b": RiskHigh, "kubectl debug x": RiskHigh,
		"kubectl scale deploy x --replicas=2": RiskMedium, "kubectl apply -f -": RiskMedium, "kubectl exec x -- ls": RiskMedium,
		"kubectl rollout restart deploy/x": RiskMedium, "kubectl newverb": RiskMedium,
		"kubectl get pods": RiskNone, "kubectl rollout status deploy/x": RiskNone,
	}
	for cmd, want := range cases {
		a := toks(cmd)
		if got := Classify(ExtractVerb(a), a); got != want {
			t.Errorf("%q: got %s want %s", cmd, got, want)
		}
	}
}

func TestAlwaysConfirmSurvivesFlagPlacement(t *testing.T) {
	yes := []string{
		"kubectl delete namespace shop", "kubectl delete ns shop", "kubectl delete --force namespace shop",
		"kubectl -n prod delete namespace shop", "kubectl delete pv x", "kubectl delete --ignore-not-found pv x",
		"kubectl delete crd x", "kubectl delete ns/shop", "kubectl drain node1",
		"kubectl set image deploy/api c=x", "kubectl -n prod set image deploy/api c=x", "kubectl set --record image deploy/api c=x",
		"kubectl set resources deploy/api --limits=cpu=1",
	}
	no := []string{
		"kubectl delete pod x", "kubectl delete deployment x", "kubectl set env deploy/x A=b", "kubectl scale deploy x --replicas=1",
		"kubectl get namespaces",
	}
	for _, c := range yes {
		a := toks(c)
		if !AlwaysConfirm(ExtractVerb(a), a) {
			t.Errorf("%q must always confirm", c)
		}
	}
	for _, c := range no {
		a := toks(c)
		if AlwaysConfirm(ExtractVerb(a), a) {
			t.Errorf("%q should not force confirmation", c)
		}
	}
}

func TestDestructiveVerbsInIsPositionIndependent(t *testing.T) {
	got := DestructiveVerbsIn(toks("kubectl get pods delete -l app=delete --dry-run=delete"))
	if len(got) != 1 || !got["delete"] {
		t.Errorf("only the bare token counts: %v", got)
	}
	if len(DestructiveVerbsIn(toks("kubectl get pods"))) != 0 {
		t.Error("reads have none")
	}
}

func TestConnectionFlags(t *testing.T) {
	refuse := []string{
		"--as=system:masters", "--as system:masters", "--as-group=x", "--as-uid=1", "--as-future=1", "--server=http://x",
		"-s http://x", "--kubeconfig=/tmp/x", "--context=other", "--cluster=x", "--user=x", "--token=x", "--username=x",
		"--password=x", "--insecure-skip-tls-verify", "--certificate-authority=x", "--client-key=x", "--tls-server-name=x",
	}
	for _, f := range refuse {
		if ConnectionFlagIn(toks("kubectl get pods "+f)) == "" {
			t.Errorf("%s must be refused", f)
		}
	}
	if ConnectionFlagIn(toks("kubectl get pods -n shop -o json -l a=b")) != "" {
		t.Error("ordinary flags are fine")
	}
	if !identityIsAuthorised(toks("kubectl get pods --as=sa"), "--as=sa") {
		t.Error("the exact token is authorised")
	}
	if identityIsAuthorised(toks("kubectl get pods --as=sa --as-group=system:masters"), "--as=sa") {
		t.Error("a second identity flag is the escape and must fail")
	}
	if identityIsAuthorised(toks("kubectl get pods --as=other"), "--as=sa") {
		t.Error("a token that only looks similar is not enough")
	}
}

func TestExitIsAnAnswer(t *testing.T) {
	d := toks("kubectl diff -f -")
	if !ExitIsAnAnswer("diff", d, 1) || ExitIsAnAnswer("diff", d, 2) {
		t.Error("diff exits 1 for differences and higher for failure")
	}
	c := toks("kubectl auth can-i delete pods")
	if !ExitIsAnAnswer("auth", c, 1) || ExitIsAnAnswer("auth", c, 2) {
		t.Error("can-i exits 1 for no")
	}
	if ExitIsAnAnswer("get", toks("kubectl get pods"), 1) || ExitIsAnAnswer("auth", toks("kubectl auth whoami"), 1) {
		t.Error("nothing else is an answer")
	}
}
