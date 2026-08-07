// Package policy holds the lines in a tool result that are about the result
// rather than part of it.
//
// Two shapes, both written by the tools themselves: a [Protected] refusal or
// withheld-namespace sentence, and a truncation marker a tool appended after
// capping its own output. Both sit at the end of a listing, which is exactly
// where every downstream bound cuts, and neither looks like an important row to
// a keep pattern. So every layer between a tool and a model must carry them
// across its own trim, or the model reads a short listing as a complete one.
package policy

import (
	"fmt"
	"regexp"
	"strings"
)

// LineRE matches a line that says something about the result.
var LineRE = regexp.MustCompile(`(?i)\[Protected\]|\[truncated|\[unavailable\]`)

// SplitLines splits on \n, \r\n and \r, keeping the line endings.
func SplitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\n':
			out = append(out, s[start:i+1])
			start = i + 1
		case '\r':
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
			}
			out = append(out, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// Split separates content into (body, policy lines). The body keeps its line
// endings, so content with no policy line comes back byte for byte.
func Split(content string) (body, policy string) {
	var b, p strings.Builder
	for _, ln := range SplitLines(content) {
		if LineRE.MatchString(ln) {
			p.WriteString(ln)
		} else {
			b.WriteString(ln)
		}
	}
	return b.String(), strings.TrimSpace(p.String())
}

// MarkerPatterns are the strings a model is told to read as "this output is
// incomplete". Everything that shortens output must emit one of them.
var MarkerPatterns = []string{"[truncated", "chars omitted"}

// TruncationClause goes to every model that reads tool output.
const TruncationClause = `IMPORTANT, truncated output:
  If a tool result contains a truncation marker (text like "[truncated" or "chars omitted"),
  your answer MUST carry a visible warning, for example:
  "> Warning: output was truncated. Narrow the query with -n, -l or --tail to see it all."
  Never drop this warning silently. The user has to know the list is incomplete.`

// TruncationMarker is the one shape a truncation marker takes. unit names what
// was lost (chars, rows, lines), because a reader told the wrong one narrows
// the wrong thing.
func TruncationMarker(omitted int, unit, hint string) string {
	if unit == "" {
		unit = "chars"
	}
	tail := ""
	if hint != "" {
		tail = " - " + hint
	}
	return fmt.Sprintf("[truncated: %d %s omitted%s]", omitted, unit, tail)
}

// PartialContextClause is the same fact for a tier that must not print
// anything: triage answers in strict JSON, so it gets the inference rule and not
// an instruction to write a warning.
const PartialContextClause = `Context completeness:
  Text containing "[truncated" or "[Protected]" is PARTIAL. A resource missing from partial
  context is not evidence that it does not exist or that the cluster is healthy. Treat it as
  unknown and prefer "investigate" over answering from the snapshot alone.`

const UnavailableMarker = "[unavailable]"

// UnavailableNotice is the one shape a "this tool cannot answer in this
// session" reply takes.
func UnavailableNotice(reason string) string {
	return UnavailableMarker + " " + strings.TrimRight(reason, " \t\r\n") + " Retrying this tool will not change that."
}

// MarkUnavailable appends the notice to a tool's own error text as a trailing
// line. Never a wrapper: other readers key on how these replies start, and a
// leading marker would turn a refused read into a successful one.
func MarkUnavailable(text, reason string) string {
	if reason == "" {
		reason = text
	}
	return strings.TrimRight(text, " \t\r\n") + "\n" + UnavailableNotice(reason)
}

const RetryClause = `Unavailable tools:
  A tool result containing "[unavailable]" means that tool cannot answer at all right now: its
  backend is not configured, its binary is missing, or the endpoint cannot be reached. Do NOT
  call it again this turn. Say plainly what is missing and answer from what the other tools
  give you. Do not present its silence as evidence about the cluster.`
