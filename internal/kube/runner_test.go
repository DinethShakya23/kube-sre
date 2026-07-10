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

func TestArgsAndPipes(t *testing.T) {
	args, pipes, err := Args("kubectl get pods -A | grep -i crash | grep -v ok")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(args, " ") != "get pods -A" || len(pipes) != 2 {
		t.Errorf("args=%v pipes=%v", args, pipes)
	}
	if _, _, err := Args("get pods; rm -rf /"); err == nil {
		t.Error("metachar must be rejected")
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
