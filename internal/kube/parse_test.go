package kube

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitFollowsPosixQuoting(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`get pods -n shop`, []string{"get", "pods", "-n", "shop"}},
		{`get pods -l 'a=b c'`, []string{"get", "pods", "-l", "a=b c"}},
		{`get pods -o "jsonpath={.items[*].metadata.name}"`, []string{"get", "pods", "-o", "jsonpath={.items[*].metadata.name}"}},
		// single quotes are literal, so the jsonpath separator keeps its backslash
		{`-o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'`, []string{"-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`}},
		// inside double quotes a backslash only escapes a quote or a backslash
		{`"a\"b"`, []string{`a"b`}},
		{`"a\\b"`, []string{`a\b`}},
		{`"a\nb"`, []string{`a\nb`}},
		{`a\ b c`, []string{"a b", "c"}},
		{`'' x`, []string{"", "x"}},
		{`a"b c"d`, []string{"ab cd"}},
		{"  spaced   out  ", []string{"spaced", "out"}},
		{``, nil},
	}
	for _, c := range cases {
		got, err := Split(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %q want %q", c.in, got, c.want)
		}
	}
	if _, err := Split(`get "pods`); err == nil || !strings.Contains(err.Error(), "closing quotation") {
		t.Errorf("unterminated quote: %v", err)
	}
	if _, err := Split(`get pods\`); err == nil {
		t.Error("a trailing backslash is an error")
	}
}

func TestSplitPipesRespectsQuotes(t *testing.T) {
	got := splitPipes(`kubectl get pods | grep "a|b" | grep -v 'x|y'`)
	if len(got) != 3 || strings.TrimSpace(got[1]) != `grep "a|b"` || strings.TrimSpace(got[2]) != `grep -v 'x|y'` {
		t.Errorf("%q", got)
	}
}

func TestValueAndBooleanGlobalFlagsAreDisjoint(t *testing.T) {
	for f := range booleanGlobalFlags {
		if valueFlags[f] {
			t.Errorf("%s is boolean and must not consume the next token, which is the verb", f)
		}
	}
}

func TestVerbIsFoundPastGlobalFlags(t *testing.T) {
	cases := map[string]string{
		"kubectl get pods":                            "get",
		"kubectl -n prod delete deploy api":           "delete",
		"kubectl --namespace=prod delete deploy api":  "delete",
		"kubectl --warnings-as-errors get secrets":    "get",
		"kubectl --insecure-skip-tls-verify get pods": "get",
		"kubectl -o json get pods":                    "get",
		"kubectl":                                     "",
		"kubectl -n":                                  "",
	}
	for cmd, want := range cases {
		toks, _ := Split(cmd)
		if got := ExtractVerb(toks); got != want {
			t.Errorf("%q: got %q want %q", cmd, got, want)
		}
	}
}

func TestFlagValueReadsEveryForm(t *testing.T) {
	cases := []struct {
		cmd, want string
	}{
		{"get pods -n kube-system", "kube-system"},
		{"get pods --namespace kube-system", "kube-system"},
		{"get pods --namespace=kube-system", "kube-system"},
		{"get pods -n=kube-system", "kube-system"},
		{"get pods -nkube-system", "kube-system"},
		{"get pods -Rn kube-system", "kube-system"},
		{"get pods -Rnkube-system", "kube-system"},
		{"get pods -Rn=kube-system", "kube-system"},
		{"exec -itn kube-system pod -- sh", "kube-system"},
		{"get pods -ojson", ""}, // the n inside -ojson is not the namespace flag
		{"get pods -o json -A", ""},
		{"get pods --no-headers", ""},
		{"get pods -n", ""},
		{"get pods --namespace=", ""},
		{"get pods -", ""},
	}
	for _, c := range cases {
		toks, _ := Split("kubectl " + c.cmd)
		verb := ExtractVerb(toks)
		if got := extractNamespace(toks, verb); got != c.want {
			t.Errorf("%q: got %q want %q", c.cmd, got, c.want)
		}
	}
	// -f is --follow on logs, so the group continues past it
	toks, _ := Split("kubectl logs -fn kube-system pod")
	if got := extractNamespace(toks, "logs"); got != "kube-system" {
		t.Errorf("logs -fn: %q", got)
	}
	// but elsewhere -f takes a value, so -fns.yaml is not a namespace
	toks, _ = Split("kubectl apply -fns.yaml")
	if got := extractNamespace(toks, "apply"); got != "" {
		t.Errorf("apply -fns.yaml invented a namespace: %q", got)
	}
	toks, _ = Split("kubectl get pods -o json")
	if got := FlagValue(toks, "-o", "--output", "get"); got != "json" {
		t.Errorf("-o: %q", got)
	}
}

func TestAllNamespacesForms(t *testing.T) {
	yes := []string{"-A", "--all-namespaces", "--all-namespaces=true", "--all-namespaces=1"}
	no := []string{"--all-namespaces=false", "--all-namespaces=0", "--all-namespaces=No", "-n x"}
	for _, f := range yes {
		toks, _ := Split("kubectl get pods " + f)
		if !isAllNamespaces(toks) {
			t.Errorf("%s should be cluster wide", f)
		}
	}
	for _, f := range no {
		toks, _ := Split("kubectl get pods " + f)
		if isAllNamespaces(toks) {
			t.Errorf("%s is not cluster wide", f)
		}
	}
}

func TestResourceTypeAndTargets(t *testing.T) {
	toks := func(s string) []string { x, _ := Split(s); return x }
	if got := extractResourceType("get", toks("kubectl -n prod get secrets")); got != "secrets" {
		t.Errorf("flag before the operand: %q", got)
	}
	if got := extractResourceType("get", toks("kubectl get deployment/api")); got != "deployment" {
		t.Errorf("slash form: %q", got)
	}
	if got := extractResourceType("logs", toks("kubectl logs secrets")); got != "" {
		t.Errorf("logs has no resource operand: %q", got)
	}
	cases := map[string][]string{
		"kubectl delete ns kube-system":      {"kube-system"},
		"kubectl delete ns/kube-system":      {"kube-system"},
		"kubectl delete ns shop kube-system": {"shop", "kube-system"},
		"kubectl get ns/Kube-System":         {"kube-system"},
		"kubectl delete --force ns a b":      {"a", "b"},
		"kubectl get namespaces":             nil,
		"kubectl get pods":                   nil,
	}
	for cmd, want := range cases {
		a := toks(cmd)
		got := targetedNamespaces(ExtractVerb(a), a)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %q want %q", cmd, got, want)
		}
	}
}

func TestResourceSpellings(t *testing.T) {
	has := func(res, want string) {
		t.Helper()
		if !ResourceSpellings(res)[want] {
			t.Errorf("%q should include %q: %v", res, want, ResourceSpellings(res))
		}
	}
	has("sa", "serviceaccounts")
	has("Secret", "secrets")
	has("secrets.v1.", "secrets")
	has("secrets.v1.", "secret")
	has("configmap", "configmaps")
	has("configmaps", "configmap")
	has("ingresses", "ingress")
	has("ingress", "ingresses")
	has("networkpolicies", "networkpolicy")
	has("networkpolicy", "networkpolicies")
	if len(ResourceSpellings("")) != 0 {
		t.Error("empty resource")
	}
}
