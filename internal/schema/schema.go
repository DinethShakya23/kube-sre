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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/DinethShakya23/kube-sre/internal/agent"
	"github.com/DinethShakya23/kube-sre/internal/audit"
	"github.com/DinethShakya23/kube-sre/internal/detectstore"
	"github.com/DinethShakya23/kube-sre/internal/fleet"
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
	out = append(out, memory.GuardMigrations...)
	out = append(out, memory.RuleMigrations...)
	out = append(out, memory.ProspectiveMigrations...)
	out = append(out, detectstore.Migrations...)
	out = append(out, fleet.Migrations...)
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

// Fingerprint is a SHA-256 over every shipped migration's DDL, in version order, with
// comments and blank lines removed so a reworded comment is not reported as drift.
func Fingerprint() string {
	ms := All()
	sort.Slice(ms, func(i, j int) bool { return ms[i].Version < ms[j].Version })
	h := sha256.New()
	for _, m := range ms {
		fmt.Fprintf(h, "%d:%s\n", m.Version, ddlOnly(m.SQL))
	}
	return hex.EncodeToString(h.Sum(nil))
}

var commentRe = regexp.MustCompile(`--[^\n]*`)

func ddlOnly(sql string) string {
	var out []string
	for _, ln := range strings.Split(commentRe.ReplaceAllString(sql, ""), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}
