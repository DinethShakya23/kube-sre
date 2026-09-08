// Package prom is query_prometheus: instant and range PromQL queries.
//
// range_minutes = 0 runs an instant query; above 0 it runs a range query and
// returns min, avg and max per series over that window.
package prom

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/detect"
	"github.com/DinethShakya23/kube-sre/internal/nsguard"
	"github.com/DinethShakya23/kube-sre/internal/policy"
)

const (
	outputCap        = 6000
	instantSeriesCap = 50
	rangeSeriesCap   = 20
	requestTimeout   = 15 * time.Second
)

type Client struct {
	URL     string
	Blocked nsguard.Blocklist
	HTTP    *http.Client
}

func New(url string, blocked nsguard.Blocklist) *Client {
	return &Client{URL: url, Blocked: blocked, HTTP: &http.Client{Timeout: requestTimeout}}
}

// BaseURL normalises the configured URL to a scheme qualified base, or "" if unset.
func (c *Client) BaseURL() string {
	u := strings.TrimSpace(c.URL)
	if u == "" {
		return ""
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
		slog.Warn("PROMETHEUS_URL is missing a protocol", "using", u)
	}
	return strings.TrimRight(u, "/")
}

func autoStep(rangeMinutes int) string {
	perPoint := (rangeMinutes * 60) / 100
	if perPoint < 15 {
		perPoint = 15
	}
	if perPoint < 60 {
		return fmt.Sprintf("%ds", perPoint)
	}
	return fmt.Sprintf("%dm", perPoint/60)
}

// typed is one Prometheus answer. Prometheus says what shape it returned and
// nothing here may guess it: an instant query answers with a vector, a matrix
// when the expression carries a range selector, and a scalar or string (a bare
// [timestamp, "value"] pair) for time() or a string literal.
type typed struct {
	resultType string
	result     any
}

