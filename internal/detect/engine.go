package detect

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/sensorium"
)

const (
	eventArmTTL        = 600 * time.Second
	tickInterval       = time.Second
	trendInterval      = 60 * time.Second
	predictedRefireTTL = 1800 * time.Second
	findingsRingSize   = 500
	withheldMessage    = "<withheld: protected namespace>"
	minTrendSamples    = 3
)

// Recorder receives findings for the flight recorder.
type Recorder interface {
	Record(episode, kind string, payload map[string]any)
}

// Series is one range-query result: labels plus (unix seconds, value) points.
type Series struct {
	Metric map[string]string
	Values [][2]float64
}

// SeriesSource fetches range series for trend predicates (Prometheus).
type SeriesSource interface {
	QuerySeries(ctx context.Context, promql string, windowMinutes int) ([]Series, error)
}

type key struct{ playbook, namespace, name string }

type keyState struct {
	armedAt   float64
	lastMatch float64
	fired     bool
	evidence  string
}

// Engine evaluates compiled predicates against the observation stream.
//
//   - Debounce: a match arms (playbook, namespace, object); it fires only if
//     still armed after DebounceSeconds. Zero fires at once.
//   - Transition dedup: a fired key does not re-fire while the condition
//     holds. A pod or node status that stops matching clears the key. Event
//     keys clear after a TTL without a new match, since events never "recover".
//   - Nothing here calls an LLM.
type Engine struct {
	Detectors []DetectBlock
	// Shadow detectors are candidates. They accrue precision but never reach
	// OnFinding, so they cannot act.
	Shadow    []DetectBlock
	ClusterID string
	OnFinding func(Finding)
	Recorder  Recorder
	Series    SeriesSource
	// Blocked namespaces have event text withheld from finding evidence.
	Blocked map[string]bool

	mu             sync.Mutex
	states         map[key]*keyState
	shadowStates   map[key]*keyState
	predictedFired map[key]float64
	findings       []Finding
	shadowFindings []Finding
	blindSince     float64
	lastTrendError string
}

func NewEngine(clusterID string, detectors []DetectBlock) *Engine {
	return &Engine{
		Detectors: detectors, ClusterID: clusterID,
		states: map[key]*keyState{}, shadowStates: map[key]*keyState{}, predictedFired: map[key]float64{},
	}
}

// Process feeds one observation and returns any findings fired right away.
func (e *Engine) Process(o sensorium.Observation) []Finding {
	e.mu.Lock()
	var fired []Finding
	for i := range e.Detectors {
		det := &e.Detectors[i]
		if st := e.advance(e.states, det, o); st != nil {
			fired = append(fired, e.fire(det, o.Namespace, o.Name, st, "watch"))
		}
	}
	for i := range e.Shadow {
		det := &e.Shadow[i]
		if st := e.advance(e.shadowStates, det, o); st != nil {
			e.emitShadow(det, o.Namespace, o.Name, st)
		}
	}
	e.mu.Unlock()
	e.notify(fired)
	return fired
}

// advance updates the state for (det, object) and returns the state when the
// detector should fire now.
func (e *Engine) advance(states map[key]*keyState, det *DetectBlock, o sensorium.Observation) *keyState {
	now := secs(o.TS)
	k := key{det.Playbook, o.Namespace, o.Name}
	st := states[k]
	stateful := det.hasStatusKind()

	// A DELETED event proves the object is gone, so its condition can no
	// longer be stuck. Disarming here keeps a normal termination (whose final
	// object still reads Terminating) from firing.
	if o.Kind == "pod_status" && o.Str("watch_type") == "DELETED" {
		if st != nil && stateful {
			delete(states, k)
		}
		return nil
	}
	if det.matches(o) {
		if st == nil {
			st = &keyState{armedAt: now, lastMatch: now, evidence: e.summarise(o)}
			states[k] = st
		} else {
			st.lastMatch = now
		}
		if !st.fired && now-st.armedAt >= float64(det.DebounceSeconds) {
			return st
		}
		return nil
	}
	// Only pod and node status are continuous; a non-matching event says
	// nothing about recovery.
	if st != nil && o.Kind == "pod_status" && stateful {
		delete(states, k)
	}
	return nil
}

func (d *DetectBlock) matches(o sensorium.Observation) bool {
	for _, p := range d.WatchPredicates {
		if p.Matches(o) {
			return true
		}
	}
	return false
}

func (d *DetectBlock) hasStatusKind() bool {
	for _, p := range d.WatchPredicates {
		if p.Kind == "Pod" || p.Kind == "Node" {
			return true
		}
	}
	return false
}

