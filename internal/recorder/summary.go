package recorder

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SpanKind is the row kind of a recorded trace span.
const SpanKind = "ki_otel_span"

func str(p map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := p[k]; ok && v != nil {
			if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
				return s
			}
		}
	}
	return ""
}

// Float reads a JSON number however the payload was decoded.
func Float(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func num(p map[string]any, k string, def any) any {
	if v, ok := p[k]; ok && v != nil {
		return v
	}
	return def
}

// Summarise describes one decision_log row in a single line, and never returns an
// empty one. Every reader of the record (the postmortem, the replay client) uses
// this, so a new kind is handled once or not at all. A blank summary cell reads as
// "this event carried nothing", which is the one thing a tamper evident log must
// never say falsely: a finding row shares no field with a naive list of top level
// keys, and a findings episode contains nothing else.
func Summarise(kind string, p map[string]any) string {
	if p == nil {
		p = map[string]any{}
	}
	switch kind {
	case "finding":
		sev := str(p, "severity")
		if sev == "" {
			sev = "warning"
		}
		tag := ""
		if eta, ok := Float(p["eta_minutes"]); ok && sev == "predicted" {
			tag = fmt.Sprintf(" (predicted ~%.0fm)", eta)
		}
		pb := str(p, "playbook")
		if pb == "" {
			pb = "?"
		}
		return fmt.Sprintf("detector %s fired on %s/%s%s", pb, str(p, "namespace"), str(p, "object"), tag)
	case "tool_call":
		t := str(p, "tool")
		if t == "" {
			t = "?"
		}
		return "tool " + t + ": " + clip(str(p, "command"), 100)
	case "tool_result":
		return "result: " + clip(str(p, "summary", "output"), 100)
	case "rollback_point":
		scope := ""
		got, ok1 := Float(p["targets_captured"])
		want, ok2 := Float(p["targets_intended"])
		if ok1 && ok2 {
			scope = fmt.Sprintf(" [%d/%d objects]", int(got), int(want))
		}
		mark := " (restorability not recorded)" + scope
		if r, ok := p["restorable"].(bool); ok {
			if r {
				mark = scope
			} else {
				mark = " ⚠️ NOT restorable" + scope
			}
		}
		return fmt.Sprintf("rollback point %s%s before: %s", str(p, "rollback_id"), mark, clip(str(p, "command"), 80))
	case "hitl_request":
		return "approval requested: " + clip(str(p, "command", "message"), 80)
	case "answer", "final":
		if t := str(p, "text", "answer", "final_text"); t != "" {
			return clip(t, 200)
		}
		return "investigation concluded"
	case GapKind:
		reason := str(p, "reason")
		if reason == "" {
			reason = "unknown cause"
		}
		return fmt.Sprintf("⚠️ %v event(s) LOST here — %s. The record below this point is incomplete.", num(p, "dropped", "?"), reason)
	case "error":
		return "error: " + clip(str(p, "error"), 160)
	case "usage":
		// Calls are named next to tokens so "called 40 times, reported no tokens", an
		// instrumentation gap, stays legible beside a genuinely cheap request.
		return fmt.Sprintf("%v token(s) over %v LLM call(s) (%v in / %v out)", num(p, "total_tokens", 0), num(p, "llm_calls", 0),
			num(p, "prompt_tokens", 0), num(p, "completion_tokens", 0))
	case "status":
		if s := str(p, "message", "status"); s != "" {
			return clip(s, 120)
		}
		return "status update"
	case "plan", "plan_transition":
		if steps, ok := p["steps"].([]any); ok && len(steps) > 0 {
			done := 0
			for _, s := range steps {
				if m, ok := s.(map[string]any); ok && m["status"] == "done" {
					done++
				}
			}
			return fmt.Sprintf("plan — %d step(s), %d done", len(steps), done)
		}
		return "plan: " + clip(str(p, "summary", "step"), 100)
	case SpanKind:
		count := 0
		if a, ok := p["attributes"].(map[string]any); ok {
			count = len(a)
		}
		op := str(p, "gen_ai.operation.name")
		if op == "" {
			op = "?"
		}
		id := str(p, "span_id")
		if id == "" {
			id = "?"
		}
		return fmt.Sprintf("span %s (%d attribute(s), span_id=%s)", op, count, clip(id, 16))
	}
	if kind == "" {
		return "record"
	}
	return kind
}
