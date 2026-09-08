package detect

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

func samples(t *testing.T, pat string) []string {
	t.Helper()
	got, err := EnumerateSamples(regexp.MustCompile(pat))
	if err != nil {
		t.Fatalf("%s: %v", pat, err)
	}
	sort.Strings(got)
	return got
}

func TestEnumerateEveryBranch(t *testing.T) {
	if got := strings.Join(samples(t, `^(OOMKilled|Error)$`), ","); got != "Error,OOMKilled" {
		t.Errorf("%s", got)
	}
	if got := strings.Join(samples(t, `^Failed(Get|Compute)Metric$`), ","); got != "FailedComputeMetric,FailedGetMetric" {
		t.Errorf("%s", got)
	}
	if got := strings.Join(samples(t, `^Init:(Error|CrashLoopBackOff)$`), ","); got != "Init:CrashLoopBackOff,Init:Error" {
		t.Errorf("%s", got)
	}
	if got := samples(t, `^BackOff?$`); len(got) != 2 {
		t.Errorf("optional part: %v", got)
	}
}

func TestEnumerateRefusesWhatItCannotExpand(t *testing.T) {
	big := regexp.MustCompile(`^[a-c][a-c][a-c][a-c][a-c][a-c][a-c][a-c][a-c][a-c]$`)
	if _, err := EnumerateSamples(big); err == nil {
		t.Error("a huge language should be refused, not truncated")
	}
}

func pred(kind, status, reason string) WatchPredicate {
	p := WatchPredicate{Kind: kind}
	if status != "" {
		p.StatusRegex = regexp.MustCompile(status)
	}
	if reason != "" {
		p.ReasonRegex = regexp.MustCompile(reason)
	}
	return p
}

func TestAStraySpaceMakesAPredicateDead(t *testing.T) {
	errs, err := PredicateLivenessErrors(pred("Event", "", `^(FailedGetResourceMetric | FailedComputeMetricsReplicas)$`), false)
	if err != nil || len(errs) != 1 || !strings.Contains(errs[0], "can never fire") || !strings.Contains(errs[0], "reason") {
		t.Errorf("%v %v", errs, err)
	}
	if errs, _ := PredicateLivenessErrors(pred("Event", "", `^(FailedGetResourceMetric|FailedComputeMetricsReplicas)$`), false); len(errs) != 0 {
		t.Errorf("a live predicate: %v", errs)
	}
}

func TestKindAndMissingStatusAreDead(t *testing.T) {
	if errs, _ := PredicateLivenessErrors(pred("pod", "^OOMKilled$", ""), false); len(errs) != 1 || !strings.Contains(errs[0], "case-sensitively") {
		t.Errorf("wrong case: %v", errs)
	}
	if errs, _ := PredicateLivenessErrors(pred("Deployment", "^x$", ""), false); len(errs) != 1 {
		t.Errorf("unknown kind: %v", errs)
	}
	if errs, _ := PredicateLivenessErrors(pred("Pod", "", ""), false); len(errs) != 1 || !strings.Contains(errs[0], "without status_regex") {
		t.Errorf("no status: %v", errs)
	}
	if errs, _ := PredicateLivenessErrors(pred("Node", "", ""), false); len(errs) != 1 {
		t.Errorf("node without status: %v", errs)
	}
}

func TestExoticPatternsAreUnknownUnlessStrict(t *testing.T) {
	p := pred("Pod", `^[a-c][a-c][a-c][a-c][a-c][a-c][a-c][a-c][a-c][a-c]$`, "")
	if errs, err := PredicateLivenessErrors(p, false); len(errs) != 0 || err != nil {
		t.Errorf("lenient: %v %v", errs, err)
	}
	if _, err := PredicateLivenessErrors(p, true); err == nil {
		t.Error("strict should surface it")
	}
}

func TestPredicatesMatchingHealthyObjectsAreRefused(t *testing.T) {
	if errs := PredicateHealthErrors(pred("Pod", "^Running$", "")); len(errs) != 1 || !strings.Contains(errs[0], "HEALTHY Pod") || !strings.Contains(errs[0], "every pod") {
		t.Errorf("%v", errs)
	}
	if errs := PredicateHealthErrors(pred("Pod", "Completed|Failed", "")); len(errs) != 1 {
		t.Errorf("alternation with a healthy arm: %v", errs)
	}
	if errs := PredicateHealthErrors(pred("Node", "Ready", "")); len(errs) != 1 {
		t.Errorf("node: %v", errs)
	}
	for _, ok := range []WatchPredicate{pred("Pod", "^OOMKilled$", ""), pred("Node", "NotReady", ""), pred("Event", "", "^BackOff$")} {
		if errs := PredicateHealthErrors(ok); len(errs) != 0 {
			t.Errorf("%v: %v", ok.Kind, errs)
		}
	}
}

func trend(metric string, mods ...func(*TrendPredicate)) TrendPredicate {
	t := TrendPredicate{Metric: metric, Threshold: 90, WindowMinutes: 30, ProjectionHorizonMin: 120, FireIfETAWithinMinutes: 30, Direction: "rising", MinR2: 0.5}
	for _, m := range mods {
		m(&t)
	}
	return t
}

func TestTrendPlaceholdersAndImpossibleKnobs(t *testing.T) {
	for _, m := range []string{
		`kube_deployment_status_replicas{deployment="your-deployment-name"}`,
		`x{deployment="your_service_name"}`, `x{ns="<namespace>"}`, `x{ns="{{ ns }}"}`, `x{ns="$NAMESPACE"}`, `x{ns="CHANGEME"}`,
	} {
		if errs := TrendLivenessErrors(trend(m)); len(errs) != 1 || !strings.Contains(errs[0], "unfilled template") {
			t.Errorf("%s: %v", m, errs)
		}
	}
	for _, m := range []string{`x{deployment="test"}`, `x{deployment="example"}`, `x{deployment="payments"}`, `x`} {
		if errs := TrendLivenessErrors(trend(m)); len(errs) != 0 {
			t.Errorf("ordinary name refused %s: %v", m, errs)
		}
	}
	cases := map[string]func(*TrendPredicate){
		"min_r2=1.5":                    func(p *TrendPredicate) { p.MinR2 = 1.5 },
		"fire_if_eta_within_minutes=0":  func(p *TrendPredicate) { p.FireIfETAWithinMinutes = 0 },
		"projection_horizon_minutes=-1": func(p *TrendPredicate) { p.ProjectionHorizonMin = -1 },
		"window_minutes=0":              func(p *TrendPredicate) { p.WindowMinutes = 0 },
		`direction="up"`:                func(p *TrendPredicate) { p.Direction = "up" },
	}
	for want, m := range cases {
		if errs := TrendLivenessErrors(trend("x", m)); len(errs) != 1 || !strings.Contains(errs[0], want) {
			t.Errorf("%s: %v", want, errs)
		}
	}
	if errs := TrendLivenessErrors(trend("  ")); len(errs) != 1 || !strings.Contains(errs[0], "no metric") {
		t.Errorf("%v", errs)
	}
	if errs := TrendLivenessErrors(trend("x", func(p *TrendPredicate) { p.MinR2 = 0 })); len(errs) != 0 {
		t.Errorf("min_r2 of 0 is a real setting: %v", errs)
	}
}