// Tick fires armed keys whose debounce has passed and expires stale ones.
func (e *Engine) Tick(now time.Time) []Finding {
	e.mu.Lock()
	t := secs(now)
	var fired []Finding
	for k, st := range e.states {
		det := e.byName(e.Detectors, k.playbook)
		if det == nil {
			delete(e.states, k)
			continue
		}
		if !st.fired && t-st.armedAt >= float64(det.DebounceSeconds) {
			fired = append(fired, e.fire(det, k.namespace, k.name, st, "watch"))
		} else if st.fired && t-st.lastMatch > eventArmTTL.Seconds() {
			delete(e.states, k)
		}
	}
	for k, st := range e.shadowStates {
		det := e.byName(e.Shadow, k.playbook)
		if det == nil {
			delete(e.shadowStates, k)
			continue
		}
		if !st.fired && t-st.armedAt >= float64(det.DebounceSeconds) {
			e.emitShadow(det, k.namespace, k.name, st)
		}
	}
	e.mu.Unlock()
	e.notify(fired)
	return fired
}

func (e *Engine) byName(set []DetectBlock, playbook string) *DetectBlock {
	for i := range set {
		if set[i].Playbook == playbook {
			return &set[i]
		}
	}
	return nil
}

func (e *Engine) fire(det *DetectBlock, ns, name string, st *keyState, source string) Finding {
	st.fired = true
	f := Finding{
		ID: newFindingID(), Playbook: det.Playbook, ClusterID: e.ClusterID,
		Namespace: ns, Object: name, Evidence: st.evidence,
		FirstSeen: st.armedAt, FiredAt: secs(time.Now()), Source: source, Severity: "warning",
	}
	e.publish(f)
	return f
}

// publish records a finding in the ring and the flight recorder. Callers hold e.mu.
func (e *Engine) publish(f Finding) {
	e.findings = appendRing(e.findings, f)
	if e.Recorder != nil {
		e.Recorder.Record("findings:"+e.ClusterID, "finding", f.Dict())
	}
	slog.Info("detector fired", "playbook", f.Playbook, "ns", f.Namespace, "object", f.Object,
		"severity", f.Severity, "evidence", clip(f.Evidence, 120))
}

// emitShadow never calls OnFinding: shadow detectors cannot act.
func (e *Engine) emitShadow(det *DetectBlock, ns, name string, st *keyState) {
	st.fired = true
	f := Finding{
		ID: newFindingID(), Playbook: det.Playbook, ClusterID: e.ClusterID,
		Namespace: ns, Object: name, Evidence: st.evidence,
		FirstSeen: st.armedAt, FiredAt: secs(time.Now()), Source: "shadow", Severity: "warning",
	}
	e.shadowFindings = appendRing(e.shadowFindings, f)
	if e.Recorder != nil {
		e.Recorder.Record("shadow-findings:"+e.ClusterID, "finding", f.Dict())
	}
	slog.Info("shadow detector fired", "playbook", det.Playbook, "ns", ns, "object", name)
}

// notify calls OnFinding outside the lock so a slow or re-entrant callback
// cannot stall or deadlock the engine.
func (e *Engine) notify(fs []Finding) {
	if e.OnFinding == nil {
		return
	}
	for _, f := range fs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Warn("on_finding callback panicked", "err", r)
				}
			}()
			e.OnFinding(f)
		}()
	}
}

func appendRing(ring []Finding, f Finding) []Finding {
	ring = append(ring, f)
	if len(ring) > findingsRingSize {
		ring = ring[len(ring)-findingsRingSize:]
	}
	return ring
}

// RecentFindings returns up to limit findings fired at or after since.
func (e *Engine) RecentFindings(limit int, since float64) []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []map[string]any
	for _, f := range e.findings {
		if f.FiredAt >= since {
			out = append(out, f.Dict())
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func (e *Engine) ShadowFindings() []Finding {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Finding(nil), e.shadowFindings...)
}

// Predictive reports whether trend detection can see its data. blind means
// the last sweep failed, so "no prediction" is absence of evidence.
func (e *Engine) Predictive() (blind bool, since float64, lastError string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.blindSince != 0, e.blindSince, e.lastTrendError
}

// EvaluateTrends projects each trend predicate and fires "predicted" findings.
// It fails open (an outage yields no findings and no panic) but not silent: the
// outage is remembered so the API can say the predictive layer is blind.
func (e *Engine) EvaluateTrends(ctx context.Context, now time.Time) []Finding {
	t := secs(now)
	var fired []Finding
	for i := range e.Detectors {
		det := &e.Detectors[i]
		for _, tp := range det.TrendPredicates {
			var series []Series
			var err error
			if e.Series == nil {
				err = fmt.Errorf("no metrics source is configured")
			} else {
				series, err = e.Series.QuerySeries(ctx, tp.Metric, tp.WindowMinutes)
			}
			e.mu.Lock()
			if err != nil {
				if e.blindSince == 0 {
					e.blindSince = t
				}
				e.lastTrendError = err.Error()
				e.mu.Unlock()
				slog.Warn("trend query unavailable", "playbook", det.Playbook, "metric", clip(tp.Metric, 80), "err", err)
				continue
			}
			e.blindSince, e.lastTrendError = 0, ""
			for _, s := range series {
				if f := e.projectSeries(det, tp, s, t); f != nil {
					fired = append(fired, *f)
				}
			}
			e.mu.Unlock()
		}
	}
	e.notify(fired)
	return fired
}

// projectSeries must be called with e.mu held.
func (e *Engine) projectSeries(det *DetectBlock, tp TrendPredicate, s Series, now float64) *Finding {
	samples := cleanSamples(s.Values)
	if len(samples) < minTrendSamples {
		return nil
	}
	eta, _, ok := ProjectETA(samples, tp.Threshold, tp.Direction, tp.MinR2)
	if !ok {
		return nil
	}
	etaMin := eta / 60
	if etaMin <= 0 || etaMin > float64(tp.ProjectionHorizonMin) || etaMin > float64(tp.FireIfETAWithinMinutes) {
		return nil
	}
	ns, name := seriesTarget(s.Metric, tp.ObjectLabel)
	k := key{det.Playbook, ns, name}
	if last, seen := e.predictedFired[k]; seen && now-last < predictedRefireTTL.Seconds() {
		return nil
	}
	e.predictedFired[k] = now
	eta1 := math.Round(etaMin*10) / 10
	f := Finding{
		ID: newFindingID(), Playbook: det.Playbook, ClusterID: e.ClusterID, Namespace: ns, Object: name,
		Evidence: fmt.Sprintf("predicted %s: %s %s toward %v, crossing in ~%.0fm",
			det.Playbook, clip(tp.Metric, 80), tp.Direction, tp.Threshold, etaMin),
		FirstSeen: now, FiredAt: now, Source: "trend", Severity: "predicted", ETAMinutes: &eta1,
	}
	e.publish(f)
	return &f
}

// Run is the tick loop: debounce expiry and stale-arm cleanup.
func (e *Engine) Run(ctx context.Context) {
	tk := time.NewTicker(tickInterval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tk.C:
			e.Tick(now)
		}
	}
}

