package agent

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/DinethShakya23/kube-sre/internal/llm"
	"github.com/DinethShakya23/kube-sre/internal/policy"
)

// Keep the last N messages of session history so the prompt does not bloat. An
// exchange is about four messages (question, tool call, tool result, answer), so
// twenty is roughly five exchanges: enough context while capping growth.
const maxSessionMessages = 20

// compressDropped builds a compact, deterministic summary of dropped messages:
// what the user asked, what was run, and the first line of each result. It makes
// no model call, so it costs no latency.
func compressDropped(dropped []llm.Message) string {
	lines := []string{"## Earlier Session Context (compressed)"}
	for _, m := range dropped {
		switch m.Role {
		case llm.User:
			lines = append(lines, "- User: "+clip(oneLine(strings.TrimSpace(m.Content)), 120))
		case llm.Assistant:
			for _, tc := range m.ToolCalls {
				cmd := firstString(tc.Args, "command", "query", "logql")
				if cmd != "" {
					lines = append(lines, "- Ran: "+clip(cmd, 100))
				}
			}
			if len(m.ToolCalls) == 0 {
				if snippet := clip(oneLine(strings.TrimSpace(m.Content)), 120); snippet != "" {
					lines = append(lines, "- Assistant: "+snippet)
				}
			}
		case llm.Tool:
			if content := strings.TrimSpace(m.Content); content != "" {
				first := strings.SplitN(content, "\n", 2)[0]
				lines = append(lines, "  -> "+clip(first, 100))
			}
		}
	}
	return strings.Join(lines, "\n")
}

func firstString(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k]; ok && v != nil {
			if s := fmt.Sprint(v); s != "" {
				return s
			}
		}
	}
	return ""
}

func oneLine(s string) string { return strings.ReplaceAll(s, "\n", " ") }

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// trimSession caps history at maxSessionMessages while keeping exchanges whole:
// a plain tail slice can start with a tool message whose parent assistant message
// was cut off, which the provider rejects with a 400. It returns a summary of
// what was dropped, to go into the system prompt, or "" when nothing was.
func trimSession(messages []llm.Message) (kept []llm.Message, summary string) {
	if len(messages) <= maxSessionMessages {
		return messages, ""
	}
	keep := messages[len(messages)-maxSessionMessages:]
	first := 0
	for i, m := range keep {
		if m.Role == llm.User {
			first = i
			break
		}
	}
	keep = keep[first:]
	dropped := messages[:len(messages)-len(keep)]
	if len(dropped) > 0 {
		summary = compressDropped(dropped)
	}
	slog.Debug("coordinator compressed session history", "from", len(messages), "to", len(keep), "dropped", len(dropped))
	return keep, summary
}

// ── tool output trimming ─────────────────────────────────────────────────────

const (
	toolOutputMaxChars = 2000
	kubectlTableRows   = 30
	logLinesKept       = 60
)

var kubectlKeepRe = regexp.MustCompile(`(?i)error|warning|failed|pending|oomkilled|crashloop|backoff|imagepull|containercreating`)

// droppedNote says that rows were dropped here, in the vocabulary the prompt
// names. The marker once said "chars trimmed", matching neither string the
// system prompt tells the model to look for: an instruction and its trigger that
// did not agree.
func droppedNote(dropped int, noun string) string {
	if dropped <= 0 {
		return ""
	}
	return fmt.Sprintf("\n[truncated: %d %s(s) omitted from LLM context - this listing is NOT the complete set; "+
		"narrow the query (-n, -l, --tail) to see the rest]", dropped, noun)
}

