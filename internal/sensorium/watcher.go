package sensorium

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	backoffMax     = 60 * time.Second
	stalenessGrace = 30 * time.Second
	pressureRatio  = 0.8
)

// StreamHealth is the live state of one `kubectl --watch` stream. It is what
// tells "quiet" apart from "deaf".
type StreamHealth struct {
	Name        string `json:"name"`
	Connected   bool   `json:"connected"`
	ConnectedAt int64  `json:"connected_at,omitempty"`
	Stopped     bool   `json:"stopped"`
	Failures    int    `json:"consecutive_failures"`
	LastError   string `json:"last_error,omitempty"`
}

// QueueStats reports the observation queue. ShedTotal > 0 means perception
// is being dropped.
type QueueStats struct {
	ShedTotal int64 `json:"shed_total"`
	HighWater int64 `json:"high_water"`
	MaxSize   int   `json:"maxsize"`
}

type normalizer func(doc []byte, w *Watcher) *Observation

type spec struct {
	name string
	args []string
	norm normalizer
}

// Watcher streams pod and event changes from kubectl.
type Watcher struct {
	Bin            string
	Kubeconfig     string
	ClusterID      string
	Namespaces     []string
	QueueSize      int
	BackoffInitial time.Duration

	epoch time.Time

	mu      sync.Mutex
	streams map[string]*StreamHealth
	warned  map[string]bool

	queue     chan Observation
	shed      atomic.Int64
	highWater atomic.Int64
	pressured atomic.Bool
}

func NewWatcher(clusterID, kubeconfig string, namespaces []string, queueSize int) *Watcher {
	if queueSize <= 0 {
		queueSize = 10000
	}
	return &Watcher{
		Bin: "kubectl", Kubeconfig: kubeconfig, ClusterID: clusterID,
		Namespaces: namespaces, QueueSize: queueSize, BackoffInitial: 2 * time.Second,
	}
}

func (w *Watcher) Health() []StreamHealth {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]StreamHealth, 0, len(w.streams))
	for _, s := range w.streams {
		out = append(out, *s)
	}
	return out
}

func (w *Watcher) AnyConnected() bool {
	for _, s := range w.Health() {
		if s.Connected {
			return true
		}
	}
	return false
}

func (w *Watcher) Queue() QueueStats {
	return QueueStats{ShedTotal: w.shed.Load(), HighWater: w.highWater.Load(), MaxSize: w.QueueSize}
}

func (w *Watcher) specs() []spec {
	tail := []string{"--watch", "--output-watch-events=true", "-o", "json"}
	kinds := []struct {
		res  string
		norm normalizer
	}{{"pods", podObservation}, {"events", eventObservation}}
	var out []spec
	for _, k := range kinds {
		if len(w.Namespaces) == 0 {
			out = append(out, spec{"get " + k.res + " -A", append([]string{"get", k.res, "-A"}, tail...), k.norm})
			continue
		}
		for _, ns := range w.Namespaces {
			out = append(out, spec{"get " + k.res + " -n " + ns, append([]string{"get", k.res, "-n", ns}, tail...), k.norm})
		}
	}
	return out
}

// Run starts one watch per resource per namespace plus a single consumer, and
// blocks until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context, sink func(Observation)) {
	w.epoch = time.Now()
	w.mu.Lock()
	w.streams = map[string]*StreamHealth{}
	w.warned = map[string]bool{}
	w.mu.Unlock()
	w.queue = make(chan Observation, w.QueueSize)
	w.shed.Store(0)
	w.highWater.Store(0)

	specs := w.specs()
	if len(w.Namespaces) > 0 {
		slog.Info("sensorium watching", "namespaces", strings.Join(w.Namespaces, ","), "streams", len(specs))
	} else {
		slog.Info("sensorium watching all namespaces", "streams", len(specs))
	}

	var wg sync.WaitGroup
	for _, s := range specs {
		wg.Add(1)
		go func(s spec) {
			defer wg.Done()
			w.watch(ctx, s)
		}(s)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case obs := <-w.queue:
				w.deliver(sink, obs)
			}
		}
	}()
	wg.Wait()
	<-done
}

