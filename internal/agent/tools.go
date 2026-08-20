package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/DinethShakya23/kube-sre/internal/helm"
	"github.com/DinethShakya23/kube-sre/internal/kube"
	"github.com/DinethShakya23/kube-sre/internal/llm"
	"github.com/DinethShakya23/kube-sre/internal/loki"
	"github.com/DinethShakya23/kube-sre/internal/prom"
	"github.com/DinethShakya23/kube-sre/internal/redact"
)

// Toolset is everything the agent can call.
type Toolset struct {
	Kubectl *kube.Tool
	Helm    *helm.Tool
	Prom    *prom.Client
	Loki    *loki.Client
}

const (
	ToolKubectl    = "run_kubectl"
	ToolHelm       = "run_helm"
	ToolPrometheus = "query_prometheus"
	ToolLoki       = "query_loki"
)

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

// Specs describes the four tools to the model.
func (t *Toolset) Specs() []llm.ToolSpec {
	return []llm.ToolSpec{
		{
			Name: ToolKubectl,
			Description: "Run a kubectl command against the configured cluster.\n" +
				"Give the whole command as one string, for example:\n" +
				"  kubectl get pods -n shop\n" +
				"  kubectl describe deployment payments-api -n shop\n" +
				"  kubectl logs payments-api-7b9d8f6c5-x2k4m -n shop --tail=100 --since=5m\n" +
				"  kubectl rollout status deployment/payments-api -n shop\n" +
				"  kubectl apply -f -   (put the manifest in the stdin argument)\n" +
				"Output is capped at 8000 characters. Any write waits for a human to approve it.",
			Parameters: obj(map[string]any{
				"command": str("The kubectl command."),
				"stdin":   str("Optional YAML piped to stdin, for kubectl apply -f -."),
			}, "command"),
		},
		{
			Name: ToolHelm,
			Description: "Run a read only Helm command to inspect release state. Use it whenever a workload was deployed with Helm.\n" +
				"Supported: helm list [-n ns] [-A]; helm status <release> [-n ns]; helm get values|manifest|notes|all <release> [-n ns]; " +
				"helm history <release> [-n ns]; helm show chart|values <chart>; helm env; helm version.\n" +
				"Not supported: install, upgrade, rollback, uninstall, repo and plugin changes.",
			Parameters: obj(map[string]any{"command": str("The helm command.")}, "command"),
		},
		{
			Name: ToolPrometheus,
			Description: "Query Prometheus for cluster metrics with PromQL. Prometheus stores metrics over time (usage, rates, restart counts), " +
				"never events, specs (limits and requests), endpoints, or reachability: those are kubectl questions.\n" +
				"Examples: sum(rate(container_cpu_usage_seconds_total[5m])) by (pod); " +
				"kube_pod_status_phase{namespace=\"shop\",phase=\"Running\"}; container_memory_working_set_bytes{pod=~\"payments-api.*\"}.\n" +
				"range_minutes 0 is an instant query for current values. Above 0 it is a range query over the last N minutes, " +
				"returning min, avg and max per series. Use a range only when the user asks about history or trends.",
			Parameters: obj(map[string]any{
				"promql":        str("The PromQL expression."),
				"range_minutes": integer("0 for an instant query, or the number of minutes to look back."),
			}, "promql"),
		},
		{
			Name: ToolLoki,
			Description: "Query Loki for container and application logs with LogQL. Prefer it over kubectl logs for logs from several pods at once, " +
				"history from crashed or restarted containers, structured filtering, and log rates.\n" +
				"Examples: {namespace=\"shop\", pod=~\"payments-api.*\"}; {namespace=\"shop\"} |= \"ERROR\"; " +
				"{app=\"nginx\"} | json | status >= 500; rate({namespace=\"shop\"} |= \"error\" [5m]).",
			Parameters: obj(map[string]any{
				"logql": str("The LogQL expression."),
				"limit": integer("Maximum log lines to return. Default 100, at most 500. Ignored for metric queries."),
				"since": str("How far back to look, such as 15m, 1h, 6h or 24h. Default 1h."),
			}, "logql"),
		},
	}
}

// Names lists the tool names, sorted.
func (t *Toolset) Names() []string {
	names := []string{ToolKubectl, ToolHelm, ToolPrometheus, ToolLoki}
	sort.Strings(names)
	return names
}

