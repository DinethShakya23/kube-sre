package cluster

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fake(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kubectl")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func resolver(t *testing.T, configured, script string) *Resolver {
	r := NewResolver(configured, "")
	r.Bin = fake(t, script)
	return r
}

func TestConfiguredIdAlwaysWins(t *testing.T) {
	r := resolver(t, "  prod-eu  ", `echo should-not-be-asked; exit 1`)
	if got := r.Resolve(context.Background()); got != "prod-eu" {
		t.Errorf("%q", got)
	}
}

func TestContextAndServerAreCombinedAndHashed(t *testing.T) {
	r := resolver(t, "", `case "$*" in
*current-context*) echo kind-dev;;
*jsonpath=\{.clusters*) echo https://10.0.0.1:6443;;
esac`)
	got := r.Resolve(context.Background())
	if !strings.HasPrefix(got, "kind-dev:") || len(got) != len("kind-dev:")+8 || strings.Contains(got, "10.0.0.1") {
		t.Errorf("%q", got)
	}
}

func TestContextAloneOrServerAlone(t *testing.T) {
	r := resolver(t, "", `case "$*" in *current-context*) echo only-ctx;; esac`)
	if got := r.Resolve(context.Background()); got != "only-ctx" {
		t.Errorf("%q", got)
	}
	r = resolver(t, "", `case "$*" in *jsonpath=\{.clusters*) echo https://api;; esac`)
	if got := r.Resolve(context.Background()); !strings.HasPrefix(got, "server:") || len(got) != len("server:")+12 {
		t.Errorf("%q", got)
	}
}

func TestInClusterUsesTheKubeSystemUID(t *testing.T) {
	r := resolver(t, "", `case "$*" in
*"get namespace kube-system"*) echo 0123456789abcdef-uid;;
*) exit 1;;
esac`)
	if got := r.Resolve(context.Background()); got != "uid:0123456789ab" {
		t.Errorf("%q", got)
	}
}

func TestSentinelWhenNothingIdentifiesTheCluster(t *testing.T) {
	r := resolver(t, "", `exit 1`)
	got := r.Resolve(context.Background())
	if got != Unresolved || IsResolved(got) {
		t.Errorf("%q", got)
	}
	if !IsResolved("prod") || IsResolved("") {
		t.Error("IsResolved")
	}
	missing := NewResolver("", "")
	missing.Bin = "/nonexistent/kubectl"
	if got := missing.Resolve(context.Background()); got != Unresolved {
		t.Errorf("a missing binary is the sentinel: %q", got)
	}
}

func TestResultIsCachedForTheProcess(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "n")
	r := resolver(t, "", `echo x >> `+counter+`; echo ctx`)
	for i := 0; i < 3; i++ {
		r.Resolve(context.Background())
	}
	b, _ := os.ReadFile(counter)
	if n := strings.Count(string(b), "x"); n != 2 {
		t.Errorf("kubectl asked %d times, want 2 (context and server, once)", n)
	}
}
