package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/kube"
	"github.com/DinethShakya23/kube-sre/internal/nsguard"
	"github.com/DinethShakya23/kube-sre/internal/policy"
)

const snapshotMaxChars = 8000

const policyPrefix = "[Protected]"

// Pod statuses that count as healthy. Anything else flips SnapshotHasIssues. The
// STATUS column mixes phases and reasons (CrashLoopBackOff, ImagePullBackOff), so
// any value not in this set is an issue, which is what is wanted.
var healthyPodStatuses = set("Running", "Completed", "Succeeded")

// Snapshotter runs the fixed, internal reads that build the cluster snapshot.
//
// It is a second kubectl executor. It exists because the snapshot is a fixed
// read that should not pay for tool dispatch, but "internal" was once taken to
// mean "trusted" and it ran whatever it was handed. Two of its callers pass an
// argument list that is not fixed: targeted reads build -n from a namespace the
// model wrote, and verification passes one derived from an applied manifest. So
// the gate lives here, at the one place the process is launched, and it is the
// blocklist itself rather than a second reading of it.
type Snapshotter struct {
	Bin        string
	Kubeconfig string
	Timeout    time.Duration
	Blocked    nsguard.Blocklist
}

func (s *Snapshotter) env() []string {
	kc := s.Kubeconfig
	if strings.HasPrefix(kc, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			kc = filepath.Join(home, kc[2:])
		}
	}
	return append(os.Environ(), "KUBECONFIG="+kc)
}

// refusal says why a read must not run, or "": the same policy run_kubectl
// applies. The connection and identity family is refused because which cluster
// this talks to is the deployment's decision.
func (s *Snapshotter) refusal(args []string) string {
	if flag := kube.ConnectionFlagIn(args); flag != "" {
		return fmt.Sprintf("%s '%s' is not permitted. Which cluster this connects to, and the identity it uses, are fixed by the deployment.", policyPrefix, flag)
	}
	if ns := kube.FlagValue(args, "-n", "--namespace", ""); ns != "" && s.Blocked[strings.ToLower(strings.TrimSpace(ns))] {
		return nsguard.ProtectedMessage(ns)
	}
	return ""
}

// filterOutput removes blocked namespace rows from a cluster wide snapshot,
// exactly as run_kubectl filters the same command's output, and returns how many
// were dropped. The count comes back instead of the sentence so the caller can
// append it after the length cap: put inside a long table that is then sliced, it
// would be deleted, which is the outcome the sentence exists to prevent.
func (s *Snapshotter) filterOutput(args []string, out string) (string, int) {
	if !kube.IsAllNamespaces(args) {
		return out, 0
	}
	return s.Blocked.DropTableRows(out)
}

// Read runs a read only kubectl command and returns (ok, text, complete).
//
// ok is false when kubectl exited non zero, could not be started or timed out.
// The text is still returned as the operator facing explanation, but a caller
// must not treat it as cluster data: parsing an error as a pod table once made an
// unreachable cluster read as empty and healthy.
//
// complete is false when the output was longer than the cap. It is a separate
// fact from ok: the read worked, the text is real, and it is not all of it.
func (s *Snapshotter) Read(ctx context.Context, args []string) (ok bool, text string, complete bool) {
	if msg := s.refusal(args); msg != "" {
		slog.Warn("context fetcher refused a snapshot read", "args", args, "reason", msg)
		return false, msg, true
	}
	cctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, s.Bin, args...)
	cmd.Env = s.env()
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	stdout := strings.ToValidUTF8(so.String(), "�")
	stderr := strings.ToValidUTF8(se.String(), "�")
	if err != nil {
		out := stdout
		if out == "" {
			out = stderr
		}
		if out == "" {
			out = fmt.Sprintf("(unavailable: %v)", err)
		}
		last := ""
		if lines := strings.Split(strings.TrimSpace(stderr), "\n"); len(lines) > 0 {
			last = lines[len(lines)-1]
		}
		slog.Warn("context fetcher kubectl read failed", "cmd", strings.Join(args[:min(2, len(args))], " "), "err", err, "stderr", last)
		return false, clip(out, snapshotMaxChars), false
	}
	filtered, dropped := s.filterOutput(args, stdout)
	return true, capWithNotices(filtered, dropped), len([]rune(filtered)) <= snapshotMaxChars
}

