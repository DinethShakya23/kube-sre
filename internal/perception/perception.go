// Package perception runs the sensorium and the detector engine, and answers one
// question in one place: is anything actually watching?
//
// An all clear is a claim about perception, not about the record. The recorder can
// be perfectly healthy and hold nothing because nothing was ever looking. Two
// surfaces make that claim about the same window, the findings endpoint and the
// morning digest, and they once computed it separately and disagreed: with no
// watch stream connected, one said the sensorium was starting and refused to call
// the cluster clear, while the other said "Quiet watch". So the classification
// lives here once and both read it.
//
// These are the states now, not a history of the window. A stream that died an
// hour ago and has since reconnected reads as active, so a reported gap is a lower
// bound on the blindness in the window, never an upper one.
package perception

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/detect"
	"github.com/DinethShakya23/kube-sre/internal/playbooks"
	"github.com/DinethShakya23/kube-sre/internal/sensorium"
)

// Sensorium states.
const (
	Disabled     = "disabled"
	Starting     = "starting"
	Active       = "active"
	Stopped      = "stopped"
	Reconnecting = "reconnecting"
)

// Predictive states.
const (
	Off   = "off"
	Blind = "blind"
)

// Why there is no engine. Four unrelated situations end at "no engine", and only
// the first is a configuration choice, so each path that can leave it absent
// records which one it was.
const (
	NotStarted     = "not_started"
	DisabledByFlag = "disabled_by_flag"
	NoDetectors    = "no_detectors"
	StartFailed    = "start_failed" // it tried and raised: an outage, not a setting
	Standby        = "standby"      // another replica holds the singleton lock: normal
	StoppedReason  = "stopped"
	Running        = "running"
)

// State is what each instrument can currently see.
type State struct {
	Sensorium           string                   `json:"sensorium"`
	SensoriumReason     string                   `json:"sensorium_reason"`
	Detectors           int                      `json:"detectors"`
	Predictive          string                   `json:"predictive"`
	PredictiveDetectors int                      `json:"predictive_detectors"`
	PredictiveError     *string                  `json:"predictive_error"`
	Streams             []sensorium.StreamHealth `json:"streams"`
	ShedTotal           int64                    `json:"-"`
	QueueHighWater      int64                    `json:"-"`
	WatchNamespaces     []string                 `json:"-"`
}

// Watching is true only when a watch stream is connected: the one state in which
// an empty findings list is evidence rather than an absence of looking.
func (s State) Watching() bool { return s.Sensorium == Active }

// Service owns the perception stack of one process.
type Service struct {
	Cfg       *config.Config
	Playbooks *playbooks.Registry
	// ClusterID names the cluster findings belong to.
	ClusterID func(context.Context) string
	// OnFinding is told about every finding, as it fires. It must not block.
	OnFinding func(detect.Finding)
	// Observe also sees every observation, after the detectors have.
	Observe func(sensorium.Observation)
	// StoredDetectors loads promoted and shadow detectors; nil when authoring is off.
	StoredDetectors func(ctx context.Context, clusterID string) (active, shadow []detect.DetectBlock, err error)
	Series          detect.SeriesSource
	Recorder        detect.Recorder
	Bin             string

	mu      sync.Mutex
	engine  *detect.Engine
	watcher *sensorium.Watcher
	reason  string
	detail  string
	cancel  context.CancelFunc
	done    sync.WaitGroup
	// lastStored is the count as of the last successful refresh; nil until one has
	// completed, which is not the same as the last refresh finding nothing.
	lastStored *[2]int
}

func NewService(cfg *config.Config, pb *playbooks.Registry) *Service {
	return &Service{Cfg: cfg, Playbooks: pb, reason: NotStarted, Bin: "kubectl",
		ClusterID: func(context.Context) string { return "unknown" }}
}

// Engine returns the detector engine, or nil when perception is not running.
func (s *Service) Engine() *detect.Engine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.engine
}

// Absence returns why the engine is absent, or Running.
func (s *Service) Absence() (reason, detail string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason, s.detail
}

