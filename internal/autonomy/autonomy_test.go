package autonomy

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/agent"
	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/detect"
)

func cfg(env map[string]string) *config.Config {
	return config.Load(func(k string) string { return env[k] })
}

func TestLevels(t *testing.T) {
	l := Ladder{cfg(map[string]string{"AUTONOMY_LEVEL": "A2", "AUTONOMY_NAMESPACE_LEVELS": "Prod=A0, dev = A3 ,bad=A9,noequals"})}
	cases := map[string]string{
		"shop": "A2", "prod": "A0", "PROD": "A0", " dev ": "A3", "bad": "A2",
		"kube-system": "A0", "Monitoring": "A0", "kube-sre": "A0",
		"": "A1", // cluster scoped is capped at investigate only
	}
	for ns, want := range cases {
		if got := l.LevelFor(ns); got != want {
			t.Errorf("%q: got %s want %s", ns, got, want)
		}
	}
	if got := (Ladder{cfg(map[string]string{"AUTONOMY_LEVEL": "A0"})}).LevelFor(""); got != "A0" {
		t.Errorf("a deployment pinned to A0 stays at A0 for cluster scoped objects: %s", got)
	}
	if got := (Ladder{cfg(map[string]string{"AUTONOMY_LEVEL": "A3"})}).LevelFor(""); got != "A1" {
		t.Errorf("%s", got)
	}
	if got := (Ladder{cfg(map[string]string{"AUTONOMY_LEVEL": "nonsense"})}).LevelFor("x"); got != "A1" {
		t.Errorf("an unknown default falls back to A1: %s", got)
	}
	if !AtLeast("A2", "A1") || AtLeast("A0", "A1") || !AtLeast("A3", "A3") {
		t.Error("ordering")
	}
}

func TestA3NeedsAnExplicitAllowlistEntry(t *testing.T) {
	l := Ladder{cfg(map[string]string{
		"AUTONOMY_LEVEL": "A3", "AUTONOMY_A3_ALLOWLIST": "CrashLoopBackOff/dev-*, ImagePullBackOff/staging,Evicted/*, malformed",
	})}
	yes := [][2]string{{"CrashLoopBackOff", "dev-team"}, {"CrashLoopBackOff", "DEV-x"}, {"ImagePullBackOff", "staging"}, {"Evicted", "anything"}}
	no := [][2]string{
		{"CrashLoopBackOff", "prod"}, {"ImagePullBackOff", "staging-2"}, {"OOMKilled", "dev-team"},
		{"Evicted", ""},             // a glob must not make cluster scoped objects auto fixable
		{"Evicted", "kube-system"},  // protected namespaces are pinned to A0
		{"CrashLoopBackOff", "dev"}, // the glob dev-* needs the dash
	}
	for _, c := range yes {
		if !l.A3Allowed(c[0], c[1]) {
			t.Errorf("%v should be allowed", c)
		}
	}
	for _, c := range no {
		if l.A3Allowed(c[0], c[1]) {
			t.Errorf("%v must not be allowed", c)
		}
	}
	notA3 := Ladder{cfg(map[string]string{"AUTONOMY_LEVEL": "A2", "AUTONOMY_A3_ALLOWLIST": "Evicted/*"})}
	if notA3.A3Allowed("Evicted", "dev") {
		t.Error("an allowlist entry alone is not enough: the namespace must be at A3")
	}
}

type recorder struct {
	mu   sync.Mutex
	reqs []agent.TurnRequest
	prep []string
}

func (r *recorder) invest(_ context.Context, q agent.TurnRequest) {
	r.mu.Lock()
	r.reqs = append(r.reqs, q)
	r.mu.Unlock()
}

func (r *recorder) prepare(s string) {
	r.mu.Lock()
	r.prep = append(r.prep, s)
	r.mu.Unlock()
}

func rig(env map[string]string) (*Watchtower, *recorder) {
	w := NewWatchtower(cfg(env))
	r := &recorder{}
	w.Investigate, w.Prepare = r.invest, r.prepare
	w.Start(context.Background())
	return w, r
}

