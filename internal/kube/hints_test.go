package kube

import (
	"strings"
	"testing"
)

func TestInterpret(t *testing.T) {
	cases := map[string]string{
		`Error from server (NotFound): namespaces "x" not found`:                    "namespace_not_found",
		`Error from server (NotFound): pods "web-1" not found`:                      "pod_not_found",
		`Error from server (NotFound): deployments.apps "d" not found`:              "not_found",
		`Error from server (Forbidden): pods is forbidden`:                          "forbidden",
		`The connection to the server 127.0.0.1:6443 was refused - did you specify`: "apiserver_unreachable",
		`Unable to connect to the server: x509`:                                     "unable_to_connect",
		`Unable to connect to the server: dial tcp: lookup foo`:                     "unable_to_connect",
		`error: unable to recognize "x.yaml": no matches for kind`:                  "crd_not_recognized",
		`Operation cannot be fulfilled on pods "a": the object has been modified`:   "concurrent_modification",
	}
	for in, want := range cases {
		if got, _ := Interpret(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	if name, hint := Interpret("something odd"); name != "" || hint != "" {
		t.Error("unknown errors get no hint")
	}
	if name, _ := Interpret(""); name != "" {
		t.Error("empty stderr gets no hint")
	}
}

func TestAnnotateKeepsOriginal(t *testing.T) {
	orig := `Error from server (Forbidden): nope`
	out, name := Annotate(orig)
	if !strings.HasPrefix(out, orig) || name != "forbidden" || !strings.Contains(out, "-> ") {
		t.Errorf("out=%q name=%q", out, name)
	}
	if out2, n := Annotate("plain"); out2 != "plain" || n != "" {
		t.Error("no match must leave the text alone")
	}
}

func TestTerminal(t *testing.T) {
	if !IsTerminal("apiserver_unreachable") || IsTerminal("forbidden") {
		t.Error("only connectivity failures are terminal")
	}
}
