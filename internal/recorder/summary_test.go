package recorder

import (
	"strings"
	"testing"
)

func TestSummariseNeverReturnsBlank(t *testing.T) {
	kinds := []string{"finding", "tool_call", "tool_result", "rollback_point", "hitl_request", "answer", "final", GapKind, "error",
		"usage", "status", "plan", "plan_transition", SpanKind, "something_new", ""}
	for _, k := range kinds {
		for _, p := range []map[string]any{nil, {}, {"unrelated": 1}} {
			if strings.TrimSpace(Summarise(k, p)) == "" {
				t.Errorf("blank summary for kind %q payload %v", k, p)
			}
		}
	}
}

func TestSummariseFindingsCarryTheirContent(t *testing.T) {
	got := Summarise("finding", map[string]any{"playbook": "crashloop", "namespace": "shop", "object": "pod/web", "severity": "predicted", "eta_minutes": 12.0})
	if got != "detector crashloop fired on shop/pod/web (predicted ~12m)" {
		t.Errorf("%q", got)
	}
}

func TestSummariseRollbackStates(t *testing.T) {
	base := map[string]any{"rollback_id": "rb1", "command": "kubectl delete deploy web", "targets_captured": 3.0, "targets_intended": 7.0}
	if got := Summarise("rollback_point", base); !strings.Contains(got, "not recorded") || !strings.Contains(got, "[3/7 objects]") {
		t.Errorf("unknown: %q", got)
	}
	base["restorable"] = false
	if got := Summarise("rollback_point", base); !strings.Contains(got, "NOT restorable") {
		t.Errorf("not restorable: %q", got)
	}
	base["restorable"] = true
	if got := Summarise("rollback_point", base); strings.Contains(got, "NOT") || strings.Contains(got, "not recorded") {
		t.Errorf("restorable: %q", got)
	}
}

func TestSummariseGapSaysTheRecordIsIncomplete(t *testing.T) {
	got := Summarise(GapKind, map[string]any{"dropped": 4.0, "reason": "db down"})
	if !strings.Contains(got, "4 event(s) LOST") || !strings.Contains(got, "incomplete") {
		t.Errorf("%q", got)
	}
}

func TestSummarisePlanAndSpan(t *testing.T) {
	plan := map[string]any{"steps": []any{map[string]any{"status": "done"}, map[string]any{"status": "pending"}}}
	if got := Summarise("plan", plan); got != "plan — 2 step(s), 1 done" {
		t.Errorf("%q", got)
	}
	if got := Summarise(SpanKind, map[string]any{"gen_ai.operation.name": "chat", "attributes": map[string]any{"a": 1}, "span_id": "abc"}); !strings.Contains(got, "span chat (1 attribute(s)") {
		t.Errorf("%q", got)
	}
}