// need reads a required string argument.
func need(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", fmt.Errorf("missing required argument %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string", key)
	}
	return s, nil
}

func optString(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string", key)
	}
	return s, nil
}

func optInt(args map[string]any, key string, def int) (int, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return def, nil
	}
	switch n := v.(type) {
	case float64:
		return int(n), nil
	case int:
		return n, nil
	case string:
		var i int
		if _, err := fmt.Sscanf(n, "%d", &i); err == nil {
			return i, nil
		}
	}
	return 0, fmt.Errorf("argument %q must be an integer", key)
}

// callOutcome is the result of one tool call.
type callOutcome struct {
	call    llm.ToolCall
	content string
	isError bool
	// approval is set when the call is a write that a human must approve. The call
	// has not run.
	approval *kube.Approval
	plan     *kube.Plan
}

func (o callOutcome) message() llm.Message {
	return llm.Message{Role: llm.Tool, ToolCallID: o.call.ID, Name: o.call.Name, Content: o.content, IsError: o.isError}
}

// toolFailure words an ordinary failure as a result. Only ordinary failures are
// isolated this way: one tool erroring must not sink the batch it ran in.
func toolFailure(call llm.ToolCall, err error) callOutcome {
	var invocation string
	if cmd, _ := call.Args["command"].(string); cmd != "" {
		invocation = fmt.Sprintf(" command='%s'", redact.Secrets(cmd, 500))
	}
	reason := redact.Secrets(err.Error(), 500)
	slog.Warn("tool call failed independently", "tool", call.Name, "call", call.ID, "reason", reason)
	return callOutcome{call: call, isError: true,
		content: fmt.Sprintf("Tool error: %s%s: %s", call.Name, invocation, reason)}
}

// prepare validates a call's arguments and, for run_kubectl, runs every gate up to
// execution. It returns an outcome that is already final (a refusal or an error),
// one that needs approval, or one with a plan ready to run.
func (t *Toolset) prepare(call llm.ToolCall, c kube.Call) callOutcome {
	switch call.Name {
	case ToolKubectl, ToolHelm, ToolPrometheus, ToolLoki:
	default:
		return callOutcome{call: call, isError: true,
			content: fmt.Sprintf("Error: %s is not a valid tool, try one of [%s].", call.Name, strings.Join(t.Names(), ", "))}
	}
	if call.Args == nil {
		return callOutcome{call: call, isError: true,
			content: fmt.Sprintf("Error: invalid arguments for %s: they must be a JSON object, got %q.", call.Name, clip(call.RawArgs, 200))}
	}
	bad := func(err error) callOutcome {
		return callOutcome{call: call, isError: true, content: fmt.Sprintf("Error: invalid arguments for %s: %v.", call.Name, err)}
	}
	switch call.Name {
	case ToolKubectl:
		command, err := need(call.Args, "command")
		if err != nil {
			return bad(err)
		}
		stdin, err := optString(call.Args, "stdin")
		if err != nil {
			return bad(err)
		}
		plan, err := t.Kubectl.Plan(command, stdin, c)
		if err != nil {
			return toolFailure(call, err)
		}
		if plan.Refusal != "" {
			return callOutcome{call: call, content: plan.Refusal}
		}
		return callOutcome{call: call, approval: plan.Approval, plan: plan}
	case ToolHelm:
		if _, err := need(call.Args, "command"); err != nil {
			return bad(err)
		}
	case ToolPrometheus:
		if _, err := need(call.Args, "promql"); err != nil {
			return bad(err)
		}
		if _, err := optInt(call.Args, "range_minutes", 0); err != nil {
			return bad(err)
		}
	case ToolLoki:
		if _, err := need(call.Args, "logql"); err != nil {
			return bad(err)
		}
		if _, err := optInt(call.Args, "limit", 100); err != nil {
			return bad(err)
		}
		if _, err := optString(call.Args, "since"); err != nil {
			return bad(err)
		}
	}
	return callOutcome{call: call}
}

