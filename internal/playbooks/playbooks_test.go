package playbooks

import (
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/detect"
	"github.com/DinethShakya23/kube-sre/internal/sensorium"
)

func TestAllPlaybooksLoad(t *testing.T) {
	r := Load()
	if n := len(r.List()); n != 27 {
		t.Fatalf("want 27 playbooks, got %d", n)
	}
	compiled := 0
	for _, pb := range r.List() {
		if pb.Detect != nil {
			compiled++
		}
	}
	if compiled != 20 || len(r.Detectors()) != 20 {
		t.Errorf("want 20 compiled detectors, got %d", compiled)
	}
}

func TestEveryPlaybookIsReachable(t *testing.T) {
	for _, pb := range Load().List() {
		live := pb.Detect != nil
		for _, tr := range pb.Triggers {
			if tr.PodStatus != nil || tr.EventReason != nil || tr.EventMessage != nil {
				live = true
			}
		}
		if !live {
			t.Errorf("%s has no usable trigger and no detector, so nothing can ever select it", pb.Name)
		}
		if len(pb.InvestigationSteps) == 0 || len(pb.ExpectedEvidence) == 0 || pb.FixTemplate == "" {
			t.Errorf("%s is missing steps, evidence or a fix template", pb.Name)
		}
	}
}

func TestMatch(t *testing.T) {
	r := Load()
	has := func(got []string, want string) bool {
		for _, g := range got {
			if g == want {
				return true
			}
		}
		return false
	}
	got := r.Match("web-1  0/1  CrashLoopBackOff  4", "")
	if !has(got, "CrashLoopBackOff") || !has(got, "CommandHardcodedFailure") {
		t.Errorf("pods snapshot: %v", got)
	}
	got = r.Match("", "Warning Unhealthy pod/web Readiness probe failed: HTTP probe failed")
	if !has(got, "ReadinessProbeFailing") || has(got, "LivenessProbeFailing") {
		t.Errorf("readiness must not select liveness: %v", got)
	}
	got = r.Match("", "0/3 nodes are available: 3 Insufficient memory")
	if !has(got, "PendingInsufficientResources") {
		t.Errorf("insufficient: %v", got)
	}
	if got := r.Match("web-1 1/1 Running 0", "Normal Scheduled pod/web assigned"); len(got) != 0 {
		t.Errorf("healthy snapshot matched %v", got)
	}
}

func obsPod(status string) sensorium.Observation {
	return sensorium.Observation{Kind: "pod_status", Namespace: "ns", Name: "p", TS: time.Now(),
		Fields: map[string]any{"status": status, "watch_type": "MODIFIED"}}
}

func obsEvent(kind, reason, msg string) sensorium.Observation {
	return sensorium.Observation{Kind: "event", Namespace: "ns", Name: "x", TS: time.Now(),
		Fields: map[string]any{"reason": reason, "message": msg, "involved_kind": kind, "event_type": "Warning"}}
}

// firesFor feeds an observation to one playbook's detector only.
func firesFor(t *testing.T, name string, o sensorium.Observation) bool {
	t.Helper()
	pb := Load().Get(name)
	if pb == nil || pb.Detect == nil {
		t.Fatalf("%s has no detector", name)
	}
	e := detect.NewEngine("c", []detect.DetectBlock{*pb.Detect})
	return len(e.Process(o))+len(e.Tick(o.TS.Add(24*time.Hour))) > 0
}