func finding(id, playbook, ns, obj string) detect.Finding {
	return detect.Finding{ID: id, Playbook: playbook, Namespace: ns, Object: obj, Evidence: "pod status=X", Severity: "warning"}
}

func TestLevelDecidesWhetherAnythingRuns(t *testing.T) {
	w, r := rig(map[string]string{"AUTONOMY_LEVEL": "A1", "AUTONOMY_NAMESPACE_LEVELS": "quiet=A0"})
	w.OnFinding(finding("f1", "CrashLoopBackOff", "quiet", "p"))
	w.OnFinding(finding("f2", "CrashLoopBackOff", "kube-system", "p"))
	w.OnFinding(finding("f3", "CrashLoopBackOff", "shop", "p"))
	w.Wait()
	if len(r.reqs) != 1 || r.reqs[0].SessionID != "auto-f3" {
		t.Fatalf("A0 and protected namespaces investigate nothing: %+v", r.reqs)
	}
	q := r.reqs[0]
	if q.UserID != "watchtower" || q.UserRole != "operator" || q.AutoApprove || q.TriggerSource != "detector" {
		t.Errorf("%+v", q)
	}
	if len(r.prep) != 1 || r.prep[0] != "auto-f3" {
		t.Errorf("%v", r.prep)
	}
	if q.Message == "" || !contains(q.Message, "triggered by detector CrashLoopBackOff") || contains(q.Message, "Propose") || contains(q.Message, "apply the appropriate fix") {
		t.Errorf("A1 reports only: %s", q.Message)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestDisabledWatchtowerDoesNothing(t *testing.T) {
	w, r := rig(map[string]string{"WATCHTOWER_ENABLED": "false"})
	w.OnFinding(finding("f", "Evicted", "shop", "p"))
	w.Wait()
	if len(r.reqs) != 0 {
		t.Error("switched off")
	}
}

func TestA2ProposesAndA3FixesOnlyAllowlisted(t *testing.T) {
	w, r := rig(map[string]string{"AUTONOMY_LEVEL": "A2"})
	w.OnFinding(finding("a", "Evicted", "shop", "p"))
	w.Wait()
	if q := r.reqs[0]; q.AutoApprove || !contains(q.Message, "Propose the exact fix commands") {
		t.Errorf("A2 proposes: %+v", q)
	}

	w, r = rig(map[string]string{"AUTONOMY_LEVEL": "A3", "AUTONOMY_A3_ALLOWLIST": "Evicted/shop"})
	var fixed atomic.Int32
	w.AfterFix = func(detect.Finding) { fixed.Add(1) }
	w.OnFinding(finding("b", "Evicted", "shop", "p"))
	w.OnFinding(finding("c", "OOMKilled", "shop", "p"))
	w.Wait()
	byID := map[string]agent.TurnRequest{}
	for _, q := range r.reqs {
		byID[q.SessionID] = q
	}
	if !byID["auto-b"].AutoApprove || !contains(byID["auto-b"].Message, "apply the appropriate fix and verify") {
		t.Errorf("allowlisted: %+v", byID["auto-b"])
	}
	if byID["auto-c"].AutoApprove || !contains(byID["auto-c"].Message, "Propose the exact fix") {
		t.Errorf("A3 but not allowlisted falls back to proposing: %+v", byID["auto-c"])
	}
	if fixed.Load() != 1 {
		t.Errorf("only the auto fix schedules a re check: %d", fixed.Load())
	}
}

func TestAPredictedFindingNeverDrivesAWrite(t *testing.T) {
	w, r := rig(map[string]string{"AUTONOMY_LEVEL": "A3", "AUTONOMY_A3_ALLOWLIST": "OOMKilled/*"})
	eta := 12.5
	f := finding("p", "OOMKilled", "shop", "web")
	f.Severity, f.ETAMinutes = "predicted", &eta
	w.OnFinding(f)
	w.Wait()
	q := r.reqs[0]
	if q.AutoApprove || !contains(q.Message, "PREDICTED failure OOMKilled") || !contains(q.Message, "~12.5m") || !contains(q.Message, "Do NOT execute destructive fixes") {
		t.Errorf("%+v", q)
	}
}

func TestBrakesCanCloseTheGateButNeverOpenIt(t *testing.T) {
	env := map[string]string{"AUTONOMY_LEVEL": "A3", "AUTONOMY_A3_ALLOWLIST": "Evicted/*"}
	w, r := rig(env)
	w.AutoWritePermitted = func() (bool, string) { return false, "change freeze" }
	w.OnFinding(finding("a", "Evicted", "shop", "p"))
	w.Wait()
	if r.reqs[0].AutoApprove {
		t.Error("the kill switch or a freeze denies the write")
	}
	w, r = rig(env)
	w.AutofixRevoked = func(context.Context) string { return "agreement collapsed" }
	w.OnFinding(finding("a", "Evicted", "shop", "p"))
	w.Wait()
	if r.reqs[0].AutoApprove {
		t.Error("a revoked record denies the write")
	}
	w, r = rig(env)
	w.AutoWritePermitted = func() (bool, string) { return true, "" }
	w.AutofixRevoked = func(context.Context) string { return "" }
	w.OnFinding(finding("a", "Evicted", "shop", "p"))
	w.Wait()
	if !r.reqs[0].AutoApprove {
		t.Error("brakes that allow leave the allowlist in charge")
	}
	w, r = rig(map[string]string{"AUTONOMY_LEVEL": "A1"})
	w.AutoWritePermitted = func() (bool, string) { return true, "" }
	w.OnFinding(finding("a", "Evicted", "shop", "p"))
	w.Wait()
	if r.reqs[0].AutoApprove {
		t.Error("a permissive brake never grants write authority")
	}
}

func TestCooldownPerObjectAndCap(t *testing.T) {
	w, r := rig(nil)
	now := time.Now()
	w.Now = func() time.Time { return now }
	w.OnFinding(finding("1", "CrashLoopBackOff", "shop", "web"))
	w.OnFinding(finding("2", "CrashLoopBackOff", "shop", "web"))
	w.OnFinding(finding("3", "CrashLoopBackOff", "shop", "api"))
	w.OnFinding(finding("4", "Evicted", "shop", "web"))
	w.Wait()
	if len(r.reqs) != 3 {
		t.Fatalf("a flapping object investigates once, other objects and playbooks are independent: %d", len(r.reqs))
	}
	now = now.Add(31 * time.Minute)
	w.OnFinding(finding("5", "CrashLoopBackOff", "shop", "web"))
	w.Wait()
	if len(r.reqs) != 4 {
		t.Error("the cooldown ends")
	}
	w.ResetCooldowns()
	w.OnFinding(finding("6", "CrashLoopBackOff", "shop", "web"))
	w.Wait()
	if len(r.reqs) != 5 {
		t.Error("reset")
	}
}

func TestConcurrencyIsCapped(t *testing.T) {
	w := NewWatchtower(cfg(nil))
	var cur, peak atomic.Int32
	release := make(chan struct{})
	w.Investigate = func(context.Context, agent.TurnRequest) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		cur.Add(-1)
	}
	w.Start(context.Background())
	for i := 0; i < 6; i++ {
		w.OnFinding(finding(string(rune('a'+i)), "CrashLoopBackOff", "shop", string(rune('a'+i))))
	}
	time.Sleep(200 * time.Millisecond)
	if p := peak.Load(); p != 2 {
		t.Errorf("at most %d at once, got %d", maxConcurrent, p)
	}
	close(release)
	w.Wait()
}

func TestAPanickingInvestigationIsContained(t *testing.T) {
	w := NewWatchtower(cfg(nil))
	w.Investigate = func(context.Context, agent.TurnRequest) { panic("boom") }
	w.Start(context.Background())
	w.OnFinding(finding("a", "Evicted", "shop", "p"))
	w.Wait()
	w.Investigate = func(context.Context, agent.TurnRequest) {}
	w.OnFinding(finding("b", "Evicted", "shop", "q"))
	w.Wait()
}
