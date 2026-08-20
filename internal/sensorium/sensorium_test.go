package sensorium

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func status(t *testing.T, js string) string {
	t.Helper()
	var p podObject
	if err := json.Unmarshal([]byte(js), &p); err != nil {
		t.Fatal(err)
	}
	return p.displayStatus()
}

func TestPodDisplayStatus(t *testing.T) {
	cases := map[string]struct{ js, want string }{
		"running":               {`{"status":{"phase":"Running","containerStatuses":[{"ready":true,"state":{"running":{}}}]}}`, "Running"},
		"crashloop":             {`{"status":{"phase":"Running","containerStatuses":[{"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}}`, "CrashLoopBackOff"},
		"oom":                   {`{"status":{"phase":"Running","containerStatuses":[{"state":{"terminated":{"reason":"OOMKilled","exitCode":137}}}]}}`, "OOMKilled"},
		"exit code":             {`{"status":{"phase":"Running","containerStatuses":[{"state":{"terminated":{"exitCode":3}}}]}}`, "ExitCode:3"},
		"signal":                {`{"status":{"phase":"Running","containerStatuses":[{"state":{"terminated":{"signal":9}}}]}}`, "Signal:9"},
		"evicted":               {`{"status":{"phase":"Failed","reason":"Evicted"}}`, "Evicted"},
		"pending":               {`{"status":{"phase":"Pending"}}`, "Pending"},
		"no phase":              {`{"status":{}}`, "Unknown"},
		"init waiting":          {`{"spec":{"initContainers":[{},{}]},"status":{"phase":"Pending","initContainerStatuses":[{"state":{"waiting":{"reason":"ImagePullBackOff"}}}]}}`, "Init:ImagePullBackOff"},
		"init progress":         {`{"spec":{"initContainers":[{},{}]},"status":{"phase":"Pending","initContainerStatuses":[{"state":{"terminated":{"exitCode":0}}},{"state":{"waiting":{"reason":"PodInitializing"}}}]}}`, "Init:1/2"},
		"init failed":           {`{"status":{"phase":"Pending","initContainerStatuses":[{"state":{"terminated":{"reason":"Error","exitCode":1}}}]}}`, "Init:Error"},
		"completed but ready":   {`{"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"ready":true,"state":{"running":{}}},{"state":{"terminated":{"reason":"Completed"}}}]}}`, "Running"},
		"completed not ready":   {`{"status":{"phase":"Running","containerStatuses":[{"ready":true,"state":{"running":{}}},{"state":{"terminated":{"reason":"Completed"}}}]}}`, "NotReady"},
		"terminating":           {`{"metadata":{"deletionTimestamp":"2026-01-01T00:00:00Z"},"status":{"phase":"Running"}}`, "Terminating"},
		"terminating node lost": {`{"metadata":{"deletionTimestamp":"x"},"status":{"phase":"Running","reason":"NodeLost"}}`, "Unknown"},
		"deleted but succeeded": {`{"metadata":{"deletionTimestamp":"x"},"status":{"phase":"Succeeded"}}`, "Succeeded"},
	}
	for name, c := range cases {
		if got := status(t, c.js); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

func TestFirstContainerHasLastWord(t *testing.T) {
	js := `{"status":{"phase":"Running","containerStatuses":[
	 {"state":{"waiting":{"reason":"CrashLoopBackOff"}}},
	 {"state":{"waiting":{"reason":"ImagePullBackOff"}}}]}}`
	if got := status(t, js); got != "CrashLoopBackOff" {
		t.Errorf("got %q", got)
	}
}

func TestPodObservation(t *testing.T) {
	w := NewWatcher("c1", "", nil, 10)
	doc := `{"type":"MODIFIED","object":{"kind":"Pod","metadata":{"namespace":"ns","name":"p","uid":"u","resourceVersion":"7",
	 "ownerReferences":[{"kind":"ConfigMap","name":"x"},{"kind":"ReplicaSet","name":"rs","controller":true}]},
	 "spec":{"nodeName":"n1"},"status":{"phase":"Running"}}}`
	o := podObservation([]byte(doc), w)
	if o == nil || o.Name != "p" || o.Namespace != "ns" || o.Str("owner") != "ReplicaSet/rs" ||
		o.Str("node") != "n1" || o.Str("watch_type") != "MODIFIED" || o.Str("resource_version") != "7" {
		t.Fatalf("got %+v", o)
	}
	if podObservation([]byte(`{"type":"ADDED","object":{"kind":"Node"}}`), w) != nil {
		t.Error("non-pods are ignored")
	}
}

func TestEventStaleness(t *testing.T) {
	w := NewWatcher("c1", "", nil, 10)
	w.epoch = time.Now()
	w.warned = map[string]bool{}
	ev := func(ts string) string {
		return `{"type":"ADDED","object":{"kind":"Event","reason":"BackOff","message":"m","type":"Warning",` + ts +
			`"involvedObject":{"kind":"Pod","name":"p","namespace":"ns"}}}`
	}
	old := w.epoch.Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	recent := w.epoch.Add(-10 * time.Second).UTC().Format(time.RFC3339)
	if eventObservation([]byte(ev(`"lastTimestamp":"`+old+`",`)), w) != nil {
		t.Error("replayed history must be dropped")
	}
	o := eventObservation([]byte(ev(`"lastTimestamp":"`+recent+`",`)), w)
	if o == nil || o.Str("reason") != "BackOff" || o.Str("involved_kind") != "Pod" || o.Namespace != "ns" {
		t.Fatalf("got %+v", o)
	}
	if eventObservation([]byte(ev("")), w) == nil {
		t.Error("unknown age fails open")
	}
	if eventObservation([]byte(ev(`"lastTimestamp":"garbage",`)), w) == nil {
		t.Error("unparseable age fails open")
	}
}

func TestEventTimestampFallbackAndUTC(t *testing.T) {
	w := NewWatcher("c1", "", nil, 10)
	w.epoch = time.Now()
	w.warned = map[string]bool{}
	old := w.epoch.Add(-time.Hour).UTC().Format(time.RFC3339)
	doc := `{"kind":"Event","reason":"X","metadata":{"namespace":"ns","creationTimestamp":"` + old + `"},"involvedObject":{"name":"p"}}`
	if eventObservation([]byte(doc), w) != nil {
		t.Error("creationTimestamp is used when others are missing")
	}
}

func fakeKubectl(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kubectl")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func collect(t *testing.T, w *Watcher, want int, d time.Duration) []Observation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var mu sync.Mutex
	var got []Observation
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		w.Run(ctx, func(o Observation) {
			mu.Lock()
			got = append(got, o)
			mu.Unlock()
			if len(got) >= want {
				cancel()
			}
		})
	}()
	<-finished
	mu.Lock()
	defer mu.Unlock()
	return append([]Observation(nil), got...)
}

func TestWatcherStreamsPods(t *testing.T) {
	w := NewWatcher("c1", "", []string{"a"}, 10)
	w.Bin = fakeKubectl(t, `case "$*" in *pods*) echo '{"type":"ADDED","object":{"kind":"Pod","metadata":{"namespace":"a","name":"p1"},"status":{"phase":"Running"}}}'
echo '{"type":"ADDED","object":{"kind":"Pod","metadata":{"namespace":"a","name":"p2"},"status":{"phase":"Pending"}}}';; esac
exec sleep 5`)
	got := collect(t, w, 2, 4*time.Second)
	if len(got) != 2 || got[0].Name != "p1" || got[1].Str("status") != "Pending" {
		t.Fatalf("got %+v", got)
	}
	if len(w.Health()) != 2 {
		t.Errorf("health: %+v", w.Health())
	}
}

func TestWatcherMissingKubectlStops(t *testing.T) {
	w := NewWatcher("c1", "", nil, 10)
	w.Bin = "/nonexistent/kubectl"
	collect(t, w, 1, 500*time.Millisecond)
	for _, h := range w.Health() {
		if !h.Stopped || h.Connected || h.LastError == "" {
			t.Errorf("stream should be stopped with a reason: %+v", h)
		}
	}
	if len(w.Health()) != 2 {
		t.Errorf("want pods and events streams, got %d", len(w.Health()))
	}
}

func TestWatcherRecordsReasonAndReconnects(t *testing.T) {
	w := NewWatcher("c1", "", nil, 10)
	w.BackoffInitial = 20 * time.Millisecond
	w.Bin = fakeKubectl(t, `echo 'Error from server (Forbidden): pods is forbidden' >&2; exit 1`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx, func(Observation) {}) }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		hs := w.Health()
		ok := len(hs) == 2
		for _, h := range hs {
			if h.Failures < 2 || h.LastError == "" {
				ok = false
			}
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("want repeated failures with the reason: %+v", w.Health())
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestEnqueueShedsOldest(t *testing.T) {
	w := NewWatcher("c1", "", nil, 3)
	w.queue = make(chan Observation, 3)
	for i := 0; i < 5; i++ {
		w.enqueue(Observation{Name: string(rune('a' + i))})
	}
	if s := w.Queue(); s.ShedTotal != 2 || s.HighWater != 3 || s.MaxSize != 3 {
		t.Errorf("stats %+v", s)
	}
	first := <-w.queue
	if first.Name != "c" {
		t.Errorf("oldest should be dropped, first is %q", first.Name)
	}
}

func TestSpecsScopeByNamespace(t *testing.T) {
	if n := len(NewWatcher("c", "", nil, 1).specs()); n != 2 {
		t.Errorf("all namespaces: %d streams", n)
	}
	w := NewWatcher("c", "", []string{"a", "b"}, 1)
	if n := len(w.specs()); n != 4 {
		t.Errorf("two namespaces: %d streams", n)
	}
}
