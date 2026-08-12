package prom

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DinethShakya23/kube-sre/internal/nsguard"
)

func server(t *testing.T, body string, status int) (*Client, *http.Request) {
	t.Helper()
	var last http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = *r
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, nsguard.Blocklist{"kube-system": true, "monitoring": true})
	return c, &last
}

func ok(resultType, result string) string {
	return `{"status":"success","data":{"resultType":"` + resultType + `","result":` + result + `}}`
}

func TestInstantVector(t *testing.T) {
	c, req := server(t, ok("vector", `[{"metric":{"namespace":"shop","pod":"web"},"value":[1700000000,"3"]}]`), 200)
	out := c.Query(context.Background(), `up{namespace="shop"}`, 0)
	if !strings.Contains(out, "PromQL (instant)") || !strings.Contains(out, "[namespace=shop, pod=web] = 3") {
		t.Errorf("%s", out)
	}
	if req.URL.Path != "/api/v1/query" || req.URL.Query().Get("query") != `up{namespace="shop"}` {
		t.Errorf("%s", req.URL)
	}
}

func TestRangeQueryPicksStepAndSummarises(t *testing.T) {
	c, req := server(t, ok("matrix", `[{"metric":{"pod":"a"},"values":[[1,"1"],[2,"3"],[3,"NaN"],[4,"+Inf"]]},{"metric":{"pod":"b"},"values":[[1,"NaN"]]}]`), 200)
	out := c.Query(context.Background(), "m", 60)
	if !strings.Contains(out, "PromQL (range 60m)") || !strings.Contains(out, "[pod=a]  min=1.0000  avg=2.0000  max=3.0000") ||
		!strings.Contains(out, "[pod=b] no numeric values") {
		t.Errorf("%s", out)
	}
	if req.URL.Path != "/api/v1/query_range" || req.URL.Query().Get("step") != "36s" {
		t.Errorf("%s", req.URL)
	}
	for minutes, want := range map[int]string{1: "15s", 30: "18s", 60: "36s", 600: "6m", 1440: "14m"} {
		if got := autoStep(minutes); got != want {
			t.Errorf("step(%d) = %s want %s", minutes, got, want)
		}
	}
}

func TestMatrixFromAnInstantQueryIsRenderedAsARange(t *testing.T) {
	// an instant query with a range selector answers with a matrix, and has no `value`
	c, _ := server(t, ok("matrix", `[{"metric":{"pod":"a"},"values":[[1,"2"],[2,"4"]]}]`), 200)
	out := c.Query(context.Background(), "m[5m]", 0)
	if !strings.Contains(out, "range selector") || strings.Contains(out, "N/A") || !strings.Contains(out, "avg=3.0000") {
		t.Errorf("%s", out)
	}
}

func TestScalarAndStringDoNotReachTheFilter(t *testing.T) {
	c, _ := server(t, ok("scalar", `[1700000000,"42"]`), 200)
	if out := c.Query(context.Background(), "scalar(x)", 0); !strings.Contains(out, "PromQL (scalar)") || !strings.Contains(out, "= 42") {
		t.Errorf("%s", out)
	}
	c, _ = server(t, ok("string", `"oops"`), 200)
	if out := c.Query(context.Background(), `"x"`, 0); !strings.Contains(out, "unreadable string result") {
		t.Errorf("%s", out)
	}
}

