package autonomy

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/agent"
	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/detect"
)

const (
	cooldown      = 30 * time.Minute
	maxConcurrent = 2
)

// Watchtower turns findings into investigations, so the agent does not wait to be
// asked. When a compiled detector fires (which costs nothing) it resolves the
// namespace's autonomy level and, at A1 and above, runs an investigation through
// the same turn machinery chat uses. At A3, and only for an allowlisted
// (playbook, namespace) pair, the investigation runs with approval bypassed so the
// proposed fix executes, and the turn's own verification follows.
//
// Guard rails: a cooldown per (playbook, namespace, object) so a flapping
// condition does not spawn investigation storms; a global concurrency cap;
// protected namespaces pinned to A0 by the ladder; and the autonomous identity runs
// with a configured role (default operator), so the role ceiling still applies
// inside the tools.
type Watchtower struct {
	Cfg    *config.Config
	Ladder Ladder
	// Prepare readies a session's event stream; Investigate runs one turn. They
	// are functions so the watchtower does not depend on how a turn is run.
	Prepare     func(session string)
	Investigate func(ctx context.Context, req agent.TurnRequest)
	// AutoWritePermitted is an extra fail closed brake on autonomous writes, such
	// as a kill switch or a change freeze. Nil means no extra brake.
	AutoWritePermitted func() (allow bool, reason string)
	// AutofixRevoked returns a reason when the recorded record has taken A3 write
	// authority away. It can close the gate, never open it. Nil means no brake.
	AutofixRevoked func(ctx context.Context) string
	// AfterFix runs after an autonomous fix, for example to schedule a re-check.
	AfterFix func(f detect.Finding)
	Now      func() time.Time

	mu     sync.Mutex
	recent map[[3]string]time.Time
	sem    chan struct{}
	wg     sync.WaitGroup
	ctx    context.Context
}

func NewWatchtower(cfg *config.Config) *Watchtower {
	return &Watchtower{Cfg: cfg, Ladder: Ladder{cfg}, Now: time.Now, recent: map[[3]string]time.Time{},
		sem: make(chan struct{}, maxConcurrent)}
}

// Start binds the watchtower's investigations to ctx.
func (w *Watchtower) Start(ctx context.Context) { w.ctx = ctx }

// Wait blocks until every running investigation has finished.
func (w *Watchtower) Wait() { w.wg.Wait() }

// OnFinding is the detector engine callback. It is synchronous, non blocking and
// never panics.
func (w *Watchtower) OnFinding(f detect.Finding) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("watchtower on_finding error", "err", r)
		}
	}()
	if !w.Cfg.Watchtower {
		return
	}
	level := w.Ladder.LevelFor(f.Namespace)
	if !AtLeast(level, "A1") {
		return
	}
	key := [3]string{f.Playbook, f.Namespace, f.Object}
	now := w.Now()
	w.mu.Lock()
	if last, ok := w.recent[key]; ok && now.Sub(last) < cooldown {
		w.mu.Unlock()
		return
	}
	w.recent[key] = now
	w.mu.Unlock()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.investigate(f, level)
	}()
}

// shouldAutoFix: A3 auto fix is allowed only for realized failures on allowlisted
// pairs. A predicted finding is lower confidence than a realized one, so it may
// pre empt (investigate, advise) but must NEVER drive a destructive autonomous
// action, whatever the level or allowlist.
func (w *Watchtower) shouldAutoFix(f detect.Finding, level string) bool {
	if f.Severity == "predicted" {
		return false
	}
	if !(level == "A3" && w.Ladder.A3Allowed(f.Playbook, f.Namespace)) {
		return false
	}
	if w.AutoWritePermitted != nil {
		if ok, reason := w.AutoWritePermitted(); !ok {
			slog.Info("watchtower A3 auto fix denied by the blast radius gate", "reason", reason)
			return false
		}
	}
	return true
}

func (w *Watchtower) question(f detect.Finding, level string, autoFix bool) string {
	if f.Severity == "predicted" {
		eta := "?"
		if f.ETAMinutes != nil {
			eta = fmt.Sprint(*f.ETAMinutes)
		}
		return fmt.Sprintf("[autonomous investigation - PREDICTED failure %s] The trend detector projects a failure for object '%s' "+
			"in namespace '%s' in ~%sm (evidence: %s). Diagnose what is trending and recommend a pre-emptive action. "+
			"Do NOT execute destructive fixes.", f.Playbook, f.Object, f.Namespace, eta, f.Evidence)
	}
	ask := fmt.Sprintf("[autonomous investigation - triggered by detector %s] The detector fired for object '%s' in namespace '%s' "+
		"(evidence: %s). Diagnose the root cause and report it concisely.", f.Playbook, f.Object, f.Namespace, f.Evidence)
	switch {
	case autoFix:
		ask += " Then apply the appropriate fix and verify it worked."
	case AtLeast(level, "A2"):
		ask += " Propose the exact fix commands but do not execute destructive actions."
	}
	return ask
}

func (w *Watchtower) investigate(f detect.Finding, level string) {
	ctx := w.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	autoFix := w.shouldAutoFix(f, level)
	if autoFix && w.AutofixRevoked != nil {
		if reason := w.AutofixRevoked(ctx); reason != "" {
			slog.Warn("watchtower A3 auto fix revoked by the recorded record", "reason", reason)
			autoFix = false
		}
	}
	session := "auto-" + f.ID
	ask := w.question(f, level, autoFix)

	select {
	case w.sem <- struct{}{}:
		defer func() { <-w.sem }()
	case <-ctx.Done():
		return
	}
	slog.Info("watchtower investigating", "playbook", f.Playbook, "ns", f.Namespace, "object", f.Object,
		"level", level, "auto_fix", autoFix, "session", session)
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("watchtower investigation failed", "session", session, "err", r)
		}
	}()
	if w.Prepare != nil {
		w.Prepare(session)
	}
	w.Investigate(ctx, agent.TurnRequest{
		Message: ask, SessionID: session, UserID: "watchtower", UserRole: w.Cfg.WatchtowerRole,
		AutoApprove: autoFix,
		// In process and detector triggered: the one caller entitled to sensor trust.
		TriggerSource: "detector",
	})
	slog.Info("watchtower done", "session", session)
	if autoFix && w.AfterFix != nil {
		w.AfterFix(f)
	}
}

// ResetCooldowns clears the cooldown table, for tests.
func (w *Watchtower) ResetCooldowns() {
	w.mu.Lock()
	w.recent = map[[3]string]time.Time{}
	w.mu.Unlock()
}
