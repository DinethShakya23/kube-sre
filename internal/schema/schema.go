// Package schema is the full list of database migrations, in one place.
//
// Each feature owns the DDL for its own tables and registers it here, so a table
// exists only when the code that uses it does. Versions are allocated in ranges to
// keep the order obvious:
//
//	10-19  flight recorder
//	20-29  request audit
//	30-39  conversation state
//	40-59  memory
package schema

import (
	"github.com/DinethShakya23/kube-sre/internal/agent"
	"github.com/DinethShakya23/kube-sre/internal/audit"
	"github.com/DinethShakya23/kube-sre/internal/memory"
	"github.com/DinethShakya23/kube-sre/internal/recorder"
	"github.com/DinethShakya23/kube-sre/internal/store"
)

// All returns every migration, unsorted; Migrate orders them.
func All() []store.Migration {
	var out []store.Migration
	out = append(out, recorder.Migrations...)
	out = append(out, audit.Migrations...)
	out = append(out, agent.CheckpointMigrations...)
	out = append(out, memory.StoreMigrations...)
	out = append(out, memory.EpisodeMigrations...)
	out = append(out, memory.KGMigrations...)
	return out
}

// Latest is the highest version a build of this code knows.
func Latest() int {
	max := 0
	for _, m := range All() {
		if m.Version > max {
			max = m.Version
		}
	}
	return max
}
