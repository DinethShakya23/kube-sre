package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/config"
	"github.com/DinethShakya23/kube-sre/internal/store/storetest"
)

var t0 = time.Unix(1_800_000_000, 0)

func newStore(t *testing.T, env map[string]string) *Store {
	t.Helper()
	db := storetest.New(t, StoreMigrations)
	s := NewStore(db, config.Load(func(k string) string { return env[k] }))
	s.Now = func() time.Time { return t0 }
	return s
}

func exec(t *testing.T, s *Store, q string, args ...any) {
	t.Helper()
	if _, err := s.DB.Exec(s.DB.Q(q), args...); err != nil {
		t.Fatal(err)
	}
}

func pref(t *testing.T, s *Store, user, key, value, source string, conf float64, seenDaysAgo int) {
	t.Helper()
	at := float64(t0.Unix()) - float64(seenDaysAgo)*86400
	exec(t, s, `INSERT INTO user_prefs (user_id, key, value, source, confidence, updated_at, last_seen_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		user, key, value, source, conf, at, at)
}

func TestEmptyStoreGivesAnEmptyContext(t *testing.T) {
	s := newStore(t, nil)
	got, err := s.LoadContext(context.Background(), "u", "s", "c1")
	if err != nil || got != "" {
		t.Errorf("%q %v", got, err)
	}
}

func TestPreferencesExplicitAlwaysInferredOnlyWhenConfidentAndFresh(t *testing.T) {
	s := newStore(t, nil)
	pref(t, s, "u", "style", "terse", "explicit", 1.0, 400)
	pref(t, s, "u", "ns", "shop", "inferred", 0.8, 3)
	pref(t, s, "u", "stale", "x", "inferred", 0.9, 90)
	pref(t, s, "u", "weak", "y", "inferred", 0.1, 1)
	pref(t, s, "other", "secret", "z", "explicit", 1.0, 1)
	got, _ := s.LoadContext(context.Background(), "u", "s", "c1")
	if !strings.Contains(got, "## Operator Preferences (remembered)") || !strings.Contains(got, "style: terse") ||
		!strings.Contains(got, "ns: shop  (inferred, confidence 80%)") {
		t.Errorf("%s", got)
	}
	for _, leak := range []string{"stale", "weak", "secret"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q must not appear: %s", leak, got)
		}
	}
	if strings.Index(got, "style: terse") > strings.Index(got, "ns: shop") {
		t.Error("explicit first")
	}
	off := newStore(t, map[string]string{"PREFERENCE_MEMORY_ENABLED": "false"})
	pref(t, off, "u", "style", "terse", "explicit", 1, 0)
	if got, _ := off.LoadContext(context.Background(), "u", "s", "c1"); got != "" {
		t.Errorf("preference memory off: %q", got)
	}
}

func TestACappedSectionSaysItIsShort(t *testing.T) {
	s := newStore(t, nil)
	for i := 0; i < 12; i++ {
		pref(t, s, "u", fmt.Sprintf("k%02d", i), "v", "explicit", 1, 0)
	}
	got, _ := s.LoadContext(context.Background(), "u", "s", "c1")
	if !strings.Contains(got, "k07") || strings.Contains(got, "k08") || !strings.Contains(got, "MORE operator preferences are stored") ||
		!strings.Contains(got, "NOT evidence that none exists") {
		t.Errorf("%s", got)
	}
	exact := newStore(t, nil)
	for i := 0; i < 8; i++ {
		pref(t, exact, "u", fmt.Sprintf("k%02d", i), "v", "explicit", 1, 0)
	}
	if got, _ := exact.LoadContext(context.Background(), "u", "s", "c1"); strings.Contains(got, "MORE") {
		t.Errorf("exactly the cap is not a subset: %s", got)
	}
}

func TestFailureHintsAreScopedRankedAndDecay(t *testing.T) {
	s := newStore(t, nil)
	add := func(name, cluster string, conf float64, count int, daysAgo int, demoted bool) {
		at := float64(t0.Unix()) - float64(daysAgo)*86400
		exec(t, s, `INSERT INTO failure_patterns (pattern_name, cluster_id, description, recommended_fix, confidence, occurrence_count, demoted, last_seen_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, name, cluster, "desc "+name, "fix "+name, conf, count, demoted, at, at, at)
	}
	add("good", "c1", 0.95, 5, 1, false)
	add("newer", "c1", 0.92, 3, 1, false)
	add("legacy", "unknown", 0.95, 2, 1, false)
	add("other-cluster", "c2", 0.99, 9, 1, false)
	add("low-confidence", "c1", 0.5, 9, 1, false)
	add("seen-once", "c1", 0.99, 1, 1, false)
	add("demoted", "c1", 0.99, 9, 1, true)
	add("stale", "c1", 0.99, 9, 45, false)
	got, _ := s.LoadContext(context.Background(), "u", "s", "c1")
	if !strings.Contains(got, "## Known Failure Patterns (this cluster)") || !strings.Contains(got, "[good] (seen 5×) desc good\n    → Fix: fix good") ||
		!strings.Contains(got, "[newer]") || !strings.Contains(got, "[legacy]") {
		t.Errorf("%s", got)
	}
	for _, no := range []string{"other-cluster", "low-confidence", "seen-once", "demoted", "stale"} {
		if strings.Contains(got, no) {
			t.Errorf("%s must be excluded: %s", no, got)
		}
	}
	if strings.Index(got, "[good]") > strings.Index(got, "[newer]") {
		t.Error("ordered by occurrence count")
	}
}

