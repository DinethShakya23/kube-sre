// Package spans builds OpenTelemetry GenAI spans over the flight recorder. Spans are
// emitted as additive decision_log rows (kind "ki_otel_span") through the existing
// record path, so they enter the same per episode hash chain and become tamper
// evident for free: no new store. Identity and attributes live in the hash covered
// payload.
package spans

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
)

const (
	Kind         = "ki_otel_span"
	OpChat       = "chat"
	OpTool       = "execute_tool"
	OpMCP        = "mcp_call"
	OpMutation   = "ki.mutation"
	OpHypothesis = "ki.hypothesis"
	OpEvidence   = "ki.evidence"
)

// TraceID is replay stable, derived from the episode id (no clock, no randomness).
func TraceID(episode string) string {
	sum := sha256.Sum256([]byte(episode))
	return hex.EncodeToString(sum[:])[:32]
}

// SpanID is deterministic from (episode, seq).
func SpanID(episode string, seq int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", episode, seq)))
	return hex.EncodeToString(sum[:])[:16]
}

func base(episode string, seq int, op string, parent string, attrs map[string]any) map[string]any {
	var p any
	if parent != "" {
		p = parent
	}
	return map[string]any{
		"trace_id": TraceID(episode), "span_id": SpanID(episode, seq), "parent_span_id": p,
		"gen_ai.operation.name": op, "attributes": attrs,
	}
}

// Chat is a GenAI chat span; its usage attributes are the single spend source.
func Chat(episode string, seq int, system, model string, in, out int, parent string) map[string]any {
	return base(episode, seq, OpChat, parent, map[string]any{
		"gen_ai.system": system, "gen_ai.request.model": model,
		"gen_ai.usage.input_tokens": in, "gen_ai.usage.output_tokens": out,
	})
}

// Tool is a tool call span, or an MCP call span.
func Tool(episode string, seq int, tool string, ok bool, parent string, mcp bool) map[string]any {
	op := OpTool
	if mcp {
		op = OpMCP
	}
	return base(episode, seq, op, parent, map[string]any{"gen_ai.tool.name": tool, "ki.tool.ok": ok})
}

// Mutation is a cluster mutating span. Provenance is enforced at build time: it must
// link at least one hypothesis and one evidence span, else it is marked
// ki.provenance_incomplete and logged. It fails loudly in the record and is never dropped.
func Mutation(episode string, seq int, action string, hypotheses, evidence []string, parent string) map[string]any {
	incomplete := len(hypotheses) == 0 || len(evidence) == 0
	if incomplete {
		slog.Warn("otel mutation span has incomplete provenance", "action", action, "hypotheses", len(hypotheses), "evidence", len(evidence))
	}
	return base(episode, seq, OpMutation, parent, map[string]any{
		"ki.action": action, "ki.links.hypothesis": nonNil(hypotheses), "ki.links.evidence": nonNil(evidence), "ki.provenance_incomplete": incomplete,
	})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Recorder is what Emit writes to.
type Recorder interface {
	Record(episode, kind string, payload map[string]any)
}

// Emit records a span, fire and forget. It is a no-op unless both CORTEX_V5_ENABLED
// and KI_V5_OTEL_SPANS_ENABLED are set, so with the flags off the recorder byte
// stream is identical to before.
func Emit(r Recorder, enabled bool, episode string, payload map[string]any) {
	if enabled && r != nil {
		r.Record(episode, Kind, payload)
	}
}

// Chain is one mutation with what it is linked to.
type Chain struct {
	MutationSpanID string           `json:"mutation_span_id"`
	Action         any              `json:"action"`
	Hypotheses     []map[string]any `json:"hypotheses"`
	Evidence       []map[string]any `json:"evidence"`
	Complete       bool             `json:"complete"`
}

func strList(v any) []string {
	var out []string
	switch xs := v.(type) {
	case []string:
		return xs
	case []any:
		for _, x := range xs {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// ProvenanceChains reconstructs hypothesis to evidence to mutation deterministically,
// with no model, from decoded span payloads in any order. Complete means both link
// lists are present and every link resolves to a span that is in the set.
func ProvenanceChains(rows []map[string]any) []Chain {
	byID := map[string]map[string]any{}
	for _, r := range rows {
		if id, ok := r["span_id"].(string); ok {
			byID[id] = r
		}
	}
	out := []Chain{}
	for _, r := range rows {
		if r["gen_ai.operation.name"] != OpMutation {
			continue
		}
		attrs, _ := r["attributes"].(map[string]any)
		hs, es := strList(attrs["ki.links.hypothesis"]), strList(attrs["ki.links.evidence"])
		c := Chain{MutationSpanID: fmt.Sprint(r["span_id"]), Action: attrs["ki.action"], Hypotheses: []map[string]any{}, Evidence: []map[string]any{}}
		resolved := true
		for _, h := range hs {
			if s, ok := byID[h]; ok {
				c.Hypotheses = append(c.Hypotheses, s)
			} else {
				resolved = false
			}
		}
		for _, e := range es {
			if s, ok := byID[e]; ok {
				c.Evidence = append(c.Evidence, s)
			} else {
				resolved = false
			}
		}
		c.Complete = len(hs) > 0 && len(es) > 0 && resolved
		out = append(out, c)
	}
	return out
}
