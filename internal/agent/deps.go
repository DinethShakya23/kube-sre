package agent

import (
	"context"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/change"
	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/events"
	"github.com/DinethShakya23/kube-sre/internal/llm"
	"github.com/DinethShakya23/kube-sre/internal/playbooks"
)

// LoadRequest asks memory for the context to pin into a turn.
type LoadRequest struct {
	UserID    string
	SessionID string
	ClusterID string
	// Query is the latest user message, used to recall similar past incidents.
	Query string
}

// Outcome is what a completed investigation or fix teaches memory.
type Outcome struct {
	SessionID string
	UserID    string
	ClusterID string
	Namespace string
	// RootCause is the root cause statement, or for a direct fix the structured
	// pattern key.
	RootCause      string
	Confidence     float64
	RecommendedFix string
	// Feedback is resolved, partial or regression, when the fix was verified.
	Feedback  *string
	Verified  *bool
	Playbooks []string
	Role      string

	TriggerSource  string
	TriggerDetail  string
	Summary        string
	Actions        []map[string]any
	EpisodeOutcome string // report_only for a synthesis
	RequestID      string
}

// Memory is the agent's long term memory. Its methods must not fail a turn: an
// investigation with no memory is still worth far more than no investigation. But
// a failed read must not be read as an absence, so an implementation words its
// own "memory unavailable" notice into the context it returns.
type Memory interface {
	// Load returns the context to pin into the coordinator prompt: preferences,
	// failure hints, past RCA, and the recalled episodes and recent changes.
	Load(ctx context.Context, req LoadRequest) string
	// Record persists an outcome as an rca_outcome row and an episode.
	Record(ctx context.Context, o Outcome)
}

// Checkpoints saves conversation state per session, which is what lets a write
// wait for approval and continue on the next message.
type Checkpoints interface {
	Load(ctx context.Context, session string) (*State, error)
	Save(ctx context.Context, session string, st *State) error
}

// Deps is everything an Agent needs.
type Deps struct {
	Cfg         *config.Config
	Tools       *Toolset
	Coordinator llm.Model
	Subagent    llm.Model
	Emitter     *events.Emitter
	Checkpoints Checkpoints
	Snapshot    *Snapshotter
	Playbooks   *playbooks.Registry
	Memory      Memory
	// Changes is the change ledger; Writeback feeds an investigation's evidence back
	// to the graph. Both are optional and gated by v5 flags.
	Changes   *change.Ledger
	Writeback func(ctx context.Context, cluster string, playbooks []string)
	// ClusterID resolves the cluster identity; it is called once per turn.
	ClusterID func(context.Context) string
	Now       func() time.Time
}

// Agent runs turns.
type Agent struct {
	Deps
}

// New builds an agent, filling in the defaults.
func New(d Deps) *Agent {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.ClusterID == nil {
		d.ClusterID = func(context.Context) string { return "unknown" }
	}
	if d.Memory == nil {
		d.Memory = nopMemory{}
	}
	return &Agent{Deps: d}
}

type nopMemory struct{}

func (nopMemory) Load(context.Context, LoadRequest) string { return "" }
func (nopMemory) Record(context.Context, Outcome)          {}