func TestPastRCAIsClusterScopedAndTruncatesTheFixOutLoud(t *testing.T) {
	s := newStore(t, nil)
	ins := func(cause, cluster, fix string, verified any, daysAgo int) {
		exec(t, s, `INSERT INTO rca_outcomes (session_id, user_id, root_cause, confidence, recommended_fix, cluster_id, namespace, verified_resolved, created_at)
			VALUES ('s', 'u', ?, 0.9, ?, ?, 'shop', ?, ?)`, cause, fix, cluster, verified, float64(t0.Unix())-float64(daysAgo)*86400)
	}
	ins("verified fix", "c1", strings.Repeat("kubectl patch ", 30), true, 5)
	ins("unknown outcome", "c1", "short fix", nil, 1)
	ins("refuted", "c1", "bad", false, 1)
	ins("elsewhere", "c9", "x", true, 1)
	got, _ := s.LoadContext(context.Background(), "u", "s", "c1")
	if !strings.Contains(got, "## Recent RCA History (this cluster)") || !strings.Contains(got, "ns=shop ✓] verified fix") ||
		!strings.Contains(got, "ns=shop ?] unknown outcome") || !strings.Contains(got, "…[fix truncated, not the whole command]") {
		t.Errorf("%s", got)
	}
	if strings.Contains(got, "refuted") || strings.Contains(got, "elsewhere") {
		t.Errorf("%s", got)
	}
	if strings.Index(got, "verified fix") > strings.Index(got, "unknown outcome") {
		t.Error("verified first")
	}
	if !strings.Contains(got, "[2027-01-15 ") && !strings.Contains(got, "["+time.Unix(t0.Unix()-5*86400, 0).UTC().Format("2006-01-02")) {
		t.Errorf("the date: %s", got)
	}
}

func TestSessionNotes(t *testing.T) {
	s := newStore(t, nil)
	for i := 0; i < 5; i++ {
		exec(t, s, `INSERT INTO session_notes (session_id, note, created_at) VALUES ('s1', ?, ?)`, fmt.Sprintf("note %d", i), float64(i))
	}
	exec(t, s, `INSERT INTO session_notes (session_id, note, created_at) VALUES ('s2', 'other session', 1)`)
	got, _ := s.LoadContext(context.Background(), "u", "s1", "c1")
	if !strings.Contains(got, "note 4") || !strings.Contains(got, "note 2") || strings.Contains(got, "note 1") || strings.Contains(got, "other session") ||
		!strings.Contains(got, "MORE notes from this session") {
		t.Errorf("%s", got)
	}
}

