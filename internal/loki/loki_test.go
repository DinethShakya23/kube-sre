package loki

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
	return New(srv.URL, nsguard.Blocklist{"kube-system": true, "monitoring": true}), &last
}

const streams = `{"data":{"resultType":"streams","result":[
 {"stream":{"namespace":"shop","pod":"web"},"values":[["1700000000000000000","GET / 200"],["1700000001000000000","boom"]]},
 {"stream":{"namespace":"kube-system","pod":"dns"},"values":[["1700000002000000000","secret leaked"]]}]}}`

func TestLogLinesAreFilteredByStreamNamespace(t *testing.T) {
	c, req := server(t, streams, 200)
	out := c.Query(context.Background(), `{app="x"}`, 100, "1h")
	if strings.Contains(out, "secret leaked") || strings.Contains(out, "kube-system") || !strings.Contains(out, "GET / 200") ||
		!strings.Contains(out, "Streams: 1") || !strings.Contains(out, "1 result(s) withheld") {
		t.Errorf("%s", out)
	}
	if !strings.Contains(out, "22:13:20  GET / 200") || !strings.Contains(out, "[namespace=shop, pod=web]") {
		t.Errorf("format: %s", out)
	}
	if req.URL.Query().Get("direction") != "backward" || req.URL.Query().Get("limit") != "100" {
		t.Errorf("%s", req.URL)
	}
}

func TestInputGateAndLimitCap(t *testing.T) {
	c, req := server(t, streams, 200)
	if out := c.Query(context.Background(), `{namespace="kube-system"}`, 10, "1h"); !strings.HasPrefix(out, "[Protected]") {
		t.Errorf("%s", out)
	}
	c.Query(context.Background(), `{app="x"}`, 9999, "1h")
	if req.URL.Query().Get("limit") != "500" {
		t.Errorf("limit %s", req.URL.Query().Get("limit"))
	}
}

func TestEverythingWithheldAndNoLogs(t *testing.T) {
	c, _ := server(t, `{"data":{"resultType":"streams","result":[{"stream":{"namespace":"monitoring"},"values":[]}]}}`, 200)
	if out := c.Query(context.Background(), `{app="x"}`, 10, "1h"); !strings.HasPrefix(out, "[Protected] All 1 result(s)") {
		t.Errorf("%s", out)
	}
	c, _ = server(t, `{"data":{"resultType":"streams","result":[]}}`, 200)
	if out := c.Query(context.Background(), `{app="x"}`, 10, "1h"); out != `No logs found for: {app="x"}` {
		t.Errorf("%s", out)
	}
}

func TestMetricQueryDetectionCoversTheIdiomaticForms(t *testing.T) {
	yes := []string{
		`rate({app="x"}[5m])`, `sum by (namespace) (rate({app="x"}[5m]))`, `sum(rate({a="b"}[1m]))`, `topk(5, rate({a="b"}[1m]))`,
		`(rate({a="b"}[1m]))`, `count_over_time({a="b"}[5m])`, `avg_over_time({a="b"} | unwrap x [5m])`, `sum without (pod) (rate({a="b"}[1m]))`,
		`quantile_over_time(0.9, {a="b"} | unwrap x [5m])`,
	}
	no := []string{`{app="x"}`, `{app="x"} |= "error"`, `{app="x"} | json | status >= 500`, ``}
	for _, q := range yes {
		if !isMetricQuery(q) {
			t.Errorf("%q is a metric query", q)
		}
	}
	for _, q := range no {
		if isMetricQuery(q) {
			t.Errorf("%q is a log query", q)
		}
	}
}

func TestMisroutedMetricQueryStillHitsTheNamespaceFilter(t *testing.T) {
	// routed as a log query, but Loki answers with a matrix, which must be filtered as one
	body := `{"data":{"resultType":"matrix","result":[
	  {"metric":{"namespace":"shop"},"values":[[1,"1"],[2,"3"]]},
	  {"metric":{"namespace":"kube-system"},"values":[[1,"9"]]}]}}`
	c, _ := server(t, body, 200)
	out := c.Query(context.Background(), `{app="x"} | logfmt`, 10, "1h")
	if strings.Contains(out, "kube-system") || !strings.Contains(out, "namespace=shop") || !strings.Contains(out, "avg=2.0000") ||
		!strings.Contains(out, "1 result(s) withheld") {
		t.Errorf("%s", out)
	}
	// and the other way round: a log query that returns streams
	c, _ = server(t, streams, 200)
	out = c.Query(context.Background(), `sum by (namespace) (rate({a="b"}[1m]))`, 10, "1h")
	if strings.Contains(out, "secret leaked") {
		t.Errorf("a streams payload must be filtered even when the query looked like a metric: %s", out)
	}
}

func TestMetricRender(t *testing.T) {
	body := `{"data":{"resultType":"matrix","result":[{"metric":{"pod":"a"},"values":[[1,"1"],[2,"NaN"],[3,"5"]]}]}}`
	c, req := server(t, body, 200)
	out := c.Query(context.Background(), `rate({app="x"}[5m])`, 10, "2h")
	if !strings.Contains(out, "LogQL (metric)") || !strings.Contains(out, "[pod=a]  min=1.0000  avg=3.0000  max=5.0000") {
		t.Errorf("%s", out)
	}
	if req.URL.Query().Get("step") != "72s" || req.URL.Query().Get("limit") != "" {
		t.Errorf("range queries send a step and no limit: %s", req.URL)
	}
}

func TestLineCap(t *testing.T) {
	var vals []string
	for i := 0; i < 300; i++ {
		vals = append(vals, `["1700000000000000000","line"]`)
	}
	c, _ := server(t, `{"data":{"resultType":"streams","result":[{"stream":{"namespace":"shop"},"values":[`+strings.Join(vals, ",")+`]}]}}`, 200)
	out := c.Query(context.Background(), `{app="x"}`, 500, "1h")
	if strings.Count(out, "  line") != logLineCap || !strings.Contains(out, "capped at 200 lines") {
		t.Errorf("%d lines", strings.Count(out, "  line"))
	}
}

func TestErrorsAndDurations(t *testing.T) {
	c, _ := server(t, "bad gateway", 502)
	if out := c.Query(context.Background(), `{a="b"}`, 10, "1h"); !strings.HasPrefix(out, "Loki HTTP 502: bad gateway") {
		t.Errorf("%s", out)
	}
	if out := New("", nil).Query(context.Background(), `{a="b"}`, 10, "1h"); !strings.Contains(out, "not configured") || !strings.Contains(out, "[unavailable]") {
		t.Errorf("%s", out)
	}
	if out := New("127.0.0.1:1", nil).Query(context.Background(), `{a="b"}`, 10, "1h"); !strings.Contains(out, "Cannot reach Loki") || !strings.Contains(out, "[unavailable]") {
		t.Errorf("%s", out)
	}
	for in, want := range map[string]int64{"15m": 900e9, "1h": 3600e9, "2d": 172800e9, "30s": 30e9, "bad": 3600e9, "": 3600e9, "5x": 5 * 3600e9, "xh": 3600e9} {
		if got := parseDurationNS(in); got != want {
			t.Errorf("%q: %d want %d", in, got, want)
		}
	}
}
