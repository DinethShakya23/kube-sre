package spans

import (
	"testing"
)

type rec struct {
	episode, kind string
	payload       map[string]any
	n             int
}

func (r *rec) Record(e, k string, p map[string]any) {
	r.episode, r.kind, r.payload, r.n = e, k, p, r.n+1
}

func TestIdsAreReplayStable(t *testing.T) {
	if TraceID("ep1") != TraceID("ep1") || TraceID("ep1") == TraceID("ep2") || len(TraceID("ep1")) != 32 {
		t.Error("trace id")
	}
	if SpanID("ep1", 3) != SpanID("ep1", 3) || SpanID("ep1", 3) == SpanID("ep1", 4) || len(SpanID("ep1", 3)) != 16 {
		t.Error("span id")
	}
}

func TestBuilders(t *testing.T) {
	c := Chat("ep", 1, "openai", "gpt-x", 100, 20, "")
	a := c["attributes"].(map[string]any)
	if c["gen_ai.operation.name"] != OpChat || c["parent_span_id"] != nil || a["gen_ai.usage.input_tokens"] != 100 || a["gen_ai.request.model"] != "gpt-x" {
		t.Errorf("%v", c)
	}
	tl := Tool("ep", 2, "kubectl", true, SpanID("ep", 1), false)
	if tl["gen_ai.operation.name"] != OpTool || tl["parent_span_id"] != SpanID("ep", 1) || tl["attributes"].(map[string]any)["ki.tool.ok"] != true {
		t.Errorf("%v", tl)
	}
	if Tool("ep", 3, "srv", false, "", true)["gen_ai.operation.name"] != OpMCP {
		t.Error("mcp")
	}
}

func TestMutationProvenanceIsFlaggedNotDropped(t *testing.T) {
	ok := Mutation("ep", 5, "scale web", []string{"h"}, []string{"e"}, "")
	if ok["attributes"].(map[string]any)["ki.provenance_incomplete"] != false {
		t.Errorf("%v", ok)
	}
	for _, m := range []map[string]any{Mutation("ep", 6, "x", nil, []string{"e"}, ""), Mutation("ep", 7, "x", []string{"h"}, nil, ""), Mutation("ep", 8, "x", nil, nil, "")} {
		if m["attributes"].(map[string]any)["ki.provenance_incomplete"] != true {
			t.Errorf("incomplete provenance must be marked: %v", m)
		}
	}
}

func TestEmitOnlyWhenEnabled(t *testing.T) {
	r := &rec{}
	Emit(r, false, "ep", map[string]any{"a": 1})
	if r.n != 0 {
		t.Error("flags off: identical recorder stream")
	}
	Emit(r, true, "ep", map[string]any{"a": 1})
	if r.n != 1 || r.kind != Kind || r.episode != "ep" {
		t.Errorf("%+v", r)
	}
	Emit(nil, true, "ep", nil) // must not panic
}

func TestProvenanceReconstruction(t *testing.T) {
	h := base("ep", 1, OpHypothesis, "", map[string]any{"h": 1})
	e := base("ep", 2, OpEvidence, "", map[string]any{"e": 1})
	complete := Mutation("ep", 3, "scale", []string{h["span_id"].(string)}, []string{e["span_id"].(string)}, "")
	dangling := Mutation("ep", 4, "delete", []string{h["span_id"].(string)}, []string{"missing-span"}, "")
	empty := Mutation("ep", 5, "patch", nil, nil, "")
	chains := ProvenanceChains([]map[string]any{complete, dangling, h, empty, e}) // any order

	if len(chains) != 3 {
		t.Fatalf("%+v", chains)
	}
	byAction := map[string]Chain{}
	for _, c := range chains {
		byAction[c.Action.(string)] = c
	}
	if c := byAction["scale"]; !c.Complete || len(c.Hypotheses) != 1 || len(c.Evidence) != 1 {
		t.Errorf("%+v", c)
	}
	if c := byAction["delete"]; c.Complete || len(c.Evidence) != 0 {
		t.Errorf("a link to a span that is not there is not provenance: %+v", c)
	}
	if c := byAction["patch"]; c.Complete {
		t.Errorf("%+v", c)
	}
	if got := ProvenanceChains(nil); got == nil || len(got) != 0 {
		t.Errorf("%v", got)
	}
}