// ReadText is for callers that render the output for a human. A failed read comes
// back as an explicit unavailable line rather than raw stderr, so an error is
// never fenced under a heading that claims it is pod state.
func (s *Snapshotter) ReadText(ctx context.Context, args []string) string {
	ok, text, _ := s.Read(ctx, args)
	if ok {
		return text
	}
	return policy.UnavailableMarker + " " + unavailableReason(text)
}

// unavailableReason is the one line worth showing a reader from a failed read.
func unavailableReason(text string) string {
	var last string
	for _, ln := range strings.Split(strings.TrimSpace(text), "\n") {
		if strings.TrimSpace(ln) != "" {
			last = strings.TrimSpace(ln)
		}
	}
	if last == "" {
		return "kubectl reported no reason"
	}
	return clip(last, 300)
}

// capWithNotices applies the length cap, then says what the cap and the filter
// each removed. Order is the whole point: both notices go after the slice, because
// a notice inside the slice can be sliced off, and these two say the text above
// them is incomplete.
func capWithNotices(text string, dropped int) string {
	var notices []string
	if dropped > 0 {
		notices = append(notices, nsguard.WithheldSentence(dropped, "row"))
	}
	if len([]rune(text)) <= snapshotMaxChars {
		if len(notices) == 0 {
			return text
		}
		return strings.TrimRight(text, "\n") + "\n" + strings.Join(notices, "\n") + "\n"
	}
	// Cut on a line boundary. A character slice ends mid row, and the fragment it
	// leaves still has enough columns to be parsed: "default app-1 1/1 Runni" was
	// counted as a pod whose STATUS is "Runni", so a listing of nothing but Running
	// pods reported an issue, fabricated out of a severed word.
	r := []rune(text)
	cut := string(r[:snapshotMaxChars])
	body := cut
	if i := strings.LastIndex(cut, "\n"); i > 0 {
		body = cut[:i]
	}
	body = strings.TrimRight(body, "\n")
	notices = append(notices, policy.TruncationMarker(len(r)-len([]rune(body)), "chars",
		"narrow the read with -n or -l; absence here is not evidence"))
	return body + "\n" + strings.Join(notices, "\n") + "\n"
}

// dataRowCount counts the rows of a kubectl table that are real resources: not
// the header and not policy text. A truncation marker is not a warning event, or
// a cut events listing would grow a warning out of the sentence saying it was cut.
func dataRowCount(table string) int {
	rows := 0
	for _, ln := range strings.Split(table, "\n") {
		if strings.TrimSpace(ln) != "" && !policy.LineRE.MatchString(ln) {
			rows++
		}
	}
	if rows == 0 {
		return 0
	}
	return rows - 1 // the first surviving line is the header
}