// RunTrends is the slower trend loop; range queries are expensive.
func (e *Engine) RunTrends(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = trendInterval
	}
	tk := time.NewTicker(interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tk.C:
			e.EvaluateTrends(ctx, now)
		}
	}
}

// ProjectETA is a least-squares projection of the threshold-crossing time in
// seconds. ok is false for fewer than 2 samples, a flat series, a fit noisier
// than minR2, a slope away from the threshold, a metric already past it, or a
// crossing in the past.
func ProjectETA(samples [][2]float64, threshold float64, direction string, minR2 float64) (eta, r2 float64, ok bool) {
	n := float64(len(samples))
	if len(samples) < 2 {
		return 0, 0, false
	}
	x0 := samples[0][0]
	var mx, my float64
	for _, s := range samples {
		mx += s[0] - x0
		my += s[1]
	}
	mx /= n
	my /= n
	var sxx, syy, sxy float64
	for _, s := range samples {
		dx, dy := s[0]-x0-mx, s[1]-my
		sxx += dx * dx
		syy += dy * dy
		sxy += dx * dy
	}
	if sxx == 0 || syy == 0 {
		return 0, 0, false
	}
	slope := sxy / sxx
	r2 = sxy * sxy / (sxx * syy)
	if r2 < minR2 {
		return 0, r2, false
	}
	current := samples[len(samples)-1][1]
	if direction == "falling" {
		if slope >= 0 || current <= threshold {
			return 0, r2, false
		}
	} else if slope <= 0 || current >= threshold {
		return 0, r2, false
	}
	eta = (threshold - current) / slope
	if eta <= 0 {
		return 0, r2, false
	}
	return eta, r2, true
}

func cleanSamples(vals [][2]float64) [][2]float64 {
	out := make([][2]float64, 0, len(vals))
	for _, v := range vals {
		if math.IsNaN(v[1]) || math.IsInf(v[1], 0) || math.IsNaN(v[0]) {
			continue
		}
		out = append(out, v)
	}
	return out
}

func seriesTarget(labels map[string]string, objectLabel string) (ns, name string) {
	ns = labels["namespace"]
	if ns == "" {
		ns = "cluster"
	}
	if objectLabel != "" {
		if name = labels[objectLabel]; name == "" {
			name = "unknown"
		}
		return
	}
	for _, l := range []string{"pod", "persistentvolumeclaim", "node", "container", "instance"} {
		if labels[l] != "" {
			return ns, labels[l]
		}
	}
	return ns, "unknown"
}

// summarise builds one line of evidence. Watching is cluster-wide and the
// blocklist does not reach it, so event free text from a protected namespace
// is withheld: the reason (a k8s enum) survives, the message does not.
func (e *Engine) summarise(o sensorium.Observation) string {
	switch o.Kind {
	case "pod_status":
		return "pod status=" + o.Str("status")
	case "event":
		reason := o.Str("reason")
		if e.Blocked[strings.ToLower(strings.TrimSpace(o.Namespace))] {
			return fmt.Sprintf("event reason=%s message=%s", reason, withheldMessage)
		}
		return fmt.Sprintf("event reason=%s message=%s", reason, clip(o.Str("message"), 140))
	case "node_status":
		return "node status=" + o.Str("status")
	}
	return o.Kind
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
