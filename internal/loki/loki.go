// Package loki is query_loki: LogQL log and metric queries.
//
// It covers what kubectl logs cannot: aggregation across pods and namespaces,
// history beyond a container's buffer, structured filtering, and log rate
// metrics. Output is capped to stay inside model context budgets.
package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/nsguard"
	"github.com/DinethShakya23/kube-sre/internal/policy"
)

const (
	outputCap      = 6000
	logLineCap     = 200
	requestTimeout = 15 * time.Second
)

type Client struct {
	URL     string
	Blocked nsguard.Blocklist
	HTTP    *http.Client
}

func New(url string, blocked nsguard.Blocklist) *Client {
	return &Client{URL: url, Blocked: blocked, HTTP: &http.Client{Timeout: requestTimeout}}
}

// Every LogQL function that yields a metric result. Testing the text for a
// handful of prefixes missed `sum by (namespace) (rate(...))`, every *_over_time
// beyond two, topk, and a leading parenthesis: seven of ten ordinary expressions.
var metricFns = map[string]bool{
	"rate": true, "rate_counter": true, "bytes_rate": true,
	"count_over_time": true, "bytes_over_time": true, "absent_over_time": true, "sum_over_time": true,
	"avg_over_time": true, "max_over_time": true, "min_over_time": true, "first_over_time": true,
	"last_over_time": true, "stdvar_over_time": true, "stddev_over_time": true, "quantile_over_time": true,
	"sum": true, "avg": true, "max": true, "min": true, "count": true, "stddev": true, "stdvar": true,
	"topk": true, "bottomk": true, "sort": true, "sort_desc": true, "label_replace": true,
	"vector": true, "approx_topk": true,
}

// The leading identifier, past any opening parentheses, followed by `(`, `by` or `without`.
var leadingFn = regexp.MustCompile(`(?i)^[\s(]*([a-z_][a-z0-9_]*)\s*(?:\(|by\b|without\b)`)

// isMetricQuery only decides which request parameters to send. What the response
// is rendered and filtered as comes from Loki's own resultType, so a wrong answer
// here can no longer switch the namespace filter off.
func isMetricQuery(logql string) bool {
	m := leadingFn.FindStringSubmatch(strings.TrimSpace(logql))
	return m != nil && metricFns[strings.ToLower(m[1])]
}

func parseDurationNS(since string) int64 {
	since = strings.ToLower(strings.TrimSpace(since))
	units := map[byte]int64{'s': 1, 'm': 60, 'h': 3600, 'd': 86400}
	hour := int64(3600) * 1e9
	if len(since) < 2 {
		return hour
	}
	n, err := strconv.Atoi(since[:len(since)-1])
	if err != nil {
		return hour
	}
	unit, ok := units[since[len(since)-1]]
	if !ok {
		unit = 3600
	}
	return int64(n) * unit * 1e9
}

func (c *Client) baseURL() string {
	u := strings.TrimSpace(c.URL)
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
		slog.Warn("LOKI_URL is missing a protocol", "using", u)
	}
	return strings.TrimRight(u, "/")
}

// Query is the tool the model calls. limit is capped at 500 and ignored for
// metric queries.
func (c *Client) Query(ctx context.Context, logql string, limit int, since string) string {
	if strings.TrimSpace(c.URL) == "" {
		return policy.MarkUnavailable("Loki is not configured. Set LOKI_URL in ~/.kube-sre/.env and restart.",
			"Loki is not configured.")
	}
	if ns := c.Blocked.InQuery(logql); ns != "" {
		slog.Warn("query_loki refused, query selects a blocked namespace", "namespace", ns)
		return nsguard.ProtectedMessage(ns)
	}
	base := c.baseURL()
	if limit > 500 {
		limit = 500
	}
	if limit <= 0 {
		limit = 100
	}
	now := time.Now().UnixNano()
	start := now - parseDurationNS(since)

	var out string
	var err error
	if isMetricQuery(logql) {
		out, err = c.rangeQuery(ctx, base, logql, start, now)
	} else {
		out, err = c.logQuery(ctx, base, logql, limit, start, now)
	}
	if err != nil {
		s := strings.ToLower(err.Error())
		switch {
		case strings.Contains(s, "timeout") || strings.Contains(s, "deadline exceeded"):
			return "Loki query timed out (15s)."
		case strings.Contains(s, "connection refused") || strings.Contains(s, "no such host") || strings.Contains(s, "dial tcp"):
			return policy.MarkUnavailable(fmt.Sprintf("Cannot reach Loki at %s. Is Loki deployed? (make install-loki-kind)", base),
				"Loki cannot be reached.")
		}
		return "Loki error: " + err.Error()
	}
	if r := []rune(out); len(r) > outputCap {
		out = string(r[:outputCap]) + "\n... [truncated - use a narrower query or shorter since]"
	}
	return out
}

type lokiResp struct {
	Data struct {
		ResultType string `json:"resultType"`
		Result     []any  `json:"result"`
	} `json:"data"`
}

