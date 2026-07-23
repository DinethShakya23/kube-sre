package kube

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeKubectl(t *testing.T, body string) *Runner {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kubectl")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Runner{Bin: p, Timeout: 5 * time.Second}
}

func TestSplit(t *testing.T) {
	got, err := Split(`get pods -o "jsonpath={.items[*].metadata.name}" -l 'a=b c'`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"get", "pods", "-o", "jsonpath={.items[*].metadata.name}", "-l", "a=b c"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %v", got)
	}
	if _, err := Split(`get "pods`); err == nil {
		t.Error("unterminated quote should fail")
	}
}

func TestParse(t *testing.T) {
	r := &Runner{Protect: &Protect{Namespaces: set("kube-system"), Resources: set("secrets")}}
	c, err := r.Parse("kubectl get pods -A | grep -i crash | grep -v ok")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Args, " ") != "get pods -A" || len(c.Pipes) != 2 || c.Risk != RiskNone {
		t.Errorf("cmd=%+v", c)
	}
	c, _ = r.Parse("delete namespace demo")
	if c.Risk != RiskHigh || !c.Confirm {
		t.Errorf("delete namespace should be high and confirm: %+v", c)
	}
	for _, bad := range []string{"get pods; rm -rf /", "get pods -n kube-system", "get secrets"} {
		if _, err := r.Parse(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestExecFiltersProtectedRows(t *testing.T) {
	r := fakeKubectl(t, `printf 'NAMESPACE NAME
kube-system dns
prod web
'`)
	r.Protect = &Protect{Namespaces: set("kube-system")}
	out, err := r.Exec(context.Background(), []string{"get", "pods", "-A"}, nil)
	if err != nil || strings.Contains(out, "dns") || !strings.Contains(out, "web") {
		t.Errorf("out=%q err=%v", out, err)
	}
}

func TestExecGrepAndCap(t *testing.T) {
	r := fakeKubectl(t, `printf 'alpha\nBeta\ngamma\n'`)
	out, err := r.Exec(context.Background(), []string{"get", "pods"}, []string{"grep -i beta"})
	if err != nil || out != "Beta\n" {
		t.Errorf("out=%q err=%v", out, err)
	}
	out, _ = r.Exec(context.Background(), []string{"get"}, []string{"grep -c a"})
	if strings.TrimSpace(out) != "3" {
		t.Errorf("count=%q", out)
	}
	if _, err := r.Exec(context.Background(), []string{"get"}, []string{"awk x"}); err == nil {
		t.Error("non-grep pipe should fail")
	}

	big := fakeKubectl(t, `head -c 20000 /dev/zero | tr '\0' 'x'`)
	out, _ = big.Exec(context.Background(), []string{"get"}, nil)
	if len(out) > MaxOutput+40 || !strings.HasSuffix(out, "[output truncated]") {
		t.Errorf("not capped: %d", len(out))
	}
}

func TestExecErrorSurfacesStderr(t *testing.T) {
	r := fakeKubectl(t, `echo boom >&2; exit 1`)
	_, err := r.Exec(context.Background(), []string{"get"}, nil)
	if err == nil || err.Error() != "boom" {
		t.Errorf("err=%v", err)
	}
}

func TestExecAddsHintOnlyWhenEnabled(t *testing.T) {
	r := fakeKubectl(t, `echo 'Error from server (Forbidden): nope' >&2; exit 1`)
	_, err := r.Exec(context.Background(), []string{"get", "pods"}, nil)
	if err == nil || strings.Contains(err.Error(), "->") {
		t.Fatalf("no hint expected, got %v", err)
	}
	r.Hints = true
	_, err = r.Exec(context.Background(), []string{"get", "pods"}, nil)
	if err == nil || !strings.Contains(err.Error(), "Forbidden") || !strings.Contains(err.Error(), "->") {
		t.Fatalf("want original plus hint, got %v", err)
	}
}

func TestExecExitOneIsAnswerForDiffAndCanI(t *testing.T) {
	r := fakeKubectl(t, `echo 'no'; exit 1`)
	for _, args := range [][]string{{"diff", "-f", "x.yaml"}, {"auth", "can-i", "delete", "pods"}} {
		out, err := r.Exec(context.Background(), args, nil)
		if err != nil || !strings.Contains(out, "no") {
			t.Errorf("%v: out=%q err=%v", args, out, err)
		}
	}
	if _, err := r.Exec(context.Background(), []string{"get", "pods"}, nil); err == nil {
		t.Error("plain get with exit 1 stays an error")
	}
}