func TestDetectorsFireOnTheirSignal(t *testing.T) {
	cases := []struct {
		playbook string
		obs      sensorium.Observation
	}{
		{"CrashLoopBackOff", obsPod("CrashLoopBackOff")},
		{"OOMKilled", obsPod("OOMKilled")},
		{"OOMKilled", obsEvent("Node", "OOMKilling", "Killed process")},
		{"ImagePullBackOff", obsPod("ErrImagePull")},
		{"ImagePullBackOff", obsEvent("Pod", "Failed", "Failed to pull image \"x\"")},
		{"InitContainerFailing", obsPod("Init:CrashLoopBackOff")},
		{"InitContainerFailing", obsPod("Init:1/2")},
		{"CreateContainerConfigError", obsPod("CreateContainerConfigError")},
		{"ContainerCreatingStuck", obsPod("ContainerCreating")},
		{"Evicted", obsPod("Evicted")},
		{"TerminatingStuck", obsPod("Terminating")},
		{"LivenessProbeFailing", obsEvent("Pod", "Unhealthy", "Liveness probe failed: timeout")},
		{"ReadinessProbeFailing", obsEvent("Pod", "Unhealthy", "Readiness probe failed: 503")},
		{"PendingInsufficientResources", obsEvent("Pod", "FailedScheduling", "0/3 nodes: Insufficient cpu")},
		{"PendingSchedulingConstraints", obsEvent("Pod", "FailedScheduling", "node(s) had untolerated taint")},
		{"PvcPending", obsEvent("PersistentVolumeClaim", "ProvisioningFailed", "storageclass not found")},
		{"QuotaExceeded", obsEvent("ReplicaSet", "FailedCreate", "exceeded quota: q")},
		{"WebhookAdmissionRejected", obsEvent("ReplicaSet", "FailedCreate", "admission webhook \"x\" denied the request")},
		{"JobBackoffLimitExceeded", obsEvent("Job", "BackoffLimitExceeded", "Job has reached the specified backoff limit")},
		{"DeploymentRolloutStuck", obsEvent("Deployment", "ProgressDeadlineExceeded", "exceeded")},
		{"HPANotScaling", obsEvent("HorizontalPodAutoscaler", "FailedGetResourceMetric", "no metrics")},
		{"ServiceNoEndpoints", obsEvent("Service", "FailedToUpdateEndpoint", "x")},
		{"NodeNotReady", obsEvent("Node", "NodeNotReady", "Node is not ready")},
	}
	for _, c := range cases {
		if !firesFor(t, c.playbook, c.obs) {
			t.Errorf("%s did not fire on %s %v", c.playbook, c.obs.Kind, c.obs.Fields)
		}
	}
}

func TestDetectorsStayQuietOnHealthySignals(t *testing.T) {
	for _, pb := range Load().List() {
		if pb.Detect == nil {
			continue
		}
		e := detect.NewEngine("c", []detect.DetectBlock{*pb.Detect})
		for _, o := range []sensorium.Observation{
			obsPod("Running"), obsPod("Completed"), obsPod("Pending"),
			{Kind: "event", Namespace: "ns", Name: "p", TS: time.Now(),
				Fields: map[string]any{"reason": "Started", "message": "Started container", "involved_kind": "Pod", "event_type": "Normal"}},
		} {
			if f := e.Process(o); len(f) != 0 {
				t.Errorf("%s fired on a healthy signal %v", pb.Name, o.Fields)
			}
		}
	}
}

func TestLivenessAndReadinessDoNotDoubleFire(t *testing.T) {
	ev := obsEvent("Pod", "Unhealthy", "Readiness probe failed: 503")
	if firesFor(t, "LivenessProbeFailing", ev) {
		t.Error("liveness detector fired on a readiness failure")
	}
	ev = obsEvent("Pod", "Unhealthy", "Liveness probe failed: timeout")
	if firesFor(t, "ReadinessProbeFailing", ev) {
		t.Error("readiness detector fired on a liveness failure")
	}
}

func TestOOMKilledCarriesATrendPredicate(t *testing.T) {
	d := Load().Get("OOMKilled").Detect
	if len(d.TrendPredicates) != 1 {
		t.Fatalf("want one trend predicate, got %d", len(d.TrendPredicates))
	}
	tp := d.TrendPredicates[0]
	if tp.Threshold != 1.0 || tp.MinR2 != 0.6 || tp.WindowMinutes != 30 || tp.ProjectionHorizonMin != 60 {
		t.Errorf("%+v", tp)
	}
}

func TestEveryShippedDetectorIsLiveAndHealthy(t *testing.T) {
	for _, d := range Load().Detectors() {
		for _, p := range d.WatchPredicates {
			errs, err := detect.PredicateLivenessErrors(p, false)
			if len(errs) > 0 || err != nil {
				t.Errorf("%s can never fire: %v %v", d.Playbook, errs, err)
			}
			if errs := detect.PredicateHealthErrors(p); len(errs) > 0 {
				t.Errorf("%s fires on healthy objects: %v", d.Playbook, errs)
			}
		}
		for _, tp := range d.TrendPredicates {
			if errs := detect.TrendLivenessErrors(tp); len(errs) > 0 {
				t.Errorf("%s: %v", d.Playbook, errs)
			}
		}
	}
}