func TestOneFailingSectionIsNamedNotSilent(t *testing.T) {
	s := newStore(t, nil)
	pref(t, s, "u", "style", "terse", "explicit", 1, 0)
	exec(t, s, `DROP TABLE failure_patterns`)
	got, err := s.LoadContext(context.Background(), "u", "s", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "## Memory partially unavailable") || !strings.Contains(got, "Stored failure hints could NOT be read") ||
		!strings.Contains(got, "style: terse") {
		t.Errorf("%s", got)
	}
	if !strings.Contains(got, "not the same as there being none") {
		t.Error("could not load is not has none")
	}
}

func TestAnUnreachableStoreIsAnErrorNotACleanSlate(t *testing.T) {
	s := newStore(t, nil)
	s.DB.Close()
	got, err := s.LoadContext(context.Background(), "u", "s", "c1")
	if !errors.Is(err, ErrUnavailable) || got != "" {
		t.Errorf("%q %v", got, err)
	}
	if !strings.Contains(UnavailableNotice(), "not the same as there being none") {
		t.Error("notice")
	}
}

func TestContextIsCappedAndTheFailureNoticeSurvivesTheCap(t *testing.T) {
	s := newStore(t, nil)
	for i := 0; i < 8; i++ {
		pref(t, s, "u", fmt.Sprintf("key%d", i), strings.Repeat("long value ", 40), "explicit", 1, 0)
	}
	got, _ := s.LoadContext(context.Background(), "u", "s", "c1")
	if len([]rune(got)) > maxContextChars+40 || !strings.HasSuffix(got, "... [context truncated]") {
		t.Errorf("%d", len(got))
	}
	exec(t, s, `DROP TABLE rca_outcomes`)
	got, _ = s.LoadContext(context.Background(), "u", "s", "c1")
	if !strings.HasPrefix(got, "## Memory partially unavailable") {
		t.Error("the notice goes first so the cap cannot remove it")
	}
}

func outcome(cause string, conf float64, verified bool, feedback string) RCAOutcome {
	o := RCAOutcome{SessionID: "s", UserID: "u", RootCause: cause, Confidence: conf, RecommendedFix: "fix " + cause, ClusterID: "c1",
		Namespace: "shop", Playbooks: []string{"Evicted"}, Role: "admin", RequestID: "r1"}
	o.Verified = &verified
	if feedback != "" {
		o.Feedback = &feedback
	}
	return o
}

