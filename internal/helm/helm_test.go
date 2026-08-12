package helm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DinethShakya23/kube-sre/internal/nsguard"
)

func fake(t *testing.T, body string) *Tool {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "helm")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := New(nsguard.Blocklist{"kube-system": true, "monitoring": true},
		map[string]bool{"secret": true, "secrets": true, "serviceaccount": true, "serviceaccounts": true})
	tool.Bin = bin
	return tool
}

func run(tool *Tool, cmd string) string { return tool.Run(context.Background(), cmd) }

func TestOnlyReadsRun(t *testing.T) {
	tool := fake(t, `echo ok`)
	for _, c := range []string{"install x y", "upgrade x y", "rollback x 1", "uninstall x", "delete x", "repo add a b", "plugin install x",
		"template x y", "-n prod install x y", "pull x", "create x", "lint x"} {
		if out := run(tool, c); !strings.HasPrefix(out, "[Not Allowed]") || !strings.Contains(out, "write operation") {
			t.Errorf("%q: %s", c, out)
		}
	}
	if out := run(tool, "frobnicate x"); !strings.Contains(out, "not a supported subcommand") {
		t.Errorf("unknown verb: %s", out)
	}
	for _, c := range []string{"list -A", "status x -n prod", "get values x -n prod", "history x", "env", "version", "show chart x", "search repo x", "-n prod list"} {
		if out := run(tool, c); out != "ok" {
			t.Errorf("%q should run: %s", c, out)
		}
	}
}

func TestShellAndConnectionOverridesRefused(t *testing.T) {
	tool := fake(t, `echo ok`)
	for _, c := range []string{"list; rm x", "list | cat", "list `id`", "list $(id)", "list > x", `list \n`} {
		if out := run(tool, c); !strings.HasPrefix(out, "[Error]") || !strings.Contains(out, "shell characters") {
			t.Errorf("%q: %s", c, out)
		}
	}
	for _, c := range []string{"list -A --kube-as-user system:masters", "list --kube-apiserver https://evil", "list --kubeconfig=/tmp/x",
		"list --kube-context=other", "list --kube-token=abc", "list --registry-config=/x", "list --repository-config /x"} {
		if out := run(tool, c); !strings.HasPrefix(out, "[Protected]") {
			t.Errorf("%q: %s", c, out)
		}
	}
	if out := run(tool, `list "unclosed`); !strings.HasPrefix(out, "[Error] Could not parse") {
		t.Errorf("%s", out)
	}
}

func TestProtectedNamespaces(t *testing.T) {
	tool := fake(t, `echo ok`)
	for _, c := range []string{"list -n kube-system", "list --namespace=monitoring", "get values x -n Monitoring", "list -nkube-system", "status x -Rn kube-system"} {
		if out := run(tool, c); !strings.HasPrefix(out, "[Protected]") {
			t.Errorf("%q: %s", c, out)
		}
	}
}

const manifest = `---
# Source: chart/templates/deploy.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
---
apiVersion: v1
kind: Secret
metadata:
  name: db
data:
  password: aHVudGVyMg==
---
apiVersion: v1
kind: "ServiceAccount"
metadata:
  name: sa
---
apiVersion: v1
kind: 'secret'  # managed by helm
metadata:
  name: other
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg
`

func TestSecretsAreStrippedFromEveryGetSpelling(t *testing.T) {
	tool := fake(t, "cat <<'EOF'\n"+manifest+"EOF")
	for _, c := range []string{"get manifest shop -n prod", "-n prod get manifest shop", "get all shop", "get hooks shop"} {
		out := run(tool, c)
		if strings.Contains(out, "aHVudGVyMg") || strings.Contains(out, "kind: Secret") || strings.Contains(out, `kind: "ServiceAccount"`) ||
			strings.Contains(out, "name: other") || strings.Contains(out, "name: db") {
			t.Errorf("%q leaked a protected document:\n%s", c, out)
		}
		if !strings.Contains(out, "kind: Deployment") || !strings.Contains(out, "kind: ConfigMap") {
			t.Errorf("%q lost ordinary documents:\n%s", c, out)
		}
		if !strings.Contains(out, "3 object(s) of a protected kind were removed") {
			t.Errorf("%q must say what was removed:\n%s", c, out)
		}
	}
}