// trimToolOutput shrinks tool output to fit the model's context and says what was
// taken out. The tool's own notices (a withheld namespace sentence, a refusal, a
// truncation marker) sit at the end of a listing, exactly where these caps cut, so
// they are lifted out first and put back last: losing one turns "3 namespaces
// were withheld" back into a listing that reads as complete.
func trimToolOutput(content string) string {
	if len([]rune(content)) <= toolOutputMaxChars {
		return content
	}
	var polLines, body []string
	for _, ln := range policy.SplitLines(content) {
		if policy.LineRE.MatchString(ln) {
			polLines = append(polLines, ln)
		} else {
			body = append(body, ln)
		}
	}
	polText := strings.TrimSpace(strings.Join(polLines, ""))

	var trimmed, note string
	if len(body) > 0 && strings.Contains(strings.ToUpper(body[0]), "NAME") {
		// A kubectl table: the header, the first rows, and any important rows.
		var kept strings.Builder
		rows, dropped := 0, 0
		for _, line := range body[1:] {
			if strings.TrimSpace(line) == "" {
				continue // a separator, not a row
			}
			switch {
			case kubectlKeepRe.MatchString(line):
				kept.WriteString(line)
			case rows < kubectlTableRows:
				kept.WriteString(line)
				rows++
			default:
				dropped++
			}
		}
		trimmed = body[0] + kept.String()
		note = droppedNote(dropped, "row")
	} else {
		// Logs, describe, prometheus, loki: the first N lines.
		n := len(body)
		if n > logLinesKept {
			n = logLinesKept
		}
		trimmed = strings.Join(body[:n], "")
		note = droppedNote(len(body)-logLinesKept, "line")
	}
	if r := []rune(trimmed); len(r) > toolOutputMaxChars {
		omitted := len(r) - toolOutputMaxChars
		trimmed = string(r[:toolOutputMaxChars])
		// Two different losses, two counts: "I see 30 of 200 rows" and "the last
		// row I see is cut in half" are not the same thing to narrow.
		note += fmt.Sprintf("\n[truncated: %d chars omitted from LLM context]", omitted)
	}
	out := trimmed + note
	if polText != "" {
		out += "\n" + polText
	}
	return out
}

// trimToolMessages caps tool message content before it is stored in state.
func trimToolMessages(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		if m.Role == llm.Tool {
			m.Content = trimToolOutput(m.Content)
		}
		out[i] = m
	}
	return out
}

// fillOrphanToolCalls adds a placeholder tool message for every tool call that
// never ran. When an approval pause fires mid batch, only the resumed call gets a
// result, and the model would re-propose the others on the next loop, causing
// redundant approval prompts. A "skipped" placeholder breaks that loop.
func fillOrphanToolCalls(msgs []llm.Message) []llm.Message {
	have := map[string]bool{}
	for _, m := range msgs {
		if m.Role == llm.Tool && m.ToolCallID != "" {
			have[m.ToolCallID] = true
		}
	}
	out := append([]llm.Message(nil), msgs...)
	for _, m := range msgs {
		if m.Role != llm.Assistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && !have[tc.ID] {
				out = append(out, llm.Message{Role: llm.Tool, ToolCallID: tc.ID, Name: tc.Name,
					Content: "Skipped - pending approval; will be re-proposed in a separate response."})
				have[tc.ID] = true
			}
		}
	}
	return out
}

// ── investigation plan ───────────────────────────────────────────────────────

const planMinSteps = 3

var planBlockRe = regexp.MustCompile(`(?m)^\s*INVESTIGATION_PLAN:\s*\n[ \t]*\n?((?:[ \t]*(?:[-•*]|\d+\.?)\s+.+\n?)+)`)
var planStepRe = regexp.MustCompile(`(?m)^[ \t]*(?:[-•*]|\d+\.?)\s+(.+)$`)

// extractPlan strips an INVESTIGATION_PLAN block from the first assistant message
// that has one and returns the steps with the cleaned messages. No block, or one
// with fewer than planMinSteps steps, returns no plan and the messages unchanged.
func extractPlan(msgs []llm.Message) ([]PlanStep, []llm.Message) {
	if len(msgs) == 0 {
		return nil, msgs
	}
	cleaned := make([]llm.Message, 0, len(msgs))
	var plan []PlanStep
	consumed := false
	for _, m := range msgs {
		if !consumed && m.Role == llm.Assistant {
			if loc := planBlockRe.FindStringSubmatchIndex(m.Content); loc != nil {
				stepsText := m.Content[loc[2]:loc[3]]
				var lines []string
				for _, sm := range planStepRe.FindAllStringSubmatch(stepsText, -1) {
					if s := strings.TrimSpace(sm[1]); s != "" {
						lines = append(lines, s)
					}
				}
				if len(lines) >= planMinSteps {
					for _, l := range lines {
						plan = append(plan, PlanStep{Description: l, Status: "pending"})
					}
					m.Content = strings.TrimSpace(m.Content[:loc[0]] + m.Content[loc[1]:])
					consumed = true
				}
			}
		}
		cleaned = append(cleaned, m)
	}
	return plan, cleaned
}

// annotatePlan marks the first N steps done and the rest skipped, where N is how
// many tool calls actually ran.
func annotatePlan(plan []PlanStep, msgs []llm.Message) {
	calls := 0
	for _, m := range msgs {
		if m.Role == llm.Assistant {
			calls += len(m.ToolCalls)
		}
	}
	for i := range plan {
		if i < calls {
			plan[i].Status = "done"
		} else {
			plan[i].Status = "skipped"
		}
	}
}
