package kube

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/DinethShakya23/kube-sre/internal/nsguard"
)

var blocked = nsguard.Blocklist{"kube-system": true, "monitoring": true}

func filterNS(cmd, out string) string {
	a := toks(cmd)
	return FilterNamespaceOutput(ExtractVerb(a), a, out, blocked)
}

func filterAll(cmd, out string) string {
	a := toks(cmd)
	return FilterAllNamespacesOutput(ExtractVerb(a), a, out, blocked)
}

const nsTable = "NAME          STATUS   AGE\ndefault       Active   5d\nkube-system   Active   5d\nmonitoring    Active   5d\nshop          Active   1d\n"

func TestNamespaceListingIsShortAndSaysSo(t *testing.T) {
	out := filterNS("kubectl get namespaces", nsTable)
	if strings.Contains(out, "kube-system") || strings.Contains(out, "monitoring") || !strings.Contains(out, "shop") {
		t.Errorf("%s", out)
	}
	if !strings.Contains(out, "2 namespace(s) withheld") || !strings.Contains(out, "NOT the complete set") {
		t.Errorf("a short listing must say it is short: %s", out)
	}
	if got := filterNS("kubectl get ns", "NAME STATUS\nshop Active\n"); strings.Contains(got, "withheld") {
		t.Error("nothing dropped, nothing said")
	}
}

func TestNamespaceListingNameFormat(t *testing.T) {
	out := filterNS("kubectl get ns -o name", "namespace/default\nnamespace/kube-system\nnamespace/shop\n")
	if strings.Contains(out, "kube-system") || !strings.Contains(out, "namespace/shop") || !strings.Contains(out, "1 namespace(s)") {
		t.Errorf("%s", out)
	}
}

func TestNamespaceListingJSONStaysParseable(t *testing.T) {
	in := `{"kind":"List","items":[{"metadata":{"name":"shop"}},{"metadata":{"name":"Kube-System"}},{"metadata":{"name":"monitoring"}}]}`
	out := filterNS("kubectl get ns -o json", in)
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output must stay valid JSON: %v\n%s", err, out)
	}
	if items := doc["items"].([]any); len(items) != 1 {
		t.Errorf("items %v", items)
	}
	if s, _ := doc["withheldByPolicy"].(string); !strings.Contains(s, "2 namespace(s)") {
		t.Errorf("the notice belongs inside the document: %v", doc)
	}
}

func TestNamespaceListingYAML(t *testing.T) {
	in := "kind: List\nitems:\n- metadata:\n    name: shop\n- metadata:\n    name: kube-system\n"
	out := filterNS("kubectl get ns -o yaml", in)
	if strings.Contains(out, "kube-system: ") || strings.Contains(out, "name: kube-system") || !strings.Contains(out, "name: shop") || !strings.Contains(out, "withheldByPolicy") {
		t.Errorf("%s", out)
	}
}

func TestUnparseableStructuredListingFailsClosed(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		out := filterNS("kubectl get ns -o "+format, "this is { not a list")
		if !strings.HasPrefix(out, "[Protected]") || strings.Contains(out, "this is") {
			t.Errorf("%s: %s", format, out)
		}
	}
}

func TestCallerShapedFormatsAreRefusedNotFiltered(t *testing.T) {
	// Each of these defeated a token filter and returned every namespace with no note.
	formats := []string{
		"jsonpath={range .items[*]}{.metadata.name}{\",\"}{end}",
		"jsonpath={.items[*].metadata.name}",
		"custom-columns=NAME:.metadata.name",
		"go-template={{range .items}}{{.metadata.name}}{{end}}",
	}
	for _, f := range formats {
		out := filterNS("kubectl get ns -o '"+f+"'", "default,kube-system,monitoring,")
		if !strings.HasPrefix(out, "[Protected]") || strings.Contains(out, "kube-system") {
			t.Errorf("%s: %s", f, out)
		}
	}
}

func TestDescribeNamespaces(t *testing.T) {
	in := "Name:         shop\nLabels:       a=b\nStatus:       Active\n\nName:         kube-system\nLabels:       c=d\nStatus:       Active\n"
	out := filterNS("kubectl describe namespaces", in)
	if strings.Contains(out, "kube-system") || !strings.Contains(out, "shop") || !strings.Contains(out, "1 namespace(s)") {
		t.Errorf("%s", out)
	}
}

func TestNonNamespaceListingsAreUntouched(t *testing.T) {
	if got := filterNS("kubectl get pods", nsTable); got != nsTable {
		t.Error("only namespace listings are filtered here")
	}
}

const podsAll = "NAMESPACE     NAME    READY   STATUS\nshop          web     1/1     Running\nkube-system   coredns 1/1     Running\nmonitoring    prom    1/1     Running\n"

func TestAllNamespacesTable(t *testing.T) {
	out := filterAll("kubectl get pods -A", podsAll)
	if strings.Contains(out, "coredns") || strings.Contains(out, "prom") || !strings.Contains(out, "web") || !strings.Contains(out, "2 result(s) withheld") {
		t.Errorf("%s", out)
	}
	if got := filterAll("kubectl get pods -n shop", podsAll); got != podsAll {
		t.Error("not cluster wide, so nothing to filter")
	}
	if got := filterAll("kubectl get pods -A --no-headers", "shop web 1/1\n"); got != "shop web 1/1\n" {
		t.Errorf("%q", got)
	}
}

func TestAllNamespacesJSONKeepsAValidDocument(t *testing.T) {
	in := `{"items":[{"metadata":{"namespace":"shop","name":"web"}},{"metadata":{"namespace":"kube-system","name":"dns"}}]}`
	out := filterAll("kubectl get pods -A -o json", in)
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("Extra data after the document is the bug this guards: %v", err)
	}
	if len(doc["items"].([]any)) != 1 || doc["withheldByPolicy"] == nil {
		t.Errorf("%v", doc)
	}
	untouched := `{"items":[{"metadata":{"namespace":"shop"}}]}`
	if got := filterAll("kubectl get pods -A -o json", untouched); got != untouched {
		t.Error("nothing dropped means byte for byte")
	}
	if got := filterAll("kubectl get pods -A -o json", "garbage"); !strings.HasPrefix(got, "[Protected]") {
		t.Errorf("fail closed: %s", got)
	}
	if got := filterAll("kubectl get pods -A -o json", `{"kind":"Pod"}`); !strings.HasPrefix(got, "[Protected]") {
		t.Errorf("not a list, fail closed: %s", got)
	}
}

func TestAllNamespacesRefusesShapesWithNoNamespace(t *testing.T) {
	for _, f := range []string{"-o name", "-o jsonpath={.items[*].metadata.name}", "-o custom-columns=A:.metadata.name"} {
		out := filterAll("kubectl get pods -A "+f, "x")
		if !strings.HasPrefix(out, "[Protected]") {
			t.Errorf("%s: %s", f, out)
		}
	}
}

func TestAllNamespacesDescribe(t *testing.T) {
	in := "Name:         web\nNamespace:    shop\nNode:         n1\n\nName:         dns\nNamespace:    kube-system\nNode:         n1\n"
	out := filterAll("kubectl describe pods -A", in)
	if strings.Contains(out, "kube-system") || !strings.Contains(out, "shop") || !strings.Contains(out, "1 result(s) withheld") {
		t.Errorf("%s", out)
	}
}
