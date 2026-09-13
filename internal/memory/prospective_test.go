package memory

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/audit"
	"github.com/DinethShakya23/kube-sre/internal/autonomy"
	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/fleet"
	"github.com/DinethShakya23/kube-sre/internal/store/storetest"
)

func prospStore(t *testing.T, env map[string]string) *Store {
	t.Helper()
	db := storetest.New(t, StoreMigrations, EpisodeMigrations, KGMigrations, ProspectiveMigrations, audit.Migrations, fleet.Migrations, autonomy.PromotionMigrations)
	s := NewStore(db, config.Load(func(k string) string { return env[k] }))
	s.Now = func() time.Time { return t0 }
	return s
}

var (
	a1   = func(l string) bool { return l != "A0" }
	prod = func(ns string) string {
		if ns == "kube-system" {
			return "A0"
		}
		return "A1"
	}
)

func due(s *Store, ns string, ago float64) {
	s.ScheduleRecheck(context.Background(), "c1", "did the fix hold in "+ns, float64(t0.Unix())-ago, ns, "CrashLoopBackOff", "recheck:"+ns, "", "watchtower")
}

func status(t *testing.T, s *Store, ns string) (string, string) {
	t.Helper()
	var st string
	var out *string
	if err := s.DB.QueryRow(s.DB.Q(`SELECT status, outcome FROM prospective_memory WHERE namespace = ?`), ns).Scan(&st, &out); err != nil {
		t.Fatal(err)
	}
	return st, deref(out)
}

