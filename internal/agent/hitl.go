package agent

import "strings"

// Approval and denial detection is plain string matching with no infrastructure.
// An approval must be recognised, never merely "not recognised as a denial": the
// resume decision once defaulted to approval, so every reply that was not on a
// short list ("No.", "no thanks", "wait", "why?", an empty message) executed the
// pending destructive command. Anything that is not an approval is a denial.

var approvalPhrases = set(
	"yes", "approve", "approved", "do it", "yes do it",
	"go ahead", "confirm", "ok", "okay", "sure", "proceed", "run it",
)

var denialPhrases = set("no", "deny", "denied", "cancel", "abort", "stop", "nope", "don't", "dont")

var autoApprovePhrases = set(
	"approve all", "auto approve", "auto-approve", "yes to all",
	"approve everything", "skip approval", "bypass hitl", "/auto-approve",
)

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}

// normalise lowercases, trims, and drops surrounding quotes and trailing
// sentence punctuation. Without it "No." matched nothing, and since the resume
// decision defaulted to approval a full stop turned a refusal into an execution.
func normalise(message string) string {
	s := strings.TrimSpace(message)
	s = strings.Trim(s, `"'`)
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, ".!")
	return strings.ToLower(strings.TrimSpace(s))
}

// IsApproval is true only for an explicit, recognised approval. This is the
// load bearing direction.
func IsApproval(message string) bool { return approvalPhrases[normalise(message)] }

func IsDenial(message string) bool { return denialPhrases[normalise(message)] }

// IsAutoApproveRequest is true if the user wants approval skipped for this turn.
func IsAutoApproveRequest(message string) bool { return autoApprovePhrases[normalise(message)] }
