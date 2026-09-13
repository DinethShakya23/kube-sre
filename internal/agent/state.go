// Package agent is the investigation agent: a coordinator that answers from a
// live cluster snapshot and its tools, and either digs into one resource or fans
// out to specialist subagents when the fault is wider than that.
//
// A turn runs:
//
//	memory -> snapshot -> coordinator -+-> answer
//	                                   +-> targeted reads -> coordinator
//	                                   +-> 4 subagents -> coordinator (synthesis)
//
// State is checkpointed per session, which is what lets a write wait for human
// approval and pick up again on the next message.
package agent

import (
	"github.com/DinethShakya23/kube-sre/internal/cortex"
	"github.com/DinethShakya23/kube-sre/internal/kube"
	"github.com/DinethShakya23/kube-sre/internal/llm"
)

// PlanStep is one step of a coordinator investigation plan. "failed" exists
// because "done" was once set whenever a tool batch returned, and every tool
// error is an ordinary tool message, so a step whose every tool errored showed a
// green tick in the live plan.
type PlanStep struct {
	Description string `json:"description"`
	Status      string `json:"status"` // pending | in_progress | done | skipped | failed
}

// Finding is what one specialist subagent reports.
type Finding struct {
	Domain        string   `json:"domain"` // pod | metrics | logs | events
	Signals       []string `json:"signals"`
	Hypothesis    string   `json:"hypothesis"`
	Confidence    float64  `json:"confidence"`
	Evidence      []string `json:"evidence"`
	ToolCallsMade []string `json:"tool_calls_made"`
}

// RCAResult is the coordinator's synthesis of the subagent findings.
type RCAResult struct {
	RootCause           string   `json:"root_cause"`
	Confidence          float64  `json:"confidence"`
	SupportingEvidence  []string `json:"supporting_evidence"`
	ConflictingEvidence []string `json:"conflicting_evidence"`
	Reasoning           string   `json:"reasoning"`
	RecommendedFix      string   `json:"recommended_fix"`
	AffectedDomain      []string `json:"affected_domain"`
}

// Targeted is a single resource issue the coordinator wants read in depth.
type Targeted struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Issue     string `json:"issue"`
}

// Pending is a tool call waiting for a human. The coordinator's tool loop is
// saved mid flight: Loop holds every message it produced so far, including the
// results of calls that already ran, so resuming continues the loop instead of
// asking the model again.
type Pending struct {
	Approval kube.Approval `json:"approval"`
	Call     llm.ToolCall  `json:"call"`
	Loop     []llm.Message `json:"loop"`
	Rounds   int           `json:"rounds"`
}

// State is carried through a turn and saved between turns.
type State struct {
	Messages []llm.Message `json:"messages"`

	// Loaded before the coordinator runs, and pinned into its system prompt.
	MemoryContext string `json:"memory_context"`
	// budget is the turn's latency tracker, when responsiveness is on. Not persisted.
	budget *cortex.PhaseBudget
	// Pre fetched live pod state and warning events, so the coordinator can answer
	// without extra tool calls.
	ClusterSnapshot string `json:"cluster_snapshot"`

	Findings    []Finding  `json:"findings"`
	RCARequired bool       `json:"rca_required"`
	RCAResult   *RCAResult `json:"rca_result"`
	Targeted    *Targeted  `json:"targeted_investigation"`
	Pending     *Pending   `json:"pending_hitl"`

	// Snapshot health flags. They only ever bias the prompt; they never gate a tool.
	SnapshotHasIssues   bool `json:"snapshot_has_issues"`   // any pod not Running, Completed or Succeeded
	SnapshotHasWarnings bool `json:"snapshot_has_warnings"` // any Warning event
	SnapshotPodCount    int  `json:"snapshot_pod_count"`
	SnapshotReadFailed  bool `json:"snapshot_read_failed"` // the kubectl read failed, so the flags mean nothing
	// The read succeeded and the text is real, but it is not all of it: a listing
	// longer than the cap was cut. Kept apart from the two health flags, which say
	// "unhealthy workloads were observed" and are shown in those words: a cluster
	// nobody could measure must not be reported as an unhealthy one.
	SnapshotComplete bool    `json:"snapshot_complete"`
	SnapshotBuiltAt  float64 `json:"snapshot_built_at"`

	Plan             []PlanStep `json:"investigation_plan"`
	MatchedPlaybooks []string   `json:"matched_playbooks"`

	SessionID string `json:"session_id"`
	UserID    string `json:"user_id"`
	UserRole  string `json:"user_role"` // superadmin | admin | operator | readonly, set by the API auth layer
	// TriggerSource is where the turn came from, and the only input to the memory
	// write admission trust score. It must never be derivable from anything a chat
	// client sends: user_id is a free form request field, so trusting it would let
	// any caller claim sensor provenance. Only an in process caller sets it.
	TriggerSource string `json:"trigger_source"` // detector | user_query
	ClusterID     string `json:"cluster_id"`
}

// TurnRequest starts one turn.
type TurnRequest struct {
	Message     string
	SessionID   string
	UserID      string
	UserRole    string
	AutoApprove bool
	// TriggerSource is "user_query" unless the caller is in process.
	TriggerSource string
}
