package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/change"
	"github.com/DinethShakya23/kube-sre/internal/llm"
)

var v5On = map[string]string{"CORTEX_V5_ENABLED": "true", "KI_V5_CHANGE_LEDGER": "true", "KI_V5_CHANGE_FIRST_RCA": "true", "KI_V5_INVESTIGATION_WRITEBACK": "true"}

func TestMutationsAreRecordedInTheChangeLedgerOnlyWhenEnabled(t *testing.T) {
	for _, on := range []bool{true, false} {
		env := map[string]string{}
		if on {
			env = v5On
		}
		r := newRig(t, okKubectl, env)
		r.a.Changes = change.NewLedger()
		r.coord.replies = []llm.Message{toolCall("c1", ToolKubectl, map[string]any{"command": "scale deployment web --replicas=2 -n shop"}), say("done")}
		r.turn(t, "scale web", func(q *TurnRequest) { q.AutoApprove = true })
		var got []change.Record
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && on && len(got) == 0 {
			got = r.a.Changes.Recent("test-cluster", "")
			time.Sleep(20 * time.Millisecond)
		}
		got = r.a.Changes.Recent("test-cluster", "")
		switch {
		case on && (len(got) != 1 || got[0].Kind != "scale" || got[0].Target != "deployment"):
			t.Errorf("enabled: %+v", got)
		case !on && len(got) != 0:
			t.Errorf("flag off must record nothing: %+v", got)
		}
	}
}

func TestChangeFirstPriorReachesTheCoordinatorPrompt(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want bool
	}{{v5On, true}, {map[string]string{"KI_V5_CHANGE_FIRST_RCA": "true"}, false}, {nil, false}} {
		r := newRig(t, okKubectl, tc.env)
		r.a.Changes = change.NewLedger()
		r.a.Changes.Add("test-cluster", change.Record{Kind: "image", Target: "deploy/api", TS: 1, Detail: "kubectl set image deploy/api api=v3"})
		r.coord.replies = []llm.Message{say("ok")}
		r.turn(t, "what changed?")
		got := strings.Contains(r.coord.seen[0][0].Content, "Recent changes (consider these FIRST")
		if got != tc.want {
			t.Errorf("env %v: prior present=%v", tc.env, got)
		}
		if tc.want && !strings.Contains(r.coord.seen[0][0].Content, "- image deploy/api") {
			t.Error("the change itself must be listed")
		}
	}
}

const crashKubectl = `case "$1 $2" in
"get pods") printf 'NAMESPACE  NAME  READY  STATUS  RESTARTS  AGE\nshop  web-1  0/1  CrashLoopBackOff  9  1d\n';;
"get events") echo "No resources found";;
*) echo done;;
esac`

func TestInvestigationWritebackFeedsTheMatchedPlaybooksBack(t *testing.T) {
	var mu sync.Mutex
	var gotCluster string
	var gotPlaybooks []string
	for _, on := range []bool{true, false} {
		env := map[string]string{}
		if on {
			env = v5On
		}
		r := newRig(t, crashKubectl, env)
		r.a.Writeback = func(_ context.Context, cluster string, playbooks []string) {
			mu.Lock()
			gotCluster, gotPlaybooks = cluster, playbooks
			mu.Unlock()
		}
		mu.Lock()
		gotCluster, gotPlaybooks = "", nil
		mu.Unlock()
		r.coord.replies = []llm.Message{
			say("RCA_REQUIRED"),
			say(`{"root_cause":"bad image","confidence":0.9,"supporting_evidence":["a"],"reasoning":"r","recommended_fix":"fix","affected_domain":["pod"]}`),
		}
		r.sub.next = func(msgs []llm.Message) *llm.Message {
			return &llm.Message{Content: "```json\n{\"domain\":\"pod\",\"signals\":[\"s\"],\"hypothesis\":\"h\",\"confidence\":0.8,\"evidence\":[\"e\"]}\n```"}
		}
		r.turn(t, "why is web crashing")
		time.Sleep(300 * time.Millisecond)
		mu.Lock()
		c, p := gotCluster, gotPlaybooks
		mu.Unlock()
		switch {
		case on && (c != "test-cluster" || len(p) == 0):
			t.Errorf("enabled: %q %v", c, p)
		case !on && (c != "" || p != nil):
			t.Errorf("flag off must not write back: %q %v", c, p)
		}
	}
}