// ScanSnapshot returns (hasIssues, hasWarnings, podCount) by scanning kubectl
// output with a cheap line based parse. The pod table is
// NAMESPACE NAME READY STATUS RESTARTS AGE and STATUS is found by header position.
//
// podsOK and eventsOK say whether the command that produced each string worked.
// They are not optional in spirit: handed stderr as if it were a pod table, a one
// line error was consumed as the header (an unreachable cluster reported as an
// empty, healthy one) and a three line error had enough columns to be counted as
// pods (a quantity invented out of an error message).
func ScanSnapshot(pods, events string, podsOK, eventsOK bool) (hasIssues, hasWarnings bool, podCount int) {
	if !podsOK {
		// Nothing about the cluster is known; callers tell that apart from an empty
		// cluster by the flag they passed in, not by these values.
		return false, false, 0
	}
	statusIdx := -1
	for _, line := range strings.Split(pods, "\n") {
		stripped := strings.TrimSpace(line)
		// A policy sentence has enough columns to be read as a pod, and counting the
		// notice that rows were removed as one of the rows would be its own lie.
		if stripped == "" || policy.LineRE.MatchString(stripped) {
			continue
		}
		cols := strings.Fields(line)
		if statusIdx < 0 {
			statusIdx = 3 // the default layout
			for i, c := range cols {
				if strings.ToUpper(c) == "STATUS" {
					statusIdx = i
					break
				}
			}
			continue
		}
		if len(cols) <= statusIdx {
			continue
		}
		podCount++
		if !healthyPodStatuses[cols[statusIdx]] {
			hasIssues = true
		}
	}
	// A failed events read is not "no warnings", and its stderr is not a warning
	// list. Neither is a table whose only surviving lines are the header and the
	// policy notice: with every warning in kube-system filtered out, that is what is left.
	hasWarnings = eventsOK && dataRowCount(events) > 0 && !strings.Contains(events, "No resources found")
	return
}

// snapshotArgs are the two fixed cluster wide reads.
var (
	podsArgs   = []string{"get", "pods", "--all-namespaces"}
	eventsArgs = []string{"get", "events", "--all-namespaces", "--sort-by=.lastTimestamp", "--field-selector=type=Warning"}
)

// SnapshotResult is what fetching the snapshot produces.
type SnapshotResult struct {
	Text        string
	HasIssues   bool
	HasWarnings bool
	PodCount    int
	ReadFailed  bool
	Complete    bool
	PodsOut     string
	EventsOut   string
	PodsOK      bool
	EventsOK    bool
}

// Fetch reads pods and warning events in parallel and renders the snapshot.
func (s *Snapshotter) Fetch(ctx context.Context) SnapshotResult {
	var wg sync.WaitGroup
	var podsOK, eventsOK, podsComplete, eventsComplete bool
	var podsOut, eventsOut string
	wg.Add(2)
	go func() { defer wg.Done(); podsOK, podsOut, podsComplete = s.Read(ctx, podsArgs) }()
	go func() { defer wg.Done(); eventsOK, eventsOut, eventsComplete = s.Read(ctx, eventsArgs) }()
	wg.Wait()

	res := SnapshotResult{
		ReadFailed: !(podsOK && eventsOK), Complete: podsComplete && eventsComplete,
		PodsOut: podsOut, EventsOut: eventsOut, PodsOK: podsOK, EventsOK: eventsOK,
	}
	parts := []string{"## Cluster Snapshot"}
	if podsOK {
		parts = append(parts, "### Live Pod State\n```\n"+strings.TrimSpace(podsOut)+"\n```")
	} else {
		// Never print kubectl's stderr under a heading that claims it is pod state.
		parts = append(parts, "### Live Pod State\n**UNAVAILABLE - the cluster read failed.** kubectl said: `"+
			unavailableReason(podsOut)+"`\n\nThis is not zero pods; it is unknown. Do not answer questions about what is running from this snapshot.")
	}
	switch {
	case !eventsOK:
		parts = append(parts, "### Warning Events\n**UNAVAILABLE - the cluster read failed.** kubectl said: `"+
			unavailableReason(eventsOut)+"`\n\nThis is not an absence of warnings; it is unknown.")
	case strings.TrimSpace(eventsOut) == "" || strings.Contains(eventsOut, "No resources found"):
		parts = append(parts, "### Warning Events\n(none - cluster appears healthy)")
	default:
		parts = append(parts, "### Warning Events (most recent)\n```\n"+strings.TrimSpace(eventsOut)+"\n```")
	}
	res.Text = strings.Join(parts, "\n\n")
	res.HasIssues, res.HasWarnings, res.PodCount = ScanSnapshot(podsOut, eventsOut, podsOK, eventsOK)
	return res
}
