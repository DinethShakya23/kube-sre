// Package metrics is the server's own metrics, served in the Prometheus text
// format at /metrics.
//
// Beyond the generic request counts and latencies it answers what the server
// actually does: which tools the agent called and whether they worked, how often
// the approval gate fires (the safety property the product is sold on), and what
// the model calls cost.
//
// Labels are bounded on purpose. A label must come from a closed set (the tool
// registry, an outcome), never from a session, namespace, cluster, pod or user:
// an unbounded label on a counter is how a metrics endpoint becomes an outage.
// Nothing here may break a request, so recording never returns an error.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type series struct {
	labels []string
	value  float64
	// histogram
	counts []float64
	sum    float64
	total  float64
}

type family struct {
	name, help, kind string
	labelNames       []string
	buckets          []float64
	mu               sync.Mutex
	series           map[string]*series
}

// Registry holds metric families.
type Registry struct {
	mu       sync.Mutex
	families []*family
	started  time.Time
}

func NewRegistry() *Registry { return &Registry{started: time.Now()} }

// Default is the process wide registry.
var Default = NewRegistry()

func (r *Registry) add(f *family) *family {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.families = append(r.families, f)
	return f
}

// Counter is a monotonically increasing value per label set.
type Counter struct{ f *family }

func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	return &Counter{r.add(&family{name: name, help: help, kind: "counter", labelNames: labels, series: map[string]*series{}})}
}

func (f *family) get(lv []string) *series {
	key := strings.Join(lv, "\x00")
	s, ok := f.series[key]
	if !ok {
		s = &series{labels: append([]string(nil), lv...)}
		if f.kind == "histogram" {
			s.counts = make([]float64, len(f.buckets))
		}
		f.series[key] = s
	}
	return s
}

// Add increases the counter. Missing label values are left empty.
func (c *Counter) Add(n float64, labelValues ...string) {
	if c == nil || n < 0 {
		return
	}
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.get(pad(labelValues, len(c.f.labelNames))).value += n
}

func (c *Counter) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Value reads a counter, for tests.
func (c *Counter) Value(labelValues ...string) float64 {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	return c.f.get(pad(labelValues, len(c.f.labelNames))).value
}

// Histogram counts observations into buckets.
type Histogram struct{ f *family }

func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) *Histogram {
	b := append([]float64(nil), buckets...)
	sort.Float64s(b)
	return &Histogram{r.add(&family{name: name, help: help, kind: "histogram", labelNames: labels, buckets: b, series: map[string]*series{}})}
}

func (h *Histogram) Observe(v float64, labelValues ...string) {
	if h == nil {
		return
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	s := h.f.get(pad(labelValues, len(h.f.labelNames)))
	for i, ub := range h.f.buckets {
		if v <= ub {
			s.counts[i]++
		}
	}
	s.sum += v
	s.total++
}

func pad(v []string, n int) []string {
	out := make([]string, n)
	copy(out, v)
	return out
}

func esc(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(s)
}

func labelText(names, values []string, extra ...string) string {
	var parts []string
	for i, n := range names {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, n, esc(values[i])))
	}
	parts = append(parts, extra...)
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// WriteTo renders the registry in the Prometheus text exposition format.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	fams := append([]*family(nil), r.families...)
	r.mu.Unlock()
	sort.Slice(fams, func(i, j int) bool { return fams[i].name < fams[j].name })
	for _, f := range fams {
		f.mu.Lock()
		if len(f.series) == 0 {
			f.mu.Unlock()
			continue
		}
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.kind)
		keys := make([]string, 0, len(f.series))
		for k := range f.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := f.series[k]
			if f.kind == "counter" {
				fmt.Fprintf(w, "%s%s %v\n", f.name, labelText(f.labelNames, s.labels), s.value)
				continue
			}
			for i, ub := range f.buckets {
				fmt.Fprintf(w, "%s_bucket%s %v\n", f.name, labelText(f.labelNames, s.labels, fmt.Sprintf(`le="%v"`, ub)), s.counts[i])
			}
			fmt.Fprintf(w, "%s_bucket%s %v\n", f.name, labelText(f.labelNames, s.labels, `le="+Inf"`), s.total)
			fmt.Fprintf(w, "%s_sum%s %v\n%s_count%s %v\n", f.name, labelText(f.labelNames, s.labels), s.sum,
				f.name, labelText(f.labelNames, s.labels), s.total)
		}
		f.mu.Unlock()
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Fprintf(w, "# TYPE go_goroutines gauge\ngo_goroutines %d\n", runtime.NumGoroutine())
	fmt.Fprintf(w, "# TYPE go_memstats_alloc_bytes gauge\ngo_memstats_alloc_bytes %d\n", ms.Alloc)
	fmt.Fprintf(w, "# TYPE process_start_time_seconds gauge\nprocess_start_time_seconds %d\n", r.started.Unix())
}

// Handler serves the registry.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Write(w)
	})
}

// Domain families, on the default registry.
var (
	ToolCalls = Default.Counter("kubesre_tool_calls_total", "Tool invocations by the agent, by tool and outcome.", "tool", "outcome")
	// A kubectl call lands near 0.1s and an LLM backed subagent near 60s; the default
	// buckets top out at 10s and would put both in +Inf.
	ToolDuration = Default.Histogram("kubesre_tool_duration_seconds", "Wall clock duration of one tool invocation.",
		[]float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}, "tool")
	HitlInterrupts = Default.Counter("kubesre_hitl_interrupts_total", "Approval gates raised, by the tool that raised one.", "tool")
	LLMCalls       = Default.Counter("kubesre_llm_calls_total", "Completed model calls, summed across every node of a request.")
	LLMTokens      = Default.Counter("kubesre_llm_tokens_total", "Tokens consumed, by direction.", "direction")

	Requests = Default.Counter("http_requests_total", "HTTP requests by method, handler and status.", "method", "handler", "status")
	Latency  = Default.Histogram("http_request_duration_seconds", "HTTP request duration.",
		[]float64{0.01, 0.05, 0.1, 0.5, 1, 5, 30, 120, 600}, "method", "handler")
)
