package events

import (
	"context"
	"sync"
	"time"
)

// Recorder receives every event for the durable decision log.
type Recorder interface {
	Record(episode, kind string, payload map[string]any)
}

// Item is one thing read off a session stream. Heartbeat is true after a quiet
// interval, so the caller can write an SSE keepalive comment.
type Item struct {
	Event     map[string]any
	Heartbeat bool
}

// session holds an unbounded queue: a turn started by the watchtower has no SSE
// consumer, and a bounded queue would block Emit forever once it filled.
type session struct {
	pending []map[string]any
	notify  chan struct{}
	history []map[string]any
}

func (s *session) push(m map[string]any) {
	s.pending = append(s.pending, m)
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Emitter is the per session event fan out.
//
// Lifecycle: Prepare(sid) before a turn resets the queue and keeps the history;
// Emit pushes an event onto the queue and into history; Close(sid) ends the
// stream; Stream(sid) is consumed by the SSE endpoint. History lets
// GET /v1/events/replay/{session} replay a session for debugging.
type Emitter struct {
	Recorder Recorder

	mu       sync.Mutex
	sessions map[string]*session
}

func NewEmitter(r Recorder) *Emitter {
	return &Emitter{Recorder: r, sessions: map[string]*session{}}
}

func (e *Emitter) ensure(id string) *session {
	s, ok := e.sessions[id]
	if !ok {
		s = &session{notify: make(chan struct{}, 1)}
		e.sessions[id] = s
	}
	return s
}

// Prepare creates or resets the queue for a new turn. It must be called before
// the workflow starts and before Stream, and it keeps existing history so a
// multi turn session accumulates one replayable log.
func (e *Emitter) Prepare(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.ensure(id)
	s.pending = nil
}

// Emit pushes an event onto the session queue and appends it to history. Every
// event is also chained into the decision log; the recorder skips token frames.
func (e *Emitter) Emit(id string, ev Event) {
	m := Map(ev)
	e.mu.Lock()
	s := e.ensure(id)
	s.history = append(s.history, m)
	s.push(m)
	e.mu.Unlock()
	if e.Recorder != nil {
		kind, _ := m["type"].(string)
		e.Recorder.Record(id, kind, m)
	}
}

// sentinel marks the end of a stream.
var sentinel = map[string]any{"__done__": true}

// Close signals that no more events will arrive: it records a final event in
// history and ends Stream.
func (e *Emitter) Close(id string) {
	m := Map(NewFinal(id))
	e.mu.Lock()
	s := e.ensure(id)
	s.history = append(s.history, m)
	s.push(sentinel)
	e.mu.Unlock()
	if e.Recorder != nil {
		e.Recorder.Record(id, "final", m)
	}
}

// HasHistory is true if this process has a history entry for the session, empty
// or not. History is in memory, so "never heard of" also covers every session
// lost to a restart or handled by another replica, and a caller turning history
// into an answer must tell an empty session from an absent one.
func (e *Emitter) HasHistory(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.sessions[id]
	return ok
}

// History returns every event recorded for a session. An empty result is
// ambiguous; check HasHistory when the difference matters.
func (e *Emitter) History(id string) []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sessions[id]
	if !ok {
		return nil
	}
	return append([]map[string]any(nil), s.history...)
}

// Stream yields events for a session until Close is called or ctx ends. After
// heartbeat of silence it yields a heartbeat item.
func (e *Emitter) Stream(ctx context.Context, id string, heartbeat time.Duration) <-chan Item {
	if heartbeat <= 0 {
		heartbeat = 15 * time.Second
	}
	e.mu.Lock()
	s := e.ensure(id)
	e.mu.Unlock()
	out := make(chan Item)
	go func() {
		defer close(out)
		for {
			e.mu.Lock()
			var m map[string]any
			if len(s.pending) > 0 {
				m, s.pending = s.pending[0], s.pending[1:]
			}
			e.mu.Unlock()
			if m != nil {
				if _, done := m["__done__"]; done {
					return
				}
				select {
				case out <- Item{Event: m}:
				case <-ctx.Done():
					return
				}
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-s.notify:
			case <-time.After(heartbeat):
				select {
				case out <- Item{Heartbeat: true}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// Forget drops a session's history, for tests and session cleanup.
func (e *Emitter) Forget(id string) {
	e.mu.Lock()
	delete(e.sessions, id)
	e.mu.Unlock()
}