// RecordDisabled notes that the flag switched perception off.
func (s *Service) RecordDisabled() {
	s.mu.Lock()
	s.reason, s.detail = DisabledByFlag, "SENSORIUM_ENABLED=false"
	s.mu.Unlock()
}

// RecordStartFailure notes that perception raised on the way up. The caller
// swallows the error so perception failing never costs availability, but a
// swallowed start is still an outage and must not read as a configuration choice.
func (s *Service) RecordStartFailure(err error) {
	s.mu.Lock()
	s.reason, s.detail = StartFailed, err.Error()
	s.mu.Unlock()
}

// Start begins perceiving: the detector engine's tick loops and the kubectl
// watch streams. It returns once they are launched.
func (s *Service) Start(ctx context.Context) error {
	detectors := s.Playbooks.Detectors()
	s.mu.Lock()
	if len(detectors) == 0 {
		s.reason, s.detail = NoDetectors, "no compiled detectors were loaded"
		s.mu.Unlock()
		slog.Info("sensorium has no compiled detectors, not starting")
		return nil
	}
	clusterID := s.ClusterID(ctx)
	eng := detect.NewEngine(clusterID, detectors)
	eng.OnFinding = s.OnFinding
	eng.Recorder = s.Recorder
	eng.Series = s.Series
	eng.Blocked = s.Cfg.BlockedNamespaces

	w := sensorium.NewWatcher(clusterID, s.Cfg.KubeconfigPath, s.Cfg.SensoriumNamespaces, s.Cfg.SensoriumQueueSize)
	w.Bin = s.Bin
	runCtx, cancel := context.WithCancel(ctx)
	s.engine, s.watcher, s.cancel = eng, w, cancel
	s.mu.Unlock()

	s.done.Add(2)
	go func() { defer s.done.Done(); eng.Run(runCtx) }()
	if s.Cfg.PredictiveDetection {
		interval := time.Duration(s.Cfg.PredictiveTrendSeconds) * time.Second
		s.done.Add(1)
		go func() { defer s.done.Done(); eng.RunTrends(runCtx, interval) }()
		slog.Info("sensorium predictive detection on", "trend_interval", interval)
	}
	if s.Cfg.NLDetectorAuthoring && s.StoredDetectors != nil {
		s.refreshStored(runCtx, eng, clusterID)
		interval := time.Duration(s.Cfg.DBDetectorRefreshSecs) * time.Second
		s.done.Add(1)
		go func() {
			defer s.done.Done()
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-t.C:
					s.refreshStored(runCtx, eng, clusterID)
				}
			}
		}()
	}
	go func() {
		defer s.done.Done()
		w.Run(runCtx, func(o sensorium.Observation) {
			eng.Process(o)
			if s.Observe != nil {
				s.Observe(o)
			}
		})
	}()
	s.mu.Lock()
	s.reason, s.detail = Running, ""
	s.mu.Unlock()
	slog.Info("sensorium started", "detectors", len(detectors), "cluster", clusterID)
	return nil
}

// Stop halts perception and records why. The watcher is kept so the queue counters
// still answer after a stop: a sensorium that has gone away can still have shed
// observations while it was up, and reporting zero there would be a claim.
func (s *Service) Stop(reason, detail string) {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel, s.engine, s.lastStored = nil, nil, nil
	if reason == "" {
		reason = StoppedReason
	}
	s.reason, s.detail = reason, detail
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.done.Wait()
}

func (s *Service) queue() sensorium.QueueStats {
	s.mu.Lock()
	w := s.watcher
	s.mu.Unlock()
	if w == nil {
		return sensorium.QueueStats{MaxSize: s.Cfg.SensoriumQueueSize}
	}
	return w.Queue()
}

// Queue reports the observation queue. ShedTotal above zero means the sensorium
// is dropping perception.
func (s *Service) Queue() sensorium.QueueStats { return s.queue() }

func (s *Service) streams() []sensorium.StreamHealth {
	s.mu.Lock()
	w := s.watcher
	s.mu.Unlock()
	if w == nil {
		return []sensorium.StreamHealth{}
	}
	return w.Health()
}