func (c *Client) get(ctx context.Context, url string, params map[string]string) (*lokiResp, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	q := req.URL.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	req.URL.RawQuery = q.Encode()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		text := string(body)
		if r := []rune(text); len(r) > 500 {
			text = string(r[:500])
		}
		return nil, fmt.Sprintf("Loki HTTP %d: %s", resp.StatusCode, text), nil
	}
	var out lokiResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, "", err
	}
	return &out, "", nil
}

func (c *Client) logQuery(ctx context.Context, base, logql string, limit int, start, end int64) (string, error) {
	data, msg, err := c.get(ctx, base+"/loki/api/v1/query_range", map[string]string{
		"query": logql, "limit": strconv.Itoa(limit), "start": strconv.FormatInt(start, 10),
		"end": strconv.FormatInt(end, 10), "direction": "backward",
	})
	if err != nil || msg != "" {
		return msg, err
	}
	// Loki says what it returned. Rendering a matrix as log lines prints numbers
	// with no labels, which is how a misrouted metric query looked like an empty answer.
	if data.Data.ResultType == "matrix" || data.Data.ResultType == "vector" {
		return c.renderMetric(logql, data), nil
	}
	return c.logLines(logql, data), nil
}

func labelPairs(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%v", k, m[k])
	}
	return strings.Join(parts, ", ")
}

func fmtTS(tsNS int64) string {
	return time.Unix(0, tsNS).UTC().Format("15:04:05")
}

func (c *Client) logLines(logql string, data *lokiResp) string {
	streams, dropped := c.Blocked.DropSeries(data.Data.Result, "stream")
	if len(streams) == 0 {
		if dropped > 0 {
			return nsguard.AllWithheldMessage(dropped)
		}
		return "No logs found for: " + logql
	}
	lines := []string{"LogQL: " + logql, fmt.Sprintf("Streams: %d", len(streams))}
	if dropped > 0 {
		lines = append(lines, strings.TrimSpace(nsguard.WithheldNote(dropped, "")))
	}
	total := 0
	for _, s := range streams {
		m, _ := s.(map[string]any)
		labels, _ := m["stream"].(map[string]any)
		lines = append(lines, "\n["+labelPairs(labels)+"]")
		values, _ := m["values"].([]any)
		for _, v := range values {
			pair, ok := v.([]any)
			if !ok || len(pair) < 2 {
				continue
			}
			ts, _ := strconv.ParseInt(fmt.Sprint(pair[0]), 10, 64)
			lines = append(lines, fmt.Sprintf("  %s  %v", fmtTS(ts), pair[1]))
			total++
			if total >= logLineCap {
				lines = append(lines, fmt.Sprintf("  ... (capped at %d lines - use a shorter since or lower limit)", logLineCap))
				return strings.Join(lines, "\n")
			}
		}
	}
	return strings.Join(lines, "\n")
}

func (c *Client) rangeQuery(ctx context.Context, base, logql string, start, end int64) (string, error) {
	durationS := (end - start) / 1e9
	step := durationS / 100
	if step < 15 {
		step = 15
	}
	data, msg, err := c.get(ctx, base+"/loki/api/v1/query_range", map[string]string{
		"query": logql, "start": strconv.FormatInt(start, 10), "end": strconv.FormatInt(end, 10),
		"step": fmt.Sprintf("%ds", step),
	})
	if err != nil || msg != "" {
		return msg, err
	}
	if data.Data.ResultType == "streams" {
		return c.logLines(logql, data), nil
	}
	return c.renderMetric(logql, data), nil
}

func (c *Client) renderMetric(logql string, data *lokiResp) string {
	results, dropped := c.Blocked.DropSeries(data.Data.Result, "metric")
	if len(results) == 0 {
		if dropped > 0 {
			return nsguard.AllWithheldMessage(dropped)
		}
		return "No metric data for: " + logql
	}
	lines := []string{"LogQL (metric): " + logql, fmt.Sprintf("Series: %d", len(results))}
	if dropped > 0 {
		lines = append(lines, strings.TrimSpace(nsguard.WithheldNote(dropped, "")))
	}
	for i, r := range results {
		if i >= 20 {
			break
		}
		m, _ := r.(map[string]any)
		labels, _ := m["metric"].(map[string]any)
		var vals []float64
		values, _ := m["values"].([]any)
		for _, v := range values {
			pair, ok := v.([]any)
			if !ok || len(pair) < 2 || fmt.Sprint(pair[1]) == "NaN" {
				continue
			}
			if f, err := strconv.ParseFloat(fmt.Sprint(pair[1]), 64); err == nil {
				vals = append(vals, f)
			}
		}
		if len(vals) > 0 {
			lo, hi, sum := vals[0], vals[0], 0.0
			for _, x := range vals {
				if x < lo {
					lo = x
				}
				if x > hi {
					hi = x
				}
				sum += x
			}
			lines = append(lines, fmt.Sprintf("  [%s]  min=%.4f  avg=%.4f  max=%.4f", labelPairs(labels), lo, sum/float64(len(vals)), hi))
		}
	}
	return strings.Join(lines, "\n")
}
