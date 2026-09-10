package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/llm"
	"github.com/DinethShakya23/kube-sre/internal/redact"
)

var mutationVerbs = set("patch", "apply", "create", "delete", "scale", "set", "rollout", "edit", "replace")
var namespaceRe = regexp.MustCompile(`(?:^|\s)(?:-n|--namespace)[ =]([a-zA-Z0-9][a-zA-Z0-9\-]*)`)
var yamlNsRe = regexp.MustCompile(`(?m)^\s*namespace:\s*([a-zA-Z0-9][a-zA-Z0-9\-]*)`)

// Verification waits for these to clear.
var transitional = set("ContainerCreating", "Pending", "Terminating", "Init", "PodInitializing")

func commandVerb(cmd string) string {
	head := strings.Fields(strings.TrimSpace(cmd))
	switch {
	case len(head) > 1 && head[0] == "kubectl":
		return strings.ToLower(head[1])
	case len(head) > 0:
		return strings.ToLower(head[0])
	}
	return ""
}

// ranMutation returns the kubectl commands, among the run_kubectl calls in msgs,
// that start with a mutation verb.
func ranMutation(msgs []llm.Message) []string {
	var cmds []string
	for _, m := range msgs {
		if m.Role != llm.Assistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			cmd, _ := tc.Args["command"].(string)
			if cmd != "" && mutationVerbs[commandVerb(cmd)] {
				cmds = append(cmds, clip(strings.TrimSpace(cmd), 200))
			}
		}
	}
	return cmds
}

// mutationPairs pairs each mutation command with its stdin YAML, redacted before
// it is kept.
func (a *Agent) mutationPairs(msgs []llm.Message) []map[string]any {
	var pairs []map[string]any
	for _, m := range msgs {
		if m.Role != llm.Assistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			cmd, _ := tc.Args["command"].(string)
			if cmd == "" || !mutationVerbs[commandVerb(cmd)] {
				continue
			}
			var stdin string
			for _, k := range []string{"stdin", "stdin_yaml", "manifest", "yaml"} {
				if s, _ := tc.Args[k].(string); s != "" {
					stdin = s
					break
				}
			}
			c, y := clip(cmd, 200), clip(stdin, 1500)
			if a.Cfg.RedactSecrets {
				c, y = redact.Secrets(cmd, 200), redact.Secrets(stdin, 1500)
			}
			var yy any
			if y != "" {
				yy = y
			}
			pairs = append(pairs, map[string]any{"command": c, "stdin_yaml": yy})
		}
	}
	return pairs
}

// inferNamespace picks the most cited namespace across kubectl args and stdin
// YAML metadata. `kubectl apply -f -` has no -n, so the namespace is in the YAML.
func inferNamespace(cmds []string, pairs []map[string]any) string {
	counts := map[string]int{}
	for _, c := range cmds {
		if m := namespaceRe.FindStringSubmatch(c); m != nil {
			counts[m[1]]++
		}
	}
	for _, p := range pairs {
		y, _ := p["stdin_yaml"].(string)
		for _, m := range yamlNsRe.FindAllStringSubmatch(y, -1) {
			counts[m[1]]++
		}
	}
	best, n := "", 0
	names := make([]string, 0, len(counts))
	for k := range counts {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if counts[k] > n {
			best, n = k, counts[k]
		}
	}
	return best
}

// outcomeKey is the stable structured pattern key.
func outcomeKey(st *State, namespace string) string {
	cluster := st.ClusterID
	if cluster == "" {
		cluster = "unknown"
	}
	if len(st.MatchedPlaybooks) > 0 {
		seen := map[string]bool{}
		var names []string
		for _, p := range st.MatchedPlaybooks {
			if !seen[p] {
				seen[p] = true
				names = append(names, p)
			}
		}
		sort.Strings(names)
		ns := ""
		if namespace != "" {
			ns = " | ns=" + namespace
		}
		return fmt.Sprintf("playbook=%s%s | cluster=%s", strings.Join(names, "+"), ns, cluster)
	}
	// The query stub is the low quality path and never promotes.
	last := clip(strings.TrimSpace(oneLine(lastUserText(st))), 60)
	return fmt.Sprintf("query=%s | cluster=%s", last, cluster)
}