// Status is the block reported on /healthz. An operator must be able to see that
// nothing is watching. watching is separate from enabled, and it is the one to
// alert on: an engine exists whether or not any watch stream is connected, and a
// missing kubectl makes the streams give up for the life of the process, so
// "running" can hold while nothing is observed. Standby is not an outage: another
// replica holds the singleton lock and is watching.
func (s *Service) Status() map[string]any {
	reason, detail := s.Absence()
	if reason != Running {
		return map[string]any{"enabled": false, "state": reason, "reason": detail, "watching": false}
	}
	watching := false
	for _, h := range s.streams() {
		if h.Connected {
			watching = true
		}
	}
	why := ""
	if !watching {
		why = "the detector engine is running but no kubectl watch stream is connected - nothing is being observed"
	}
	return map[string]any{"enabled": true, "state": Running, "watching": watching, "reason": why}
}

// State classifies both instruments. It never fails: perception is not a request path.
func (s *Service) State() State {
	q := s.queue()
	st := State{
		Predictive: Off, Streams: []sensorium.StreamHealth{},
		ShedTotal: q.ShedTotal, QueueHighWater: q.HighWater, WatchNamespaces: s.Cfg.SensoriumNamespaces,
	}
	eng := s.Engine()
	if eng == nil {
		reason, detail := s.Absence()
		st.Sensorium, st.SensoriumReason = Disabled, absencePhrase(reason, detail)
		return st
	}
	streams := s.streams()
	st.Streams = streams
	// "active" is a claim about perception, not about object lifetime. An engine
	// exists regardless; what decides the word is whether any watch stream is connected.
	connected, allStopped := false, len(streams) > 0
	for _, h := range streams {
		if h.Connected {
			connected = true
		}
		if !h.Stopped {
			allStopped = false
		}
	}
	switch {
	case len(streams) == 0:
		st.Sensorium = Starting
	case connected:
		st.Sensorium = Active
	case allStopped:
		st.Sensorium = Stopped
	default:
		st.Sensorium = Reconnecting
	}
	// Same rule one layer over: trend detectors only watch while Prometheus answers.
	for _, d := range eng.Detectors {
		if len(d.TrendPredicates) > 0 {
			st.PredictiveDetectors++
		}
	}
	st.Detectors = len(eng.Detectors)
	blind, _, lastErr := eng.Predictive()
	switch {
	case !s.Cfg.PredictiveDetection || st.PredictiveDetectors == 0:
		st.Predictive = Off
	case blind:
		st.Predictive = Blind
	default:
		st.Predictive = "active"
	}
	if lastErr != "" {
		st.PredictiveError = &lastErr
	}
	return st
}

// Gaps returns one sentence per reason a finding could not have been produced. It
// is empty exactly when every instrument was able to look and what it saw reached
// the detectors. Predictive off is a deliberate configuration, not a gap.
//
// Four independent ways to fail, and only the first is an instrument being
// unavailable: the watch stream can be down, Prometheus can be unreachable, the
// queue between them can have dropped what the stream did see, and the sensorium
// can be scoped to a subset of namespaces. The last two leave every instrument
// reading healthy.
func Gaps(st State) []string {
	var gaps []string
	switch st.Sensorium {
	case Disabled:
		gaps = append(gaps, firstNonEmpty(st.SensoriumReason, absencePhrase("", "")))
	case Starting:
		gaps = append(gaps, "no kubectl watch stream has started - the sensorium was not perceiving, so no detector finding could have been produced")
	case Stopped:
		gaps = append(gaps, "every kubectl watch stream has stopped - no detector finding could have been produced ("+streamReasons(st.Streams)+")")
	case Reconnecting:
		gaps = append(gaps, "no kubectl watch stream is connected (reconnecting: "+streamReasons(st.Streams)+") - detector findings are incomplete for this window")
	}
	if st.Predictive == Blind {
		reason := "no reason recorded"
		if st.PredictiveError != nil {
			reason = *st.PredictiveError
		}
		gaps = append(gaps, "predictive detection is blind - Prometheus could not be queried ("+reason+"), so no predicted finding could have fired")
	}
	// A scoped sensorium is not broken; it is doing what it was told. It is still
	// blind outside its scope, and whoever reads an empty list is usually not the one
	// who set the flag.
	if len(st.WatchNamespaces) > 0 {
		gaps = append(gaps, fmt.Sprintf("the sensorium watches only %d namespace(s) (%s) - nothing outside them was perceived, so an empty findings list is not a statement about the cluster",
			len(st.WatchNamespaces), strings.Join(st.WatchNamespaces, ", ")))
	}
	if st.ShedTotal > 0 {
		gaps = append(gaps, fmt.Sprintf("the observation queue dropped %d event(s) before any detector saw them (queue high-water %d) - findings are incomplete for this window",
			st.ShedTotal, st.QueueHighWater))
	}
	return gaps
}