func TestNamespaceGuardInputAndOutput(t *testing.T) {
	c, _ := server(t, ok("vector", `[]`), 200)
	if out := c.Query(context.Background(), `up{namespace="kube-system"}`, 0); !strings.HasPrefix(out, "[Protected]") {
		t.Errorf("input gate: %s", out)
	}
	c, _ = server(t, ok("vector", `[{"metric":{"namespace":"shop"},"value":[1,"1"]},{"metric":{"namespace":"monitoring"},"value":[1,"2"]},{"metric":{"node":"n1"},"value":[1,"3"]}]`), 200)
	out := c.Query(context.Background(), "up", 0)
	if strings.Contains(out, "monitoring") || !strings.Contains(out, "namespace=shop") || !strings.Contains(out, "node=n1") ||
		!strings.Contains(out, "1 result(s) withheld") {
		t.Errorf("output gate: %s", out)
	}
	c, _ = server(t, ok("vector", `[{"metric":{"namespace":"kube-system"},"value":[1,"1"]}]`), 200)
	if out := c.Query(context.Background(), "up", 0); !strings.HasPrefix(out, "[Protected] All 1 result(s)") {
		t.Errorf("everything withheld: %s", out)
	}
	c, _ = server(t, ok("vector", `[]`), 200)
	if out := c.Query(context.Background(), "up", 0); out != "No data for query: up" {
		t.Errorf("%s", out)
	}
}

func TestErrorsAreWordedForTheModel(t *testing.T) {
	c, _ := server(t, `oops`, 500)
	if out := c.Query(context.Background(), "up", 0); !strings.HasPrefix(out, "Prometheus HTTP 500: oops") {
		t.Errorf("%s", out)
	}
	c, _ = server(t, `{"status":"error","error":"bad query"}`, 200)
	if out := c.Query(context.Background(), "up", 0); out != "Prometheus error: bad query" {
		t.Errorf("%s", out)
	}
	if out := New("", nil).Query(context.Background(), "up", 0); !strings.Contains(out, "not configured") || !strings.Contains(out, "[unavailable]") {
		t.Errorf("%s", out)
	}
	dead := New("127.0.0.1:1", nil)
	if out := dead.Query(context.Background(), "up", 0); !strings.Contains(out, "Cannot reach Prometheus") || !strings.Contains(out, "[unavailable]") {
		t.Errorf("%s", out)
	}
	if u := New("prom.local:9090/", nil).BaseURL(); u != "http://prom.local:9090" {
		t.Errorf("base url %q", u)
	}
}

func TestOutputIsCapped(t *testing.T) {
	var items []string
	for i := 0; i < 50; i++ {
		items = append(items, `{"metric":{"pod":"`+strings.Repeat("p", 200)+`"},"value":[1,"1"]}`)
	}
	c, _ := server(t, ok("vector", "["+strings.Join(items, ",")+"]"), 200)
	out := c.Query(context.Background(), "up", 0)
	if !strings.Contains(out, "[truncated") || len([]rune(out)) > outputCap+100 {
		t.Errorf("len %d", len([]rune(out)))
	}
}

func TestQuerySeriesForDetectors(t *testing.T) {
	c, _ := server(t, ok("matrix", `[{"metric":{"namespace":"kube-system","pod":"dns"},"values":[[1,"0.5"],[2,"0.7"],[3,"NaN"]]}]`), 200)
	series, err := c.QuerySeries(context.Background(), "m", 30)
	if err != nil || len(series) != 1 {
		t.Fatalf("%v %v", series, err)
	}
	// detectors are meant to watch protected namespaces, so nothing is filtered.
	// NaN samples come back as they are; the engine is what drops them.
	if series[0].Metric["namespace"] != "kube-system" || len(series[0].Values) != 3 || series[0].Values[1] != [2]float64{2, 0.7} {
		t.Errorf("%+v", series[0])
	}
	c, _ = server(t, ok("scalar", `[1,"2"]`), 200)
	if _, err := c.QuerySeries(context.Background(), "m", 0); err == nil || !strings.Contains(err.Error(), "carries no labelled series") {
		t.Errorf("a scalar is an error, not an empty list: %v", err)
	}
	c, _ = server(t, `nope`, 502)
	if _, err := c.QuerySeries(context.Background(), "m", 0); err == nil {
		t.Error("an outage is an error")
	}
	c, _ = server(t, ok("matrix", `[]`), 200)
	if s, err := c.QuerySeries(context.Background(), "m", 0); err != nil || len(s) != 0 {
		t.Errorf("no data is not an error: %v %v", s, err)
	}
}
