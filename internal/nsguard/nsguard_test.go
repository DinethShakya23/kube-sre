package nsguard

import (
	"strings"
	"testing"
)

var b = Blocklist{"kube-system": true, "monitoring": true}

func TestInQuery(t *testing.T) {
	cases := map[string]string{
		`{namespace="kube-system"}`:                "kube-system",
		`{namespace = "Monitoring"}`:               "monitoring",
		`{namespace=~"kube-.*"}`:                   "kube-system",
		`{namespace=~"mon.*|x"}`:                   "monitoring",
		`{namespace!="kube-system"}`:               "",
		`{namespace!~"kube-system"}`:               "",
		`{namespace="shop"}`:                       "",
		`{namespace=~"(unclosed"}`:                 "",
		`sum(rate(x{namespace="monitoring"}[5m]))`: "monitoring",
		`{mynamespace="kube-system"}`:              "",
	}
	for q, want := range cases {
		if got := b.InQuery(q); got != want {
			t.Errorf("%s: got %q want %q", q, got, want)
		}
	}
}

func TestSeriesLabelsLooksInEveryContainer(t *testing.T) {
	loki := map[string]any{"stream": map[string]any{"namespace": "kube-system"}}
	prom := map[string]any{"metric": map[string]any{"namespace": "monitoring"}}
	if SeriesLabels(loki, "metric")["namespace"] != "kube-system" {
		t.Error("a wrong hint must not switch the guard off")
	}
	if SeriesLabels(prom, "")["namespace"] != "monitoring" {
		t.Error("metric container")
	}
	if len(SeriesLabels([]any{1.0, "5"}, "")) != 0 {
		t.Error("a scalar pair has no labels and must not panic")
	}
}

func TestDropSeries(t *testing.T) {
	in := []any{
		map[string]any{"metric": map[string]any{"namespace": "shop"}},
		map[string]any{"metric": map[string]any{"namespace": "Kube-System"}},
		map[string]any{"metric": map[string]any{"node": "n1"}},
		[]any{1.0, "3"},
	}
	out, dropped := b.DropSeries(in, "")
	if len(out) != 3 || dropped != 1 {
		t.Errorf("out=%d dropped=%d", len(out), dropped)
	}
}

func TestDropTableRows(t *testing.T) {
	table := "NAMESPACE     NAME   READY\nshop          web    1/1\nkube-system   dns    1/1\nMonitoring    prom   1/1\n"
	out, dropped := b.DropTableRows(table)
	if dropped != 2 || strings.Contains(out, "dns") || strings.Contains(out, "prom") || !strings.Contains(out, "web") {
		t.Errorf("dropped=%d out=%q", dropped, out)
	}
	if !strings.Contains(out, "NAMESPACE") {
		t.Error("the header row stays")
	}
}

func TestAnnotateKeepsDocumentsParseable(t *testing.T) {
	doc := Annotate(map[string]any{"items": []any{}}, 3, "namespace")
	if s, _ := doc[WithheldKey].(string); !strings.Contains(s, "3 namespace(s) withheld") {
		t.Errorf("%v", doc)
	}
	if _, ok := Annotate(map[string]any{}, 0, "")[WithheldKey]; ok {
		t.Error("nothing dropped, nothing said")
	}
	if !strings.HasPrefix(WithheldNote(2, ""), "\n[Protected] 2 result(s) withheld") {
		t.Error("default noun")
	}
}
