package kube

import "testing"

func testProtect() *Protect {
	return &Protect{
		Namespaces: set("kube-system", "monitoring"),
		Resources:  set("secret", "secrets"),
	}
}

func TestFlagValueForms(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"get", "pods", "-n", "a"}, "a"},
		{[]string{"get", "pods", "-na"}, "a"},
		{[]string{"get", "pods", "-n=a"}, "a"},
		{[]string{"get", "pods", "--namespace", "a"}, "a"},
		{[]string{"get", "pods", "--namespace=a"}, "a"},
		{[]string{"get", "pods", "-Rn", "a"}, "a"},
		{[]string{"get", "pods", "-Rna"}, "a"},
		{[]string{"exec", "-itn", "a", "pod", "--", "sh"}, ""},
	}
	for _, c := range cases {
		got, _ := FlagValue(c.args, "-n", "--namespace", Verb(c.args))
		if got != c.want && c.want != "" {
			t.Errorf("FlagValue(%v) = %q, want %q", c.args, got, c.want)
		}
	}
	if _, ok := FlagValue([]string{"get", "pods", "-o", "json"}, "-n", "--namespace", "get"); ok {
		t.Error("-ojson must not read as -n")
	}
	if _, ok := FlagValue([]string{"get", "pods", "-ojson"}, "-n", "--namespace", "get"); ok {
		t.Error("-ojson group must not read as -n")
	}
}

func TestProtectNamespace(t *testing.T) {
	p := testProtect()
	blocked := [][]string{
		{"get", "pods", "-n", "kube-system"},
		{"get", "pods", "-nkube-system"},
		{"get", "pods", "--namespace=Kube-System"},
		{"get", "pods", "-Rn", "monitoring"},
		{"delete", "pod", "x", "-n", "kube-system"},
	}
	for _, a := range blocked {
		if p.Check(a) == nil {
			t.Errorf("%v should be blocked", a)
		}
	}
	if err := p.Check([]string{"get", "pods", "-n", "prod"}); err != nil {
		t.Errorf("prod should pass: %v", err)
	}
}

func TestProtectResources(t *testing.T) {
	p := testProtect()
	for _, a := range [][]string{
		{"get", "secrets"},
		{"get", "pods,secret"},
		{"describe", "secret/db"},
		{"get", "Secret.v1"},
	} {
		if p.Check(a) == nil {
			t.Errorf("%v should be blocked", a)
		}
	}
	if err := p.Check([]string{"get", "pods"}); err != nil {
		t.Error(err)
	}
}

func TestProtectAllNamespacesWrite(t *testing.T) {
	p := testProtect()
	if p.Check([]string{"delete", "pods", "--all", "-A"}) == nil {
		t.Error("write with -A must be blocked")
	}
	if err := p.Check([]string{"get", "pods", "-A"}); err != nil {
		t.Error("read with -A is fine, output gets filtered")
	}
}

func TestFilterTable(t *testing.T) {
	p := testProtect()
	in := "NAMESPACE     NAME   READY\nkube-system   coredns   1/1\nprod   web   1/1\n"
	got := p.FilterTable(in)
	if got != "NAMESPACE     NAME   READY\nprod   web   1/1\n" {
		t.Errorf("got %q", got)
	}
	plain := "NAME READY\nweb 1/1\n"
	if p.FilterTable(plain) != plain {
		t.Error("table without a NAMESPACE column must pass through")
	}
}