func TestSchedulingIsIdempotentAndReopensAFiredCheck(t *testing.T) {
	s := prospStore(t, map[string]string{"MEMORY_PROSPECTIVE": "true"})
	ctx := context.Background()
	id1 := s.ScheduleRecheck(ctx, "c1", "check x", float64(t0.Unix())-10, "shop", "q", "k", "", "wt")
	id2 := s.ScheduleRecheck(ctx, "c1", "check x again", float64(t0.Unix())+900, "shop", "q", "k", "", "wt")
	if id1 == 0 || id1 != id2 {
		t.Fatalf("a repeat must refresh one row: %d %d", id1, id2)
	}
	var n int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM prospective_memory`).Scan(&n)
	if n != 1 {
		t.Errorf("rows %d", n)
	}
	if got := s.ScheduleRecheck(ctx, "", "x", 0, "", "", "", "", ""); got != 0 {
		t.Error("no cluster")
	}
	if got := s.ScheduleRecheck(ctx, "c1", "  ", 0, "", "", "", "", ""); got != 0 {
		t.Error("no condition")
	}

	// A fired check is reopened by a new schedule.
	s.ScheduleRecheck(ctx, "c1", "check y", float64(t0.Unix())-10, "dev", "", "ky", "", "wt")
	s.RunProspectiveOnce(ctx, prod, a1, func(context.Context, Recheck, string) string { return OutcomeResolved })
	if st, out := status(t, s, "dev"); st != "done" || out != "resolved" {
		t.Fatalf("%s %s", st, out)
	}
	s.ScheduleRecheck(ctx, "c1", "check y", float64(t0.Unix())+900, "dev", "", "ky", "", "wt")
	if st, out := status(t, s, "dev"); st != "pending" || out != "" {
		t.Errorf("reopened: %s %q", st, out)
	}
}

func TestOnlyDueChecksFireAndOnlyOnce(t *testing.T) {
	s := prospStore(t, map[string]string{"MEMORY_PROSPECTIVE": "true"})
	ctx := context.Background()
	due(s, "shop", 60)
	due(s, "later", -3600)
	calls := 0
	d := func(_ context.Context, r Recheck, level string) string {
		calls++
		if r.Namespace != "shop" || r.CheckQuery != "CrashLoopBackOff" || level != "A1" {
			t.Errorf("%+v %s", r, level)
		}
		return OutcomeStillBroken
	}
	if n := s.RunProspectiveOnce(ctx, prod, a1, d); n != 1 || calls != 1 {
		t.Fatalf("%d %d", n, calls)
	}
	if n := s.RunProspectiveOnce(ctx, prod, a1, d); n != 0 {
		t.Errorf("already fired: %d", n)
	}
	if st, out := status(t, s, "shop"); st != "done" || out != "still_broken" {
		t.Errorf("%s %s", st, out)
	}
	if st, _ := status(t, s, "later"); st != "pending" {
		t.Errorf("not yet due: %s", st)
	}
}

func TestOffModeNamespacesNeverFire(t *testing.T) {
	s := prospStore(t, map[string]string{"MEMORY_PROSPECTIVE": "true"})
	due(s, "kube-system", 10)
	s.RunProspectiveOnce(context.Background(), prod, a1, func(context.Context, Recheck, string) string { panic("must not dispatch") })
	if st, out := status(t, s, "kube-system"); st != "cancelled" || out != "skipped_a0" {
		t.Errorf("%s %s", st, out)
	}
}

func TestAnUnverifiedOrFailedCheckIsRetriedNotClosed(t *testing.T) {
	s := prospStore(t, map[string]string{"MEMORY_PROSPECTIVE": "true"})
	ctx := context.Background()
	due(s, "shop", 10)
	due(s, "api", 10)
	due(s, "web", 10)
	d := func(_ context.Context, r Recheck, _ string) string {
		switch r.Namespace {
		case "shop":
			return OutcomeUnverified
		case "api":
			panic("boom")
		}
		return OutcomeError
	}
	if n := s.RunProspectiveOnce(ctx, prod, a1, d); n != 3 {
		t.Fatalf("%d", n)
	}
	for ns, want := range map[string]string{"shop": "unverified", "api": "error", "web": "error"} {
		if st, out := status(t, s, ns); st != "pending" || out != want {
			t.Errorf("%s: %s %s", ns, st, out)
		}
	}
	// Next pass picks them up again.
	if n := s.RunProspectiveOnce(ctx, prod, a1, func(context.Context, Recheck, string) string { return OutcomeResolved }); n != 3 {
		t.Errorf("retry: %d", n)
	}
	if st, _ := status(t, s, "shop"); st != "done" {
		t.Errorf("%s", st)
	}
}

func TestProspectiveIsOffByDefaultAndReportsAFailedClaim(t *testing.T) {
	off := prospStore(t, nil)
	due(off, "shop", 10)
	if n := off.RunProspectiveOnce(context.Background(), prod, a1, nil); n != 0 {
		t.Errorf("flag off: %d", n)
	}
	on := prospStore(t, map[string]string{"MEMORY_PROSPECTIVE": "true"})
	_, _ = on.DB.Exec(`DROP TABLE prospective_memory`)
	if n := on.RunProspectiveOnce(context.Background(), prod, a1, nil); n != 0 {
		t.Errorf("%d", n)
	}
	if f := on.Live.DrainPassFailures(); len(f) != 1 || f[0][0] != "prospective_fired" {
		t.Errorf("a failed claim must be visible: %v", f)
	}
}

func TestNoDispatcherIsUnverifiedNotDone(t *testing.T) {
	s := prospStore(t, map[string]string{"MEMORY_PROSPECTIVE": "true"})
	due(s, "shop", 10)
	s.RunProspectiveOnce(context.Background(), prod, a1, nil)
	if st, out := status(t, s, "shop"); st != "pending" || out != "unverified" {
		t.Errorf("%s %s", st, out)
	}
}

func seedOld(t *testing.T, s *Store) {
	t.Helper()
	old := float64(t0.Unix()) - 40*86400
	for _, q := range []string{
		`INSERT INTO session_notes (session_id, note, created_at) VALUES ('s', 'old', ` + ftoa(old) + `), ('s', 'new', ` + ftoa(float64(t0.Unix())) + `)`,
		`INSERT INTO prospective_memory (cluster_id, condition, dedup_key, due_at, status, created_at) VALUES ('c', 'x', 'done-old', 0, 'done', ` + ftoa(old) + `)`,
		`INSERT INTO prospective_memory (cluster_id, condition, dedup_key, due_at, status, created_at) VALUES ('c', 'x', 'pending-old', 0, 'pending', ` + ftoa(old) + `)`,
	} {
		if _, err := s.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	oldTS := t0.Add(-40 * 24 * time.Hour)
	if _, err := s.DB.Exec(s.DB.Q(`INSERT INTO request_log (path, method, created_at) VALUES ('/old', 'GET', ?), ('/new', 'GET', ?)`),
		s.retentionArg(RetentionRule{}, oldTS), s.retentionArg(RetentionRule{}, t0)); err != nil {
		t.Fatal(err)
	}
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', 0, 64) }

func TestRetentionPrunesOnlyAgedFinishedRowsAndOnlyWhenAsked(t *testing.T) {
	ctx := context.Background()
	off := prospStore(t, nil)
	seedOld(t, off)
	if off.PruneOnce(ctx) != 0 {
		t.Error("retention is off by default: keeping everything is the safe default")
	}

	s := prospStore(t, map[string]string{"MEMORY_RETENTION_DAYS": "30"})
	seedOld(t, s)
	if n := s.PruneOnce(ctx); n != 3 { // old note, done-old re-check, old request
		t.Fatalf("pruned %d", n)
	}
	count := func(q string) int {
		var n int
		_ = s.DB.QueryRow(q).Scan(&n)
		return n
	}
	if count(`SELECT COUNT(*) FROM session_notes`) != 1 || count(`SELECT COUNT(*) FROM request_log`) != 1 {
		t.Error("recent rows must survive")
	}
	if count(`SELECT COUNT(*) FROM prospective_memory WHERE dedup_key = 'pending-old'`) != 1 {
		t.Error("an unfinished re-check is not prunable however old")
	}
	if s.PruneOnce(ctx) != 0 {
		t.Error("idempotent")
	}
}

func TestRetentionNeverListsAChainedOrLearnedTable(t *testing.T) {
	for _, r := range RetentionRules {
		if why, refused := RetentionRefused[r.Table]; refused {
			t.Errorf("%s is refused (%s) and must not be a rule", r.Table, why)
		}
	}
	for _, must := range []string{"decision_log", "memory_audit", "decision_log_head", "memory_chain_head", "chain_truncation", "episodes"} {
		if RetentionRefused[must] == "" {
			t.Errorf("%s must be refused with a reason", must)
		}
	}
}

func TestRetentionFloorClampsAShortSetting(t *testing.T) {
	r := RetentionRule{FloorDays: 90}
	if r.EffectiveDays(7) != 90 || r.EffectiveDays(365) != 365 || (RetentionRule{}).EffectiveDays(7) != 7 {
		t.Error("floor")
	}
}

func TestRetentionFailureIsRegistered(t *testing.T) {
	s := prospStore(t, map[string]string{"MEMORY_RETENTION_DAYS": "30"})
	_, _ = s.DB.Exec(`DROP TABLE session_notes`)
	s.PruneOnce(context.Background())
	if f := s.Live.DrainPassFailures(); len(f) != 1 || f[0][0] != "retention_pruned" {
		t.Errorf("%v", f)
	}
}
