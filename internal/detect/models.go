package detect

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/sensorium"
)

// WatchPredicate is one machine-checkable condition over the observation stream.
//
//	Pod   - StatusRegex against the computed STATUS column.
//	Event - Reason/Message regexes against Warning events; InvolvedKind narrows.
//	Node  - StatusRegex against the node condition summary.
type WatchPredicate struct {
	Kind         string
	StatusRegex  *regexp.Regexp
	ReasonRegex  *regexp.Regexp
	MessageRegex *regexp.Regexp
	InvolvedKind string
}

func (p WatchPredicate) Matches(o sensorium.Observation) bool {
	switch {
	case p.Kind == "Pod" && o.Kind == "pod_status", p.Kind == "Node" && o.Kind == "node_status":
		return p.StatusRegex != nil && p.StatusRegex.MatchString(o.Str("status"))
	case p.Kind == "Event" && o.Kind == "event":
		if t := o.Str("event_type"); t != "" && t != "Warning" {
			return false
		}
		if p.InvolvedKind != "" && o.Str("involved_kind") != p.InvolvedKind {
			return false
		}
		// When both are set, both must match; message narrows broad reasons.
		reasonOK := p.ReasonRegex == nil || p.ReasonRegex.MatchString(o.Str("reason"))
		messageOK := p.MessageRegex == nil || p.MessageRegex.MatchString(o.Str("message"))
		return reasonOK && messageOK
	}
	return false
}

// TrendPredicate projects a metric toward a threshold with a least-squares fit.
// It fires a "predicted" finding when the crossing ETA is close enough and the
// fit is clean (r2 >= MinR2).
type TrendPredicate struct {
	Metric                 string
	Threshold              float64
	WindowMinutes          int
	ProjectionHorizonMin   int
	FireIfETAWithinMinutes int
	Direction              string // rising | falling
	MinR2                  float64
	ObjectLabel            string
}

// DetectBlock is the compiled `detect:` block of one playbook.
type DetectBlock struct {
	Playbook          string
	WatchPredicates   []WatchPredicate
	PromQL            []string
	DebounceSeconds   int
	TrendPredicates   []TrendPredicate
	DroppedPredicates []string
	FiresOnHealthy    []string
}

// Finding is a detector firing. It costs no LLM tokens.
type Finding struct {
	ID         string
	Playbook   string
	ClusterID  string
	Namespace  string
	Object     string
	Evidence   string
	FirstSeen  float64
	FiredAt    float64
	Source     string // watch | trend | shadow
	Severity   string // warning | predicted
	ETAMinutes *float64
}

func (f Finding) Dict() map[string]any {
	return map[string]any{
		"type": "finding", "id": f.ID, "playbook": f.Playbook, "cluster_id": f.ClusterID,
		"namespace": f.Namespace, "object": f.Object, "evidence": f.Evidence,
		"first_seen": f.FirstSeen, "fired_at": f.FiredAt, "source": f.Source,
		"severity": f.Severity, "eta_minutes": f.ETAMinutes,
	}
}

func newFindingID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "fnd-" + hex.EncodeToString(b)
}