// waitForRollout polls until pods in the namespace leave transitional statuses or
// the wait runs out. `kubectl apply` returns at once but a rollout takes seconds,
// and a snapshot taken mid roll would report "partial" for a correct fix.
func (a *Agent) waitForRollout(ctx context.Context, ns string, maxWait, poll time.Duration) {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		out := a.Snapshot.ReadText(ctx, []string{"get", "pods", "-n", ns, "--no-headers"})
		moving := false
		for _, line := range strings.Split(out, "\n") {
			cols := strings.Fields(line)
			if len(cols) < 3 {
				continue
			}
			if transitional[cols[2]] || strings.HasPrefix(cols[2], "Init:") || strings.HasPrefix(cols[2], "PodInitializing") {
				moving = true
				break
			}
		}
		if !moving {
			return
		}
		wait := time.Until(deadline)
		if wait > poll {
			wait = poll
		}
		if wait <= 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// verifyResolution re-reads the cluster after a fix. It returns (verified,
// feedback): true and "resolved" when nothing is unhealthy, false and "partial"
// or "regression" when it is, and nil when verification is off or could not run.
func (a *Agent) verifyResolution(ctx context.Context, ns string, hadIssues bool) (*bool, *string) {
	if !a.Cfg.ReflexionVerify {
		return nil, nil
	}
	if ns != "" {
		a.waitForRollout(ctx, ns, 30*time.Second, 2*time.Second)
	}
	nsArg := []string{"--all-namespaces"}
	if ns != "" {
		nsArg = []string{"-n", ns}
	}
	podsOK, pods, _ := a.Snapshot.Read(ctx, append([]string{"get", "pods"}, nsArg...))
	eventsOK, events, _ := a.Snapshot.Read(ctx, append(append([]string{"get", "events"}, nsArg...), "--sort-by=.lastTimestamp", "--field-selector=type=Warning"))
	if !podsOK {
		// A failed post fix read must not be recorded as verified: promotion selects
		// on verified, and a read is most likely to fail right after a disruptive change.
		slog.Warn("cannot verify resolution, the post fix cluster read failed; recording the outcome as unverified",
			"reason", unavailableReason(pods))
		return nil, nil
	}
	issues, _, _ := ScanSnapshot(pods, events, podsOK, eventsOK)
	tr, fl := true, false
	resolved, partial, regression := "resolved", "partial", "regression"
	if !issues {
		// Healthy pods mean the fix is in effect even if old warnings linger.
		return &tr, &resolved
	}
	if !hadIssues {
		return &fl, &regression
	}
	return &fl, &partial
}

// resolveConfidence: verified with a playbook is 0.9 (eligible for promotion),
// either alone is 0.7, neither is 0.5.
func resolveConfidence(hasPlaybook bool, verified *bool) float64 {
	v := verified != nil && *verified
	switch {
	case v && hasPlaybook:
		return 0.9
	case v || hasPlaybook:
		return 0.7
	}
	return 0.5
}

// maybeRecordDirectOutcome persists a structured, verified outcome when a direct
// answer mutated state. It runs after the answer is delivered, off the request path.
func (a *Agent) maybeRecordDirectOutcome(ctx context.Context, st *State, msgs []llm.Message) {
	cmds := ranMutation(msgs)
	if len(cmds) > 0 && a.Cfg.CortexV5 && a.Cfg.ChangeLedger && a.Changes != nil {
		// So change first RCA can rank what was just applied on a later investigation.
		a.Changes.RecordCommands(st.ClusterID, cmds, float64(a.Now().UnixNano())/1e9, "")
	}
	if !a.Cfg.Reflexion || len(cmds) == 0 {
		return
	}
	pairs := a.mutationPairs(msgs)
	ns := inferNamespace(cmds, pairs)
	hadIssues := st.SnapshotHasIssues
	// The turn keeps changing the state, so copy what the write needs.
	snapshot := State{
		SessionID: st.SessionID, UserID: st.UserID, ClusterID: st.ClusterID, UserRole: st.UserRole,
		TriggerSource: st.TriggerSource, MatchedPlaybooks: append([]string(nil), st.MatchedPlaybooks...),
		Messages: []llm.Message{{Role: llm.User, Content: lastUserText(st)}},
	}
	a.recordAsync(func(ctx context.Context) {
		verified, feedback := a.verifyResolution(ctx, ns, hadIssues)
		key := outcomeKey(&snapshot, ns)
		payload, _ := json.Marshal(pairs)
		conf := resolveConfidence(len(snapshot.MatchedPlaybooks) > 0, verified)
		a.Memory.Record(ctx, Outcome{
			SessionID: snapshot.SessionID, UserID: snapshot.UserID, ClusterID: snapshot.ClusterID, Namespace: ns,
			RootCause: clip(key, 240), Confidence: conf, RecommendedFix: clip(string(payload), 8000),
			Feedback: feedback, Verified: verified, Playbooks: snapshot.MatchedPlaybooks, Role: snapshot.UserRole,
			RequestID: snapshot.SessionID, TriggerSource: snapshot.TriggerSource, TriggerDetail: clip(lastUserText(&snapshot), 300),
			Summary: fmt.Sprintf("Direct fix in ns=%s: %s", ns, clip(key, 200)), Actions: pairs,
			EpisodeOutcome: derefStr(feedback),
		})
		slog.Info("rca outcome written", "session", snapshot.SessionID, "cluster", snapshot.ClusterID, "confidence", conf)
	})
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
