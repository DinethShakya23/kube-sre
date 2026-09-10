package change

import (
	"strings"
	"testing"
)

func TestParseKubectl(t *testing.T) {
	cases := []struct {
		cmd, kind, target string
	}{
		{"kubectl scale deploy/web --replicas=3", "scale", "deploy/web"},
		{"kubectl -n shop rollout restart deployment web", "", ""}, // a leading flag has no verb
		{"kubectl set image deploy/web web=img:v2", "image", "deploy/web"},
		{"kubectl set env deploy/web A=1", "env", "deploy/web"},
		{"kubectl set something deploy/web", "config", "deploy/web"},
		{"kubectl patch deployment web -p {}", "config", "deployment"},
		{"kubectl rollout undo deploy/web", "rollout", "undo"},
		{"kubectl drain node-1 --ignore-daemonsets", "node", "node-1"},
		{"delete pod web-1", "delete", "pod"},
		{"kubectl apply -f x.yaml", "apply", "x.yaml"},
	}
	for _, c := range cases {
		r := ParseKubectl(c.cmd, 100, "shop")
		if c.kind == "" {
			if r != nil {
				t.Errorf("%q: %+v", c.cmd, r)
			}
			continue
		}
		if r == nil || r.Kind != c.kind || r.Target != c.target || r.TS != 100 || r.Namespace != "shop" {
			t.Errorf("%q: %+v", c.cmd, r)
		}
	}
	for _, c := range []string{"kubectl get pods", "kubectl describe pod x", "kubectl logs x", "", "kubectl", "kubectl set"} {
		if r := ParseKubectl(c, 1, ""); r != nil {
			t.Errorf("%q must not be a change: %+v", c, r)
		}
	}
	if r := ParseKubectl("kubectl apply -f "+strings.Repeat("x", 300), 1, ""); len(r.Detail) != 120 {
		t.Errorf("detail is bounded: %d", len(r.Detail))
	}
}

func TestLedgerIsBoundedPerClusterAndScopedByNamespace(t *testing.T) {
	l := NewLedger()
	for i := 0; i < maxPerCluster+50; i++ {
		l.Add("c1", Record{Kind: "scale", TS: float64(i)})
	}
	got := l.Recent("c1", "")
	if len(got) != maxPerCluster || got[0].TS != 50 {
		t.Errorf("%d first=%v", len(got), got[0].TS)
	}
	if len(l.Recent("c2", "")) != 0 {
		t.Error("clusters are separate")
	}

	l.Add("c3", Record{Kind: "scale", Namespace: "shop"})
	l.Add("c3", Record{Kind: "config", Namespace: "api"})
	l.Add("c3", Record{Kind: "node"}) // applies everywhere
	if got := l.Recent("c3", "shop"); len(got) != 2 || got[0].Namespace != "shop" || got[1].Kind != "node" {
		t.Errorf("%+v", got)
	}
	if got := l.Recent("c3", ""); len(got) != 3 {
		t.Errorf("%d", len(got))
	}
}

func TestRecordCommandsCountsOnlyChanges(t *testing.T) {
	l := NewLedger()
	if n := l.RecordCommands("c1", []string{"kubectl scale deploy/web --replicas=2", "kubectl get pods", "kubectl delete pod x"}, 10, "shop"); n != 2 {
		t.Errorf("%d", n)
	}
	if len(l.Recent("c1", "shop")) != 2 {
		t.Error("recorded")
	}
}

func TestPriorRanksMostRecentFirstAndSaysNothingWhenEmpty(t *testing.T) {
	if RenderPrior(nil, 5, 0) != "" {
		t.Error("no changes, no block")
	}
	changes := []Record{
		{Kind: "config", Target: "cm/a", TS: 100, Detail: "old"},
		{Kind: "scale", Target: "deploy/web", TS: 900, Namespace: "shop", Detail: "kubectl scale deploy/web --replicas=0"},
		{Kind: "image", Target: "deploy/api", TS: 500},
	}
	got := RenderPrior(changes, 2, 960)
	lines := strings.Split(got, "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "## Recent changes (consider these FIRST") ||
		lines[1] != "- scale deploy/web in shop (1m ago) — kubectl scale deploy/web --replicas=0" || lines[2] != "- image deploy/api (7m ago)" {
		t.Errorf("%q", got)
	}
	if strings.Contains(RenderPrior(changes, 5, 0), "ago") {
		t.Error("no clock, no age")
	}
	if RankByRecency(changes)[0].Target != "deploy/web" || changes[0].Target != "cm/a" {
		t.Error("ranking must not reorder the caller's slice")
	}
}