// deliver keeps one bad observation from killing perception.
func (w *Watcher) deliver(sink func(Observation), obs Observation) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("sensorium sink failed for one observation", "err", r)
		}
	}()
	sink(obs)
}

// enqueue never blocks: blocking would back-pressure kubectl itself and end in
// a reconnect storm. When full it drops the oldest, since a pod status is a
// level and the newest one supersedes the stale one.
func (w *Watcher) enqueue(obs Observation) {
	for {
		select {
		case w.queue <- obs:
			n := int64(len(w.queue))
			for {
				hw := w.highWater.Load()
				if n <= hw || w.highWater.CompareAndSwap(hw, n) {
					break
				}
			}
			if float64(n) >= float64(cap(w.queue))*pressureRatio && w.pressured.CompareAndSwap(false, true) {
				slog.Warn("sensorium queue is filling up, nothing shed yet",
					"len", n, "max", cap(w.queue),
					"hint", "narrow SENSORIUM_WATCH_NAMESPACES or raise SENSORIUM_QUEUE_MAXSIZE")
			}
			return
		default:
			select {
			case <-w.queue:
				if c := w.shed.Add(1); c == 1 || c%1000 == 0 {
					slog.Warn("sensorium queue full, detection is lossy", "shed", c)
				}
			default:
			}
		}
	}
}

func (w *Watcher) health(name string) *StreamHealth {
	w.mu.Lock()
	defer w.mu.Unlock()
	h, ok := w.streams[name]
	if !ok {
		h = &StreamHealth{Name: name}
		w.streams[name] = h
	}
	return h
}

func (w *Watcher) update(name string, f func(h *StreamHealth)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f(w.streams[name])
}

func (w *Watcher) watch(ctx context.Context, s spec) {
	w.health(s.name)
	backoff := w.BackoffInitial
	for ctx.Err() == nil {
		args := s.args
		if w.Kubeconfig != "" {
			args = append([]string{"--kubeconfig", w.Kubeconfig}, args...)
		}
		cmd := exec.CommandContext(ctx, w.Bin, args...)
		var errBuf tailBuffer
		cmd.Stderr = &errBuf
		out, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err != nil {
			if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
				w.update(s.name, func(h *StreamHealth) {
					h.Connected, h.Stopped, h.LastError = false, true, "kubectl not found on the server"
				})
				slog.Warn("sensorium: kubectl not found, watcher disabled")
				return
			}
			w.update(s.name, func(h *StreamHealth) {
				h.Connected, h.LastError = false, err.Error()
				h.Failures++
			})
			slog.Warn("sensorium watch error", "stream", s.name, "err", err)
		} else {
			slog.Info("sensorium watch started", "stream", s.name)
			w.update(s.name, func(h *StreamHealth) {
				h.Connected, h.ConnectedAt, h.LastError = true, time.Now().Unix(), ""
			})
			w.pressured.Store(false)
			w.resetWarned()
			w.read(out, s, func() {
				backoff = w.BackoffInitial
				w.update(s.name, func(h *StreamHealth) { h.Failures = 0 })
			})
			_ = cmd.Wait()
			if ctx.Err() != nil {
				w.update(s.name, func(h *StreamHealth) { h.Connected = false })
				return
			}
			reason := errBuf.last()
			if reason == "" {
				reason = fmt.Sprintf("stream closed (rc=%d)", cmd.ProcessState.ExitCode())
			}
			w.update(s.name, func(h *StreamHealth) {
				h.Connected, h.LastError = false, truncate(reason, 300)
				h.Failures++
			})
			slog.Warn("sensorium watch closed", "stream", s.name, "reason", reason)
		}
		select {
		case <-ctx.Done():
			w.update(s.name, func(h *StreamHealth) { h.Connected = false })
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > backoffMax {
			backoff = backoffMax
		}
	}
}

// read decodes concatenated JSON documents until the stream ends.
func (w *Watcher) read(r io.Reader, s spec, flowing func()) {
	dec := json.NewDecoder(r)
	for {
		var doc json.RawMessage
		if err := dec.Decode(&doc); err != nil {
			return
		}
		if obs := s.norm(doc, w); obs != nil {
			w.enqueue(*obs)
		}
		flowing()
	}
}

func (w *Watcher) resetWarned() {
	w.mu.Lock()
	w.warned = map[string]bool{}
	w.mu.Unlock()
}