// queryTyped never panics; errors come back as the second value, already worded
// for the model, with the unavailable marker where a retry cannot help.
func (c *Client) queryTyped(ctx context.Context, promql string, rangeMinutes int) (typed, string) {
	base := c.BaseURL()
	if base == "" {
		return typed{}, policy.MarkUnavailable(
			"Prometheus is not configured. Set PROMETHEUS_URL in ~/.kube-sre/.env and restart.",
			"Prometheus is not configured.")
	}
	slog.Debug("query_prometheus", "promql", promql, "range_minutes", rangeMinutes)
	var reqURL string
	params := map[string]string{"query": promql}
	if rangeMinutes <= 0 {
		reqURL = base + "/api/v1/query"
	} else {
		now := time.Now().Unix()
		reqURL = base + "/api/v1/query_range"
		params["start"] = strconv.FormatInt(now-int64(rangeMinutes)*60, 10)
		params["end"] = strconv.FormatInt(now, 10)
		params["step"] = autoStep(rangeMinutes)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return typed{}, "Prometheus error: " + err.Error()
	}
	q := req.URL.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	req.URL.RawQuery = q.Encode()
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if isTimeout(err) {
			return typed{}, "Prometheus query timed out (15s)."
		}
		if isConnect(err) {
			return typed{}, policy.MarkUnavailable(fmt.Sprintf("Cannot reach Prometheus at %s. "+
				"Is kube-prometheus-stack deployed? (make install-prometheus-kind)", base),
				"Prometheus cannot be reached.")
		}
		return typed{}, "Prometheus error: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return typed{}, fmt.Sprintf("Prometheus HTTP %d: %s", resp.StatusCode, clip(string(body), 500))
	}
	var doc struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     any    `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return typed{}, "Prometheus error: " + err.Error()
	}
	if doc.Status != "success" {
		e := doc.Error
		if e == "" {
			e = "unknown"
		}
		return typed{}, "Prometheus error: " + e
	}
	return typed{doc.Data.ResultType, doc.Data.Result}, ""
}

func isTimeout(err error) bool {
	type timeout interface{ Timeout() bool }
	for e := err; e != nil; {
		if t, ok := e.(timeout); ok && t.Timeout() {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

func isConnect(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "connection refused") || strings.Contains(s, "no such host") ||
		strings.Contains(s, "no route to host") || strings.Contains(s, "network is unreachable") ||
		strings.Contains(s, "dial tcp")
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// QuerySeries returns the result series and an error, for deterministic (non
// LLM) consumers such as the detector engine. "No data" and "I could not ask"
// are different answers, and this is where a detector tells them apart. A scalar
// or string answer is an error, not an empty list: these callers feed verdicts.
// The namespace filter is deliberately not applied: the caller's PromQL comes
// from human reviewed playbooks and is supposed to watch protected namespaces.
func (c *Client) QuerySeries(ctx context.Context, promql string, rangeMinutes int) ([]detect.Series, error) {
	t, msg := c.queryTyped(ctx, promql, rangeMinutes)
	if msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	list, ok := t.result.([]any)
	if !ok {
		return nil, fmt.Errorf("Prometheus returned a '%s' result, which carries no labelled series. "+
			"Wrap the expression so it yields a vector or matrix.", orUnknown(t.resultType))
	}
	var out []detect.Series
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Prometheus returned a '%s' result, which carries no labelled series. "+
				"Wrap the expression so it yields a vector or matrix.", orUnknown(t.resultType))
		}
		s := detect.Series{Metric: map[string]string{}}
		if lm, ok := m["metric"].(map[string]any); ok {
			for k, v := range lm {
				s.Metric[k] = fmt.Sprint(v)
			}
		}
		// An instant query answers with one value and a range query with many.
		if pair, ok := m["value"].([]any); ok && len(pair) >= 2 {
			ts, ok1 := pair[0].(float64)
			val, err := strconv.ParseFloat(fmt.Sprint(pair[1]), 64)
			if ok1 && err == nil {
				s.Values = append(s.Values, [2]float64{ts, val})
			}
		}
		if vals, ok := m["values"].([]any); ok {
			for _, v := range vals {
				pair, ok := v.([]any)
				if !ok || len(pair) < 2 {
					continue
				}
				ts, ok1 := pair[0].(float64)
				val, err := strconv.ParseFloat(fmt.Sprint(pair[1]), 64)
				if !ok1 || err != nil {
					continue
				}
				s.Values = append(s.Values, [2]float64{ts, val})
			}
		}
		out = append(out, s)
	}
	return out, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func labelString(m any) string {
	lm, _ := m.(map[string]any)
	keys := make([]string, 0, len(lm))
	for k := range lm {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%v", k, lm[k])
	}
	return strings.Join(parts, ", ")
}

func fmtInstant(query string, results []any) string {
	lines := []string{fmt.Sprintf("PromQL (instant): %s", query), fmt.Sprintf("Series: %d", len(results))}
	for i, r := range results {
		if i >= instantSeriesCap {
			break
		}
		m := r.(map[string]any)
		value := any("N/A")
		if pair, ok := m["value"].([]any); ok && len(pair) > 1 {
			value = pair[1]
		}
		lines = append(lines, fmt.Sprintf("  [%s] = %v", labelString(m["metric"]), value))
	}
	if len(results) > instantSeriesCap {
		lines = append(lines, fmt.Sprintf("  ... %d more series (narrow the query)", len(results)-instantSeriesCap))
	}
	return strings.Join(lines, "\n")
}

func numericValues(m map[string]any, skip ...string) []float64 {
	var out []float64
	vals, _ := m["values"].([]any)
	for _, v := range vals {
		pair, ok := v.([]any)
		if !ok || len(pair) < 2 {
			continue
		}
		s := fmt.Sprint(pair[1])
		skipIt := false
		for _, k := range skip {
			if s == k {
				skipIt = true
			}
		}
		if skipIt {
			continue
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			out = append(out, f)
		}
	}
	return out
}

func minAvgMax(v []float64) (lo, avg, hi float64) {
	lo, hi = v[0], v[0]
	sum := 0.0
	for _, x := range v {
		if x < lo {
			lo = x
		}
		if x > hi {
			hi = x
		}
		sum += x
	}
	return lo, sum / float64(len(v)), hi
}

func fmtRange(query, span string, results []any) string {
	lines := []string{fmt.Sprintf("PromQL (%s): %s", span, query), fmt.Sprintf("Series: %d", len(results))}
	for i, r := range results {
		if i >= rangeSeriesCap {
			break
		}
		m := r.(map[string]any)
		labels := labelString(m["metric"])
		vals := numericValues(m, "NaN", "+Inf", "-Inf")
		if len(vals) > 0 {
			lo, avg, hi := minAvgMax(vals)
			lines = append(lines, fmt.Sprintf("  [%s]  min=%.4f  avg=%.4f  max=%.4f", labels, lo, avg, hi))
		} else {
			lines = append(lines, fmt.Sprintf("  [%s] no numeric values", labels))
		}
	}
	if len(results) > rangeSeriesCap {
		lines = append(lines, fmt.Sprintf("  ... %d more series", len(results)-rangeSeriesCap))
	}
	return strings.Join(lines, "\n")
}

// fmtScalar renders a scalar or string result, a bare [timestamp, "value"] pair.
func fmtScalar(query, resultType string, raw any) string {
	if pair, ok := raw.([]any); ok && len(pair) == 2 {
		return fmt.Sprintf("PromQL (%s): %s\n  = %v", resultType, query, pair[1])
	}
	return fmt.Sprintf("PromQL (%s): %s\n  (unreadable %s result)", resultType, query, resultType)
}

// Query is the tool the model calls.
func (c *Client) Query(ctx context.Context, promql string, rangeMinutes int) string {
	if ns := c.Blocked.InQuery(promql); ns != "" {
		slog.Warn("query_prometheus refused, query selects a blocked namespace", "namespace", ns)
		return nsguard.ProtectedMessage(ns)
	}
	t, msg := c.queryTyped(ctx, promql, rangeMinutes)
	if msg != "" {
		return msg
	}
	// A scalar or string carries no labels, so there is nothing for the namespace
	// filter to read, and handing it to the filter is what once raised a type error.
	if t.resultType == "scalar" || t.resultType == "string" {
		return fmtScalar(promql, t.resultType, t.result)
	}
	var raw []any
	if list, ok := t.result.([]any); ok {
		for _, r := range list {
			if _, isMap := r.(map[string]any); isMap {
				raw = append(raw, r)
			}
		}
	}
	results, dropped := c.Blocked.DropSeries(raw, "metric")
	if len(results) == 0 {
		if dropped > 0 {
			return nsguard.AllWithheldMessage(dropped)
		}
		return "No data for query: " + promql
	}
	var out string
	// A matrix from an instant query means the expression carried a range selector.
	if t.resultType == "matrix" {
		span := "range selector"
		if rangeMinutes > 0 {
			span = fmt.Sprintf("range %dm", rangeMinutes)
		}
		out = fmtRange(promql, span, results)
	} else {
		out = fmtInstant(promql, results)
	}
	if dropped > 0 {
		out += nsguard.WithheldNote(dropped, "")
	}
	if r := []rune(out); len(r) > outputCap {
		out = string(r[:outputCap]) + "\n... [truncated - use a more specific query]"
	}
	return out
}