// execute runs a call that has passed prepare and needs no approval, or one that
// a human has approved.
func (t *Toolset) execute(ctx context.Context, o callOutcome, c kube.Call) callOutcome {
	call := o.call
	switch call.Name {
	case ToolKubectl:
		out, err := t.Kubectl.Execute(ctx, o.plan, c)
		if err != nil {
			return toolFailure(call, err)
		}
		return callOutcome{call: call, content: out}
	case ToolHelm:
		cmd, _ := need(call.Args, "command")
		return callOutcome{call: call, content: t.Helm.Run(ctx, cmd)}
	case ToolPrometheus:
		q, _ := need(call.Args, "promql")
		minutes, _ := optInt(call.Args, "range_minutes", 0)
		return callOutcome{call: call, content: t.Prom.Query(ctx, q, minutes)}
	case ToolLoki:
		q, _ := need(call.Args, "logql")
		limit, _ := optInt(call.Args, "limit", 100)
		since, _ := optString(call.Args, "since")
		if since == "" {
			since = "1h"
		}
		return callOutcome{call: call, content: t.Loki.Query(ctx, q, limit, since)}
	}
	return callOutcome{call: call, isError: true, content: "Error: unknown tool"}
}

// callInfo is what an event shows about a call: run_kubectl and run_helm show the
// command, the query tools show none.
func callInfo(call llm.ToolCall) *string {
	if call.Name == ToolKubectl || call.Name == ToolHelm {
		if cmd, ok := call.Args["command"].(string); ok {
			return &cmd
		}
	}
	return nil
}

// batchHooks lets the caller observe a batch as it runs.
type batchHooks struct {
	onStart func(llm.ToolCall)
	onEnd   func(llm.ToolCall, string)
}

// runBatch prepares every call, holds back the first one that needs approval, and
// runs the rest in parallel. The held back call is returned unexecuted; any other
// call that also needs approval gets a placeholder result, because the model is
// told to propose one mutation per response and a second prompt for the same
// batch would be a redundant round trip.
func (t *Toolset) runBatch(ctx context.Context, calls []llm.ToolCall, c kube.Call, h batchHooks) (msgs []llm.Message, held *llm.ToolCall, approval *kube.Approval) {
	prepared := make([]callOutcome, len(calls))
	for i, call := range calls {
		prepared[i] = t.prepare(call, c)
	}
	results := make([]callOutcome, len(calls))
	filled := make([]bool, len(calls))
	var wg sync.WaitGroup
	for i, o := range prepared {
		switch {
		case o.approval != nil && held == nil:
			cp := o.call
			held, approval = &cp, o.approval
			continue
		case o.approval != nil:
			results[i] = callOutcome{call: o.call, content: "Skipped - pending approval; will be re-proposed in a separate response."}
			filled[i] = true
			continue
		case o.plan == nil:
			// Already final: a refusal, a validation error, or a tool that needs no plan.
			if o.content != "" || o.isError {
				results[i], filled[i] = o, true
				if h.onStart != nil {
					h.onStart(o.call)
				}
				if h.onEnd != nil {
					h.onEnd(o.call, o.content)
				}
				continue
			}
		}
		filled[i] = true
		wg.Add(1)
		go func(i int, o callOutcome) {
			defer wg.Done()
			if h.onStart != nil {
				h.onStart(o.call)
			}
			results[i] = t.execute(ctx, o, c)
			if h.onEnd != nil {
				h.onEnd(o.call, results[i].content)
			}
		}(i, prepared[i])
	}
	wg.Wait()
	for i, r := range results {
		if filled[i] {
			msgs = append(msgs, r.message())
		}
	}
	return msgs, held, approval
}

// resolvePending runs or cancels the call that was waiting for a human. The call
// is planned again rather than restored: a plan is not saved with the pause, and
// planning again means a change of role or configuration since the request still
// counts.
func (t *Toolset) resolvePending(ctx context.Context, call llm.ToolCall, approved bool, c kube.Call, h batchHooks) llm.Message {
	if !approved {
		return callOutcome{call: call, content: "Action cancelled by user."}.message()
	}
	o := t.prepare(call, c)
	if o.plan == nil || o.plan.Refusal != "" {
		return o.message()
	}
	if h.onStart != nil {
		h.onStart(call)
	}
	out := t.execute(ctx, o, c)
	if h.onEnd != nil {
		h.onEnd(call, out.content)
	}
	return out.message()
}