// warnOnce logs a reason once per (re)connect.
func (w *Watcher) warnOnce(reason string) {
	w.mu.Lock()
	seen := w.warned[reason]
	w.warned[reason] = true
	w.mu.Unlock()
	if !seen {
		slog.Warn("sensorium: the event staleness filter cannot run for these events, " +
			"replayed history may look current: " + reason)
	}
}

type watchDoc struct {
	Type   string          `json:"type"`
	Object json.RawMessage `json:"object"`
}

// unwrap handles both wrapped ({type, object}) and bare objects.
func unwrap(doc []byte) (typ string, obj []byte) {
	var d watchDoc
	if json.Unmarshal(doc, &d) == nil && len(d.Object) > 0 {
		return d.Type, d.Object
	}
	return "", doc
}

func podObservation(doc []byte, w *Watcher) *Observation {
	typ, raw := unwrap(doc)
	var p podObject
	if json.Unmarshal(raw, &p) != nil || p.Kind != "Pod" {
		return nil
	}
	owner := ""
	for _, ref := range p.Metadata.OwnerReferences {
		if ref.Controller {
			owner = ref.Kind + "/" + ref.Name
			break
		}
	}
	return &Observation{
		Kind: "pod_status", ClusterID: w.ClusterID,
		Namespace: p.Metadata.Namespace, Name: p.Metadata.Name, TS: time.Now(),
		Fields: map[string]any{
			"status":           p.displayStatus(),
			"watch_type":       typ,
			"node":             p.Spec.NodeName,
			"owner":            owner,
			"uid":              p.Metadata.UID,
			"resource_version": p.Metadata.ResourceVersion,
		},
	}
}

type eventObject struct {
	Kind          string `json:"kind"`
	Reason        string `json:"reason"`
	Message       string `json:"message"`
	Type          string `json:"type"`
	LastTimestamp string `json:"lastTimestamp"`
	EventTime     string `json:"eventTime"`
	Metadata      struct {
		Namespace         string `json:"namespace"`
		CreationTimestamp string `json:"creationTimestamp"`
	} `json:"metadata"`
	InvolvedObject struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"involvedObject"`
}

// eventTime parses an Event's last activity time. ok=false means the age is
// unknown, and the caller fails open: dropping an event we cannot date would
// turn a timestamp quirk into a missed incident.
func (e *eventObject) eventAge(w *Watcher) (t time.Time, ok bool) {
	raw := e.LastTimestamp
	if raw == "" {
		raw = e.EventTime
	}
	if raw == "" {
		raw = e.Metadata.CreationTimestamp
	}
	if raw == "" {
		w.warnOnce("an Event carried no lastTimestamp, eventTime or creationTimestamp")
		return t, false
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		w.warnOnce("an Event timestamp could not be parsed: " + truncate(raw, 40))
		return t, false
	}
	return t, true
}

func eventObservation(doc []byte, w *Watcher) *Observation {
	_, raw := unwrap(doc)
	var e eventObject
	if json.Unmarshal(raw, &e) != nil || e.Kind != "Event" {
		return nil
	}
	// The watch replays recent history on connect, so old events are not news.
	if t, ok := e.eventAge(w); ok && t.Before(w.epoch.Add(-stalenessGrace)) {
		return nil
	}
	ns := e.InvolvedObject.Namespace
	if ns == "" {
		ns = e.Metadata.Namespace
	}
	return &Observation{
		Kind: "event", ClusterID: w.ClusterID, Namespace: ns,
		Name: e.InvolvedObject.Name, TS: time.Now(),
		Fields: map[string]any{
			"reason":        e.Reason,
			"message":       e.Message,
			"involved_kind": e.InvolvedObject.Kind,
			"event_type":    e.Type,
		},
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// tailBuffer keeps only the last few lines written to it, so stderr never
// blocks the child and the reason for a drop is still available.
type tailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if t.buf.Len() > 4096 {
		b := t.buf.Bytes()
		keep := append([]byte(nil), b[len(b)-4096:]...)
		t.buf.Reset()
		t.buf.Write(keep)
	}
	return len(p), nil
}

func (t *tailBuffer) last() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(t.buf.String()), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
