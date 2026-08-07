// Package nsguard is the namespace blocklist for tools that reach the cluster
// indirectly: logs (Loki) and metrics (Prometheus), where a namespace is a label
// matcher rather than a -n flag.
//
// Two gates, because a query language cannot be guarded by reading the query:
//
//  1. Input: a query that names a blocked namespace in a positive matcher is
//     refused, so the user gets the same clear [Protected] answer kubectl gives.
//  2. Output: every returned stream or series is dropped if its own namespace
//     label is blocked. This is the load bearing gate: it works on ground truth
//     from the datasource, so it also catches {app="nginx"} matching a pod that
//     runs in kube-system, and any regex matcher the input gate could not decide.
//
// Known residual: a result with no namespace label passes. Node and cluster
// metrics have none, and dropping them would break monitoring, so an aggregation
// that discards the label can still return a number computed over a blocked
// namespace. Closing that would mean refusing aggregate queries; it is a
// deliberate trade.
package nsguard

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/DinethShakya23/kube-sre/internal/policy"
)

// Blocklist is a set of lowercase namespace names.
type Blocklist map[string]bool

var matcherRe = regexp.MustCompile(`\bnamespace\s*(=~|!~|!=|=)\s*"([^"]*)"`)

// InQuery returns the blocked namespace a query positively selects, or "".
// Only positive matchers count: namespace!="kube-system" asks to exclude it. A
// regex matcher is refused when it fully matches a blocked namespace, which is
// best effort since an arbitrary regex cannot be decided here.
func (b Blocklist) InQuery(query string) string {
	names := make([]string, 0, len(b))
	for ns := range b {
		names = append(names, ns)
	}
	sort.Strings(names)
	for _, m := range matcherRe.FindAllStringSubmatch(query, -1) {
		op, value := m[1], m[2]
		switch op {
		case "=":
			if b[strings.ToLower(value)] {
				return strings.ToLower(value)
			}
		case "=~":
			re, err := regexp.Compile("^(?:" + value + ")$")
			if err != nil {
				continue
			}
			for _, ns := range names {
				if re.MatchString(ns) {
					return ns
				}
			}
		}
	}
	return ""
}

var labelContainers = []string{"stream", "metric", "labels"}

// SeriesLabels is the label set of one result, wherever the datasource put it.
// The guard must not depend on a guess about the query: Loki uses "stream" for
// logs and "metric" for metrics, and looking in each container makes the filter
// independent of how the request was classified. A scalar or string result is a
// bare pair with no labels, so there is nothing to protect.
func SeriesLabels(item any, hint string) map[string]any {
	m, ok := item.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	keys := labelContainers
	if hint != "" {
		keys = append([]string{hint}, labelContainers...)
	}
	for _, k := range keys {
		if v, ok := m[k].(map[string]any); ok && len(v) > 0 {
			return v
		}
	}
	return map[string]any{}
}

// DropSeries splits results into (allowed, number dropped) by namespace label.
// hint says where this datasource usually keeps labels; every known container
// is consulted regardless, so a wrong hint cannot switch the guard off.
func (b Blocklist) DropSeries(results []any, hint string) ([]any, int) {
	allowed := make([]any, 0, len(results))
	for _, r := range results {
		ns := strings.ToLower(fmt.Sprint(SeriesLabels(r, hint)["namespace"]))
		if ns == "<nil>" {
			ns = ""
		}
		if !b[ns] {
			allowed = append(allowed, r)
		}
	}
	return allowed, len(results) - len(allowed)
}

// DropTableRows drops rows of a cluster wide kubectl table whose first column is
// a blocked namespace. It has two callers: the tool, on the output of a call,
// and the context fetcher, on the snapshot pasted into every prompt.
func (b Blocklist) DropTableRows(output string) (string, int) {
	var kept strings.Builder
	dropped := 0
	for _, ln := range policy.SplitLines(output) {
		f := strings.Fields(ln)
		if len(f) > 0 && b[strings.ToLower(f[0])] {
			dropped++
			continue
		}
		kept.WriteString(ln)
	}
	return kept.String(), dropped
}

func ProtectedMessage(namespace string) string {
	return fmt.Sprintf("[Protected] Access to namespace '%s' is not permitted. "+
		"This applies to logs and metrics exactly as it applies to kubectl.", namespace)
}

// AllWithheldMessage is for when every result was dropped: say so, rather than
// naming a namespace we never selected.
func AllWithheldMessage(dropped int) string {
	return fmt.Sprintf("[Protected] All %d result(s) belong to a namespace in "+
		"KUBECTL_BLOCKED_NAMESPACES and were withheld. Nothing else matched.", dropped)
}

// WithheldKey is where the notice goes in a json or yaml payload. A sentence
// appended after the document is not JSON, so structured output carries the
// notice as a field and only text formats get a trailing line.
const WithheldKey = "withheldByPolicy"

func WithheldSentence(dropped int, noun string) string {
	if noun == "" {
		noun = "result"
	}
	return fmt.Sprintf("[Protected] %d %s(s) withheld: they belong to a namespace in "+
		"KUBECTL_BLOCKED_NAMESPACES. This listing is NOT the complete set.", dropped, noun)
}

// Annotate records the withholding inside a structured document, keeping it parseable.
func Annotate(doc map[string]any, dropped int, noun string) map[string]any {
	if dropped > 0 {
		doc[WithheldKey] = WithheldSentence(dropped, noun)
	}
	return doc
}

// WithheldNote is the one sentence every filter that removes rows must append.
// A filtered listing and a complete one are the same bytes, so without it an
// agent asked "does the monitoring namespace exist?" gets a short list it has no
// way to know is short, and answers no. noun names what was removed, since
// "3 result(s)" reads as three pods when the caller asked for namespaces.
func WithheldNote(dropped int, noun string) string {
	return "\n" + WithheldSentence(dropped, noun)
}