func secs(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// ParseBlock compiles a playbook's `detect:` mapping. A nil result with no
// error means the playbook has nothing compiled (LLM-only).
//
// promql is declarative only: nothing evaluates it, so on its own it must not
// make a block valid, or the detector would load and never fire.
func ParseBlock(playbook string, raw map[string]any) (*DetectBlock, error) {
	if raw == nil {
		return nil, nil
	}
	var preds []WatchPredicate
	for _, item := range asList(raw["watch_predicates"]) {
		e, ok := item.(map[string]any)
		if !ok || str(e["kind"]) == "" {
			continue
		}
		p := WatchPredicate{Kind: str(e["kind"]), InvolvedKind: str(e["involved_kind"])}
		var err error
		if p.StatusRegex, err = compile(e["status_regex"]); err != nil {
			return nil, err
		}
		if p.ReasonRegex, err = compile(e["reason_regex"]); err != nil {
			return nil, err
		}
		if p.MessageRegex, err = compile(e["message_regex"]); err != nil {
			return nil, err
		}
		preds = append(preds, p)
	}
	var promql []string
	for _, q := range asList(raw["promql"]) {
		promql = append(promql, str(q))
	}

	var trends []TrendPredicate
	for _, item := range asList(raw["trend_predicates"]) {
		e, ok := item.(map[string]any)
		if !ok || str(e["metric"]) == "" {
			continue
		}
		th, present := e["threshold"]
		if !present {
			continue
		}
		threshold, ok := toFloat(th)
		if !ok {
			return nil, fmt.Errorf("detector %q: threshold %v is not a number", playbook, th)
		}
		dir := str(e["direction"])
		if dir == "" {
			dir = "rising"
		}
		trends = append(trends, TrendPredicate{
			Metric:    str(e["metric"]),
			Threshold: threshold,
			WindowMinutes: knobInt(playbook, e, "window_minutes", 30, positive,
				"a window of zero minutes or less collects no samples to fit"),
			ProjectionHorizonMin: knobInt(playbook, e, "projection_horizon_minutes", 120, positive,
				"no projected ETA can fall inside a horizon of zero"),
			FireIfETAWithinMinutes: knobInt(playbook, e, "fire_if_eta_within_minutes", 30, positive,
				"an ETA of zero or less is dropped, so this could never fire"),
			Direction: dir,
			// 0 is a real setting: fire regardless of fit quality.
			MinR2:       knobFloat(playbook, e, "min_r2", 0.5, 0, 1),
			ObjectLabel: str(e["object_label"]),
		})
	}
	if len(preds) == 0 && len(trends) == 0 {
		if len(promql) > 0 {
			slog.Warn("detector declares only promql, which is not evaluated, so it can never fire",
				"playbook", playbook)
		}
		return nil, nil
	}
	return &DetectBlock{
		Playbook: playbook, WatchPredicates: preds, PromQL: promql, TrendPredicates: trends,
		// A negative debounce would silently mean no debounce at all.
		DebounceSeconds: knobInt(playbook, raw, "debounce_seconds", 0, func(v int) bool { return v >= 0 },
			"a negative debounce disables debouncing rather than shortening it"),
	}, nil
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func str(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	}
	return fmt.Sprint(v)
}

func compile(v any) (*regexp.Regexp, error) {
	s := str(v)
	if s == "" {
		return nil, nil
	}
	return regexp.Compile(s)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case string:
		i, err := strconv.Atoi(n)
		return i, err == nil
	}
	return 0, false
}

func positive(v int) bool { return v > 0 }

// knobInt tells "absent" from "set to zero". Zero is honoured where it means
// something and refused out loud where it would be a silent no-op.
func knobInt(name string, e map[string]any, key string, def int, valid func(int) bool, why string) int {
	raw, ok := e[key]
	if !ok || raw == nil {
		return def
	}
	v, ok := toInt(raw)
	if !ok {
		slog.Warn("detector setting is not a number, using the default", "detector", name, "key", key, "value", raw, "default", def)
		return def
	}
	if !valid(v) {
		slog.Warn("detector setting is invalid, using the default; the loaded detector is not the one written",
			"detector", name, "key", key, "value", v, "why", why, "default", def)
		return def
	}
	return v
}

func knobFloat(name string, e map[string]any, key string, def, lo, hi float64) float64 {
	raw, ok := e[key]
	if !ok || raw == nil {
		return def
	}
	v, ok := toFloat(raw)
	if !ok {
		slog.Warn("detector setting is not a number, using the default", "detector", name, "key", key, "value", raw, "default", def)
		return def
	}
	if v < lo || v > hi {
		slog.Warn("detector setting is out of range, using the default", "detector", name, "key", key, "value", v, "default", def)
		return def
	}
	return v
}
