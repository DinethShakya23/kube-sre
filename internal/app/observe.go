package app

import (
	"context"
	"sync/atomic"

	"github.com/DinethShakya23/kube-sre/internal/memory"
	"github.com/DinethShakya23/kube-sre/internal/sensorium"
)

// graphFeed moves observations into the knowledge graph off the watch path. The
// queue is bounded and sheds when full; the drop count is reported, because a
// graph that trails the cluster must not read as a graph that is current.
type graphFeed struct {
	store   *memory.Store
	queue   chan sensorium.Observation
	dropped atomic.Int64
}

func newGraphFeed(s *memory.Store, size int) *graphFeed {
	if size <= 0 {
		size = 1000
	}
	return &graphFeed{store: s, queue: make(chan sensorium.Observation, size)}
}

func (g *graphFeed) Offer(o sensorium.Observation) {
	if o.Kind != "pod_status" {
		return
	}
	select {
	case g.queue <- o:
	default:
		g.dropped.Add(1)
	}
}

func (g *graphFeed) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case o := <-g.queue:
			g.store.IngestPodObservation(ctx, o)
		}
	}
}

func (g *graphFeed) Dropped() int64 { return g.dropped.Load() }