func TestStripIsANoopWithoutProtectedKinds(t *testing.T) {
	tool := fake(t, "printf 'kind: Deployment\\nmetadata:\\n  name: x\\n'")
	out := run(tool, "get manifest x")
	if strings.Contains(out, "removed") || !strings.Contains(out, "kind: Deployment") {
		t.Errorf("%s", out)
	}
}

const releaseTable = "NAME\tNAMESPACE\tREVISION\nweb\tshop\t1\nprom\tmonitoring\t3\ndns\tkube-system\t2\n"

func TestReleaseTableIsFilteredAndSaysSo(t *testing.T) {
	tool := fake(t, "printf '"+strings.ReplaceAll(releaseTable, "\n", "\\n")+"'")
	out := run(tool, "list -A")
	if strings.Contains(out, "prom") || strings.Contains(out, "dns") || !strings.Contains(out, "web") || !strings.Contains(out, "2 release(s) withheld") {
		t.Errorf("%s", out)
	}
}

func TestReleaseJSONAndYAML(t *testing.T) {
	js := `[{"name":"web","namespace":"shop"},{"name":"prom","namespace":"Monitoring"}]`
	tool := fake(t, "echo '"+js+"'")
	out := run(tool, "list -A -o json")
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got) != 1 || got[0]["name"] != "web" {
		t.Errorf("%v %s", err, out)
	}
	tool = fake(t, "printf -- '- name: web\\n  namespace: shop\\n- name: prom\\n  namespace: monitoring\\n'")
	out = run(tool, "list -A -o yaml")
	if strings.Contains(out, "prom") || !strings.Contains(out, "web") {
		t.Errorf("%s", out)
	}
	tool = fake(t, "echo '[not json'")
	if out := run(tool, "list -A -o json"); !strings.HasPrefix(out, "[Protected]") {
		t.Errorf("fail closed: %s", out)
	}
}

func TestAWarningOnStderrIsNotAParseFailure(t *testing.T) {
	js := `[{"name":"web","namespace":"shop"}]`
	tool := fake(t, "echo 'WARNING: Kubernetes configuration file is group-readable' >&2; echo '"+js+"'")
	out := run(tool, "list -A -o json")
	if strings.Contains(out, "could not be parsed") || !strings.Contains(out, `"web"`) {
		t.Errorf("%s", out)
	}
	if !strings.Contains(out, "helm exited 0, so this is a warning about the client") {
		t.Errorf("the warning is labelled, not lost: %s", out)
	}
}

func TestFailureIsStatedAndUnreachableClusterIsMarked(t *testing.T) {
	tool := fake(t, `echo 'Error: Kubernetes cluster unreachable: Get "https://127.0.0.1:6443/version": dial tcp 127.0.0.1:6443: connect: connection refused' >&2; exit 1`)
	out := run(tool, "list")
	if !strings.HasPrefix(out, "[helm exited 1]") || !strings.Contains(out, "[unavailable]") {
		t.Errorf("%s", out)
	}
	tool = fake(t, `echo 'Error: release: not found' >&2; exit 1`)
	if out := run(tool, "status nope"); !strings.HasPrefix(out, "[helm exited 1] Error: release: not found") || strings.Contains(out, "[unavailable]") {
		t.Errorf("%s", out)
	}
	tool = fake(t, `echo partial; echo 'boom' >&2; exit 2`)
	if out := run(tool, "history x"); !strings.Contains(out, "may be partial") || !strings.HasSuffix(out, "partial") {
		t.Errorf("%s", out)
	}
	tool = fake(t, `exit 0`)
	if out := run(tool, "env"); out != "(no output)" {
		t.Errorf("%q", out)
	}
}

func TestMissingBinaryAndCap(t *testing.T) {
	tool := New(nsguard.Blocklist{}, nil)
	tool.Bin = "/nonexistent/helm"
	if out := run(tool, "list"); !strings.Contains(out, "not found on PATH") || !strings.Contains(out, "[unavailable]") {
		t.Errorf("%s", out)
	}
	big := fake(t, `i=0; while [ $i -lt 2000 ]; do echo "release line number $i with padding text"; i=$((i+1)); done`)
	out := run(big, "history x")
	if !strings.Contains(out, "[truncated:") || len([]rune(out)) > outputCap+300 {
		t.Errorf("len=%d", len([]rune(out)))
	}
}
