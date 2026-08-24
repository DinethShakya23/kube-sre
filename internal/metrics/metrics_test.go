package metrics

import (
	"strings"
	"testing"
)

func TestCounterAndHistogramExposition(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("jobs_total", "Jobs.", "kind", "outcome")
	c.Inc("a", "ok")
	c.Add(2, "a", "ok")
	c.Inc("b", `we"ird`)
	c.Add(-1, "a", "ok")
	if c.Value("a", "ok") != 3 {
		t.Errorf("negative adds are ignored: %v", c.Value("a", "ok"))
	}
	h := r.Histogram("dur_seconds", "Duration.", []float64{1, 5}, "tool")
	h.Observe(0.5, "x")
	h.Observe(3, "x")
	h.Observe(9, "x")
	var b strings.Builder
	r.Write(&b)
	out := b.String()
	for _, want := range []string{
		"# TYPE jobs_total counter", `jobs_total{kind="a",outcome="ok"} 3`, `jobs_total{kind="b",outcome="we\"ird"} 1`,
		`dur_seconds_bucket{tool="x",le="1"} 1`, `dur_seconds_bucket{tool="x",le="5"} 2`, `dur_seconds_bucket{tool="x",le="+Inf"} 3`,
		`dur_seconds_sum{tool="x"} 12.5`, `dur_seconds_count{tool="x"} 3`, "go_goroutines",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestNilAndUnlabelledAreSafe(t *testing.T) {
	var c *Counter
	c.Inc("x")
	var h *Histogram
	h.Observe(1)
	r := NewRegistry()
	u := r.Counter("plain_total", "p")
	u.Inc()
	var b strings.Builder
	r.Write(&b)
	if !strings.Contains(b.String(), "plain_total 1") {
		t.Errorf("%s", b.String())
	}
	if strings.Contains(b.String(), "# TYPE never_used") {
		t.Error("empty families are omitted")
	}
}