func firstNonEmpty(items ...string) string {
	for _, s := range items {
		if s != "" {
			return s
		}
	}
	return ""
}

// absencePhrase gives one truthful clause per way the engine can be missing. A
// standby replica is behaving correctly and a failed start is an outage, and a
// single sentence naming two causes as fact would be false on both.
func absencePhrase(reason, detail string) string {
	switch reason {
	case DisabledByFlag:
		return "the sensorium is switched off (SENSORIUM_ENABLED=false) - no detector finding could have been produced"
	case NoDetectors:
		return "the sensorium loaded no compiled detectors, so it did not start - no detector finding could have been produced"
	case StartFailed:
		if detail == "" {
			detail = "no reason recorded"
		}
		return "the sensorium FAILED to start (" + detail + ") - this replica has been perceiving nothing since, and this is an outage rather than a setting"
	case Standby:
		return "this replica is a leader-election standby and watches nothing by design; the replica holding the singleton lock is the one that perceives, so read its findings, not this replica's silence"
	case StoppedReason:
		return "the sensorium has been stopped (the process is shutting down) - no detector finding could have been produced"
	}
	return "the sensorium has not started yet - no detector finding could have been produced"
}

func streamReasons(streams []sensorium.StreamHealth) string {
	var parts []string
	for _, s := range streams {
		reason := s.LastError
		if reason == "" {
			reason = "no reason recorded"
		}
		parts = append(parts, s.Name+": "+reason)
	}
	if len(parts) == 0 {
		return "no reason recorded"
	}
	return strings.Join(parts, "; ")
}

// refreshStored reloads promoted and shadow detectors into the engine. A failed
// read keeps the set already loaded: a read that failed says nothing about which
// detectors should be live, and replacing a working set with the empty one a failed
// read returns is not failing open, it is disarming.
func (s *Service) refreshStored(ctx context.Context, eng *detect.Engine, clusterID string) {
	active, shadow, err := s.StoredDetectors(ctx, clusterID)
	if err != nil {
		kept := [2]int{}
		s.mu.Lock()
		if s.lastStored != nil {
			kept = *s.lastStored
		}
		s.mu.Unlock()
		slog.Warn("stored detector refresh failed, keeping the set already loaded", "active", kept[0], "shadow", kept[1], "err", err)
		return
	}
	eng.SetStoredDetectors(active, shadow)
	counts := [2]int{len(active), len(shadow)}
	s.mu.Lock()
	prev := s.lastStored
	s.lastStored = &counts
	s.mu.Unlock()
	// Logged on the first refresh and on every change, never on an unchanged steady
	// state. The first refresh has to speak even at zero loaded: that is exactly what
	// a cluster id mismatch looks like, and it is indistinguishable from nobody having
	// authored a detector unless the line is emitted at least once.
	if prev == nil || *prev != counts {
		was := "startup"
		if prev != nil {
			was = fmt.Sprintf("%d/%d", prev[0], prev[1])
		}
		slog.Info("stored detectors", "active", counts[0], "shadow", counts[1], "was", was, "cluster", clusterID)
	}
}

// RecordStandby notes that another replica holds the singleton lock. That is normal,
// not an outage.
func (s *Service) RecordStandby() {
	s.mu.Lock()
	s.reason, s.detail = Standby, "another replica holds the singleton lock"
	s.mu.Unlock()
}