func count(t *testing.T, s *Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(s.DB.Q(q), args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOutcomeIsAlwaysStoredButOnlyVerifiedConfidentOnesSeedPatterns(t *testing.T) {
	s := newStore(t, nil)
	ctx := context.Background()
	s.RecordOutcome(ctx, outcome("low", 0.8, true, "resolved"))
	s.RecordOutcome(ctx, outcome("unverified", 0.95, false, "partial"))
	s.RecordOutcome(ctx, outcome("regression", 0.95, true, "regression"))
	s.RecordOutcome(ctx, RCAOutcome{SessionID: "s", UserID: "u", RootCause: "unknown verification", Confidence: 0.99, RecommendedFix: "f"})
	if n := count(t, s, `SELECT COUNT(*) FROM rca_outcomes`); n != 4 {
		t.Errorf("every outcome is stored: %d", n)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM failure_patterns`); n != 0 {
		t.Errorf("none is eligible: %d", n)
	}
	s.RecordOutcome(ctx, outcome("good", 0.95, true, "resolved"))
	if n := count(t, s, `SELECT COUNT(*) FROM failure_patterns WHERE pattern_name = 'good' AND occurrence_count = 1 AND cluster_id = 'c1'`); n != 1 {
		t.Error("seeded")
	}
	var role, pb string
	var ver bool
	if err := s.DB.QueryRow(s.DB.Q(`SELECT created_by_role, playbooks_matched, verified_resolved FROM rca_outcomes WHERE root_cause = 'good'`)).Scan(&role, &pb, &ver); err != nil ||
		role != "admin" || pb != `["Evicted"]` || !ver {
		t.Errorf("%s %s %v %v", role, pb, ver, err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM rca_outcomes WHERE root_cause = 'unknown verification' AND verified_resolved IS NULL AND cluster_id = 'unknown'`); n != 1 {
		t.Error("unknown stays NULL and an empty cluster is unknown")
	}
}

func TestCooldownStopsTheCounterInflatingAndThenItBumps(t *testing.T) {
	s := newStore(t, nil)
	ctx := context.Background()
	s.RecordOutcome(ctx, outcome("p", 0.92, true, "resolved"))
	o := outcome("p", 0.96, true, "resolved")
	o.RecommendedFix = "newer fix"
	s.RecordOutcome(ctx, o)
	var c int
	var fix string
	var conf float64
	read := func() {
		if err := s.DB.QueryRow(s.DB.Q(`SELECT occurrence_count, recommended_fix, confidence FROM failure_patterns WHERE pattern_name = 'p'`)).Scan(&c, &fix, &conf); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if c != 1 || fix != "newer fix" || conf != 0.96 {
		t.Errorf("inside the cooldown the count holds, the fix refreshes and confidence only rises: %d %s %v", c, fix, conf)
	}
	s.Now = func() time.Time { return t0.Add(2 * time.Hour) }
	s.RecordOutcome(ctx, outcome("p", 0.93, true, "resolved"))
	read()
	if c != 2 || conf != 0.96 {
		t.Errorf("after the cooldown it bumps and confidence never drops: %d %v", c, conf)
	}
	s.DemotePattern(ctx, "p", "c1")
	if n := count(t, s, `SELECT COUNT(*) FROM failure_patterns WHERE demoted = ?`, true); n != 1 {
		t.Error("demoted")
	}
	got, _ := s.LoadContext(ctx, "u", "s", "c1")
	if strings.Contains(got, "[p]") {
		t.Error("a demoted pattern stops being injected")
	}
	s.Now = func() time.Time { return t0.Add(5 * time.Hour) }
	s.RecordOutcome(ctx, outcome("p", 0.95, true, "resolved"))
	if n := count(t, s, `SELECT COUNT(*) FROM failure_patterns WHERE demoted = ?`, false); n != 1 {
		t.Error("a fresh verified outcome re-promotes it")
	}
}

func TestPatternsAreKeyedByClusterAndTheNameIsClipped(t *testing.T) {
	s := newStore(t, nil)
	ctx := context.Background()
	s.RecordOutcome(ctx, outcome("same name", 0.95, true, "resolved"))
	o := outcome("same name", 0.95, true, "resolved")
	o.ClusterID = "c2"
	s.RecordOutcome(ctx, o)
	if n := count(t, s, `SELECT COUNT(*) FROM failure_patterns WHERE pattern_name = 'same name'`); n != 2 {
		t.Errorf("two clusters, two patterns: %d", n)
	}
	long := outcome(strings.Repeat("x", 300), 0.95, true, "resolved")
	s.RecordOutcome(ctx, long)
	if n := count(t, s, `SELECT COUNT(*) FROM failure_patterns WHERE LENGTH(pattern_name) = 120`); n != 1 {
		t.Error("the pattern name is 120 characters")
	}
}

func TestRecordingNeverPanicsOrReturnsAnErrorWhenTheStoreIsDown(t *testing.T) {
	s := newStore(t, nil)
	s.DB.Close()
	s.RecordOutcome(context.Background(), outcome("x", 0.99, true, "resolved"))
	s.DemotePattern(context.Background(), "x", "c1")
}
