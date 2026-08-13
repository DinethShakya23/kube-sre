package events

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWireShapeIsFlat(t *testing.T) {
	cmd := "kubectl get pods"
	cases := map[string]Event{
		"status":       NewStatus("s", "analyzing", "Analyzing your request…"),
		"tool_call":    NewToolCall("s", "run_kubectl", &cmd),
		"tool_result":  NewToolResult("s", "run_kubectl", "ok"),
		"token":        NewToken("s", "hi"),
		"final":        NewFinal("s"),
		"hitl_request": NewHitlRequest("s", "high", "kubectl delete pod x", nil),
		"plan":         NewPlan("s", []map[string]any{{"description": "a", "status": "pending"}}),
		"error":        NewError("s", "boom"),
		"usage":        NewUsage("s", 10, 5, 2),
	}
	for typ, ev := range cases {
		m := Map(ev)
		if m["type"] != typ || m["session_id"] != "s" || m["ts"] == nil || ev.Kind() != typ {
			t.Errorf("%s: %v", typ, m)
		}
	}
	m := Map(NewToolCall("s", "run_helm", nil))
	if v, ok := m["command"]; !ok || v != nil {
		t.Errorf("command is present and null when absent: %v", m)
	}
	u := Map(NewUsage("s", 10, 5, 2))
	if u["total_tokens"] != float64(15) || u["llm_calls"] != float64(2) {
		t.Errorf("%v", u)
	}
	h := Map(NewHitlRequest("s", "high", "c", nil))
	if id, _ := h["action_id"].(string); len(id) != 36 || strings.Count(id, "-") != 4 || id[14] != '4' {
		t.Errorf("action id %v", h["action_id"])
	}
	if _, err := json.Marshal(Map(NewPlan("s", nil))); err != nil {
		t.Error(err)
	}
}

type rec struct {
	mu   sync.Mutex
	kind []string
}

func (r *rec) Record(ep, kind string, _ map[string]any) {
	r.mu.Lock()
	r.kind = append(r.kind, ep+":"+kind)
	r.mu.Unlock()
}

func TestEmitStreamAndHistory(t *testing.T) {
	r := &rec{}
	e := NewEmitter(r)
	e.Prepare("s1")
	e.Emit("s1", NewStatus("s1", "loading", "x"))
	e.Emit("s1", NewToken("s1", "hello"))
	e.Close("s1")

	var got []string
	for it := range e.Stream(context.Background(), "s1", time.Second) {
		got = append(got, it.Event["type"].(string))
	}
	if strings.Join(got, ",") != "status,token" {
		t.Errorf("stream %v", got)
	}
	h := e.History("s1")
	if len(h) != 3 || h[2]["type"] != "final" {
		t.Errorf("history %v", h)
	}
	if strings.Join(r.kind, ",") != "s1:status,s1:token,s1:final" {
		t.Errorf("recorder %v", r.kind)
	}
}

func TestPrepareKeepsHistoryAcrossTurns(t *testing.T) {
	e := NewEmitter(nil)
	e.Prepare("s")
	e.Emit("s", NewStatus("s", "a", "1"))
	e.Close("s")
	e.Prepare("s")
	e.Emit("s", NewStatus("s", "b", "2"))
	e.Close("s")
	if h := e.History("s"); len(h) != 4 {
		t.Errorf("a multi turn session accumulates one log: %d", len(h))
	}
	if !e.HasHistory("s") || e.HasHistory("never") {
		t.Error("has history distinguishes empty from absent")
	}
	e.Prepare("empty")
	if !e.HasHistory("empty") || len(e.History("empty")) != 0 {
		t.Error("a prepared session with no events is present and empty")
	}
	if e.History("never") != nil {
		t.Error("absent")
	}
}

func TestStreamHeartbeatsAndStopsOnContext(t *testing.T) {
	e := NewEmitter(nil)
	e.Prepare("s")
	ctx, cancel := context.WithCancel(context.Background())
	ch := e.Stream(ctx, "s", 30*time.Millisecond)
	it := <-ch
	if !it.Heartbeat || it.Event != nil {
		t.Errorf("%+v", it)
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			for range ch {
			}
		}
	case <-time.After(time.Second):
		t.Fatal("the stream must stop when the context ends")
	}
}

func TestEmitFromManyGoroutines(t *testing.T) {
	e := NewEmitter(nil)
	e.Prepare("s")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Emit("s", NewToken("s", "x"))
		}()
	}
	wg.Wait()
	e.Close("s")
	n := 0
	for it := range e.Stream(context.Background(), "s", time.Second) {
		if it.Event != nil {
			n++
		}
	}
	if n != 50 {
		t.Errorf("got %d", n)
	}
}

func TestEmitNeverBlocksWithoutAConsumer(t *testing.T) {
	e := NewEmitter(nil)
	e.Prepare("s")
	done := make(chan struct{})
	go func() {
		for i := 0; i < 20000; i++ {
			e.Emit("s", NewToken("s", "x"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Emit blocked with nobody reading, which would hang any turn the watchtower starts")
	}
}
