package kube

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		args []string
		want Risk
	}{
		{[]string{"get", "pods"}, RiskNone},
		{[]string{"-n", "prod", "get", "pods"}, RiskNone},
		{[]string{"describe", "pod", "x"}, RiskNone},
		{[]string{"rollout", "status", "deploy/x"}, RiskNone},
		{[]string{"rollout", "restart", "deploy/x"}, RiskMedium},
		{[]string{"rollout", "undo", "deploy/x"}, RiskMedium},
		{[]string{"scale", "deploy/x", "--replicas=2"}, RiskMedium},
		{[]string{"delete", "pod", "x"}, RiskHigh},
		{[]string{"drain", "node1"}, RiskHigh},
		{[]string{"label", "pod", "x", "a=b"}, RiskMedium},
		{[]string{"auth", "can-i", "get", "pods"}, RiskNone},
		{[]string{"auth", "reconcile", "-f", "-"}, RiskMedium},
		{[]string{"somenewverb"}, RiskMedium},
	}
	for _, c := range cases {
		if got := Classify(c.args); got != c.want {
			t.Errorf("Classify(%v) = %s, want %s", c.args, got, c.want)
		}
	}
}

func TestCheckRejects(t *testing.T) {
	bad := [][]string{
		{},
		{"get", "pods;rm"},
		{"get", "pods", "$(id)"},
		{"edit", "deploy/x"},
		{"cluster-info", "dump"},
	}
	for _, args := range bad {
		if err := Check(args); err == nil {
			t.Errorf("Check(%v) should fail", args)
		}
	}
	if err := Check([]string{"cluster-info"}); err != nil {
		t.Errorf("bare cluster-info should pass: %v", err)
	}
	if err := Check([]string{"get", "pods", "-o", "jsonpath={.items[*].metadata.name}"}); err != nil {
		t.Errorf("jsonpath should pass: %v", err)
	}
}

func TestAlwaysConfirm(t *testing.T) {
	if !AlwaysConfirm([]string{"delete", "namespace", "x"}) {
		t.Error("delete namespace must always confirm")
	}
	if AlwaysConfirm([]string{"delete", "pod", "x"}) {
		t.Error("delete pod is not always-confirm")
	}
	if !AlwaysConfirm([]string{"set", "image", "deploy/x", "c=img"}) {
		t.Error("set image must always confirm")
	}
}
