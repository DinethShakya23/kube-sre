package store

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
)

// SchemaState classifies the database against the migrations this build ships, so a
// stale or downgraded database is loud instead of silent. Every memory, recorder and
// audit write is fire and forget by design, so a missing column becomes a logged
// warning inside a swallowed exception: memory silently stops recording, /healthz
// keeps saying enabled, and the first symptom is an empty table weeks later.
//
//	current     every migration this build ships is applied and nothing unknown is
//	stale       a shipped migration is not applied; columns this build writes may not exist
//	ahead       the database holds a migration this build does not know: the deployment was
//	            rolled back and the database was not, the dangerous direction
//	unrecorded  no migration is recorded at all
//	unknown     the check itself could not run; not a verdict, see reason
func (d *DB) SchemaState(ctx context.Context, shipped []Migration) map[string]any {
	expected := 0
	known := map[int]bool{}
	for _, m := range shipped {
		known[m.Version] = true
		if m.Version > expected {
			expected = m.Version
		}
	}
	out := func(state string, applied *int, reason string) map[string]any {
		var a any
		if applied != nil {
			a = *applied
		}
		return map[string]any{"state": state, "expected_version": expected, "applied_version": a, "matches": state == "current", "reason": reason}
	}
	rows, err := d.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		slog.Warn("schema check could not run", "err", err)
		return out("unknown", nil, "could not read schema_migrations: "+err.Error())
	}
	var applied []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return out("unknown", nil, "could not read schema_migrations: "+err.Error())
		}
		applied = append(applied, v)
	}
	rows.Close()
	if len(applied) == 0 {
		reason := "no row in schema_migrations, so this database has never had `kube-sre db-init` run against it; the schema this build writes to is unverified"
		slog.Warn("schema: " + reason)
		return out("unrecorded", nil, reason)
	}
	sort.Ints(applied)
	top := applied[len(applied)-1]
	have := map[int]bool{}
	var unknownApplied, missing []int
	for _, v := range applied {
		have[v] = true
		if !known[v] {
			unknownApplied = append(unknownApplied, v)
		}
	}
	for _, m := range shipped {
		if !have[m.Version] {
			missing = append(missing, m.Version)
		}
	}
	sort.Ints(missing)
	switch {
	case len(unknownApplied) > 0:
		reason := fmt.Sprintf("the database holds migration(s) %v that this build does not ship, and is at v%d against this build's v%d; the deployment was rolled back and "+
			"the database was not. Running this build's DDL will NOT restore the older shape.", unknownApplied, top, expected)
		slog.Warn("schema: ahead, " + reason)
		return out("ahead", &top, reason)
	case len(missing) > 0:
		reason := fmt.Sprintf("migration(s) %v have not been applied. Columns this build writes to may not exist, and every memory, recorder and audit write is "+
			"fire and forget, so the failure is logged and swallowed and not raised. Run `kube-sre db-init`.", missing)
		slog.Warn("schema: stale, " + reason)
		return out("stale", &top, reason)
	}
	slog.Info("schema is current", "version", top)
	return out("current", &top, "")
}
