package app

import (
	"context"
	"errors"
	"strings"

	"github.com/DinethShakya23/kube-sre/internal/agent"
	"github.com/DinethShakya23/kube-sre/internal/memory"
)

// memoryAdapter gives the agent its long term memory, built from the store, the
// episode recall and the change graph. It never fails a turn, and every failed
// read says so in the context it returns: a missing section is not an empty one.
type memoryAdapter struct {
	store *memory.Store
	cfg   changeCfg
}

type changeCfg struct{ minutes, limit int }

func newMemoryAdapter(s *memory.Store) *memoryAdapter {
	return &memoryAdapter{store: s, cfg: changeCfg{minutes: 15, limit: 12}}
}

const (
	recallUnavailable = "## Similar past episodes unavailable\nPast episodes could NOT be searched - this is not the same as there being " +
		"none. Do not assume this issue has no precedent."
	changesUnavailable = "## Recent cluster changes unavailable\nThe cluster change log could NOT be read - this is not the same as nothing " +
		"having changed. Do not tell the user the cluster has been quiet."
)

func (m *memoryAdapter) Load(ctx context.Context, req agent.LoadRequest) string {
	var parts []string
	pinned, err := m.store.LoadContext(ctx, req.UserID, req.SessionID, req.ClusterID)
	if err != nil {
		parts = append(parts, memory.UnavailableNotice())
	} else if pinned != "" {
		parts = append(parts, pinned)
	}

	if strings.TrimSpace(req.Query) != "" {
		eps, err := m.store.RecallEpisodes(ctx, req.Query, req.ClusterID, 3)
		switch {
		case errors.Is(err, memory.ErrRecallFailed):
			parts = append(parts, recallUnavailable)
		case err == nil:
			if block := memory.RenderRecallBlock(eps); block != "" {
				parts = append(parts, block)
			}
		}
	}

	block, err := m.store.RecentChangesBlock(ctx, req.ClusterID, m.cfg.minutes, m.cfg.limit)
	if err != nil {
		parts = append(parts, changesUnavailable)
	} else if block != "" {
		parts = append(parts, "## Recent cluster changes (last 15 min)\n"+block)
	}
	return strings.Join(parts, "\n\n")
}

func (m *memoryAdapter) Record(ctx context.Context, o agent.Outcome) {
	m.store.RecordOutcome(ctx, memory.RCAOutcome{
		SessionID: o.SessionID, UserID: o.UserID, RootCause: o.RootCause, Confidence: o.Confidence,
		RecommendedFix: o.RecommendedFix, Feedback: o.Feedback, ClusterID: o.ClusterID, Namespace: o.Namespace,
		Verified: o.Verified, Playbooks: o.Playbooks, Role: o.Role, RequestID: o.RequestID,
	})
	conf := o.Confidence
	ep := memory.EpisodeInput{
		ClusterID: o.ClusterID, Namespace: o.Namespace, TriggerKind: memory.TriggerKindFor(o.TriggerSource),
		TriggerDetail: o.TriggerDetail, Summary: o.Summary, RootCause: o.RootCause, Actions: o.Actions,
		Outcome: o.EpisodeOutcome, Verified: o.Verified, Confidence: &conf, Playbooks: o.Playbooks, Role: o.Role, RequestID: o.RequestID,
	}
	if ep.Summary == "" {
		ep.Summary = o.RootCause
	}
	m.store.WriteEpisode(ctx, ep)
}
