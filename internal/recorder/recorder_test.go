package recorder

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DinethShakya23/kube-sre/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background(), Migrations); err != nil {
		t.Fatal(err)
	}
	return db
}

func started(t *testing.T, db *store.DB) *Recorder {
	t.Helper()
	r := New(db, true, true)
	r.Start(context.Background())
	t.Cleanup(r.Close)
	return r
}

// waitRows polls until an episode has n rows, since writes are asynchronous.
func waitRows(t *testing.T, r *Recorder, ep string, n int) []Row {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := r.FetchEpisode(context.Background(), ep)
		if err == nil && len(rows) >= n {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("wanted %d rows for %s, got %d (err=%v)", n, ep, len(rows), err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestChainIsContiguousAndVerifies(t *testing.T) {
	r := started(t, testDB(t))
	for i := 0; i < 5; i++ {
		r.Record("ep1", "finding", map[string]any{"n": i, "note": "hello"})
	}
	rows := waitRows(t, r, "ep1", 5)
	for i, row := range rows {
		if row.Seq != int64(i) {
			t.Errorf("seq %d at index %d", row.Seq, i)
		}
	}
	if rows[0].PrevHash != "" || rows[1].PrevHash != rows[0].Hash {
		t.Error("links are wrong")
	}
	if !VerifyChain(rows, 0, "") {
		t.Error("an untouched chain must verify")
	}
	if v := r.VerifyEpisode(context.Background(), "ep1", rows); !v.Valid || !v.Verified {
		t.Errorf("verdict %+v", v)
	}
}

func TestEpisodesAreIndependentAndTokensSkipped(t *testing.T) {
	r := started(t, testDB(t))
	r.Record("a", "token", map[string]any{"t": "x"})
	r.Record("a", "finding", map[string]any{"k": 1})
	r.Record("b", "finding", map[string]any{"k": 2})
	a := waitRows(t, r, "a", 1)
	b := waitRows(t, r, "b", 1)
	time.Sleep(700 * time.Millisecond)
	a, _ = r.FetchEpisode(context.Background(), "a")
	if len(a) != 1 || a[0].Kind != "finding" || b[0].Seq != 0 {
		t.Errorf("a=%+v b=%+v", a, b)
	}
}

func TestEditedRowBreaksTheChain(t *testing.T) {
	db := testDB(t)
	r := started(t, db)
	for i := 0; i < 4; i++ {
		r.Record("ep", "e", map[string]any{"i": i})
	}
	waitRows(t, r, "ep", 4)
	if _, err := db.Exec(`UPDATE decision_log SET payload = '{"i":99}' WHERE seq = 2`); err != nil {
		t.Fatal(err)
	}
	rows, _ := r.FetchEpisode(context.Background(), "ep")
	if v := r.VerifyEpisode(context.Background(), "ep", rows); v.Valid {
		t.Error("an edited payload must fail verification")
	}
}

func TestTruncationOfNewestRowsIsCaughtByTheAnchor(t *testing.T) {
	db := testDB(t)
	r := started(t, db)
	for i := 0; i < 4; i++ {
		r.Record("ep", "e", map[string]any{"i": i})
	}
	waitRows(t, r, "ep", 4)
	if _, err := db.Exec(`DELETE FROM decision_log WHERE seq >= 2`); err != nil {
		t.Fatal(err)
	}
	rows, _ := r.FetchEpisode(context.Background(), "ep")
	if !VerifyChain(rows, 0, "") {
		t.Fatal("the links alone still verify after a tail delete, that is the point")
	}
	v := r.VerifyEpisode(context.Background(), "ep", rows)
	if v.Valid || !v.Verified {
		t.Errorf("the head anchor must catch it: %+v", v)
	}
}

func TestRemovedFrontIsTamperedUnlessDeclared(t *testing.T) {
	db := testDB(t)
	r := started(t, db)
	for i := 0; i < 5; i++ {
		r.Record("ep", "e", map[string]any{"i": i})
	}
	all := waitRows(t, r, "ep", 5)
	if _, err := db.Exec(`DELETE FROM decision_log WHERE seq < 2`); err != nil {
		t.Fatal(err)
	}
	rows, _ := r.FetchEpisode(context.Background(), "ep")
	if v := r.VerifyEpisode(context.Background(), "ep", rows); v.Valid {
		t.Error("a front removed with no declaration is tampering")
	}
	bad := all[2].PrevHash
	if _, err := db.Exec(db.Q(`INSERT INTO chain_truncation (chain, scope_id, through_seq, resume_seq, resume_prev_hash, archive_hash)
		VALUES ('decision_log', 'ep', 1, 2, ?, 'abc')`), "wrong"); err != nil {
		t.Fatal(err)
	}
	if v := r.VerifyEpisode(context.Background(), "ep", rows); v.Valid {
		t.Error("a record that does not describe the rows is itself a contradiction")
	}
	if _, err := db.Exec(db.Q(`UPDATE chain_truncation SET resume_prev_hash = ?`), bad); err != nil {
		t.Fatal(err)
	}
	if v := r.VerifyEpisode(context.Background(), "ep", rows); !v.Valid || !v.Verified {
		t.Errorf("a consistent declaration should verify: %+v", v)
	}
}

func TestLostBatchLeavesNoSeqGapAndIsWrittenIntoTheChain(t *testing.T) {
	db := testDB(t)
	r := started(t, db)
	r.Record("ep", "e", map[string]any{"n": 1})
	waitRows(t, r, "ep", 1)

	if _, err := db.Exec(`ALTER TABLE decision_log RENAME TO decision_log_away`); err != nil {
		t.Fatal(err)
	}
	r.Record("ep", "e", map[string]any{"n": 2})
	r.Record("ep", "e", map[string]any{"n": 3})
	time.Sleep(1200 * time.Millisecond)
	if _, err := db.Exec(`ALTER TABLE decision_log_away RENAME TO decision_log`); err != nil {
		t.Fatal(err)
	}
	r.Record("ep", "e", map[string]any{"n": 4})

	rows := waitRows(t, r, "ep", 3)
	var gap *Row
	for i := range rows {
		if rows[i].Seq != int64(i) {
			t.Fatalf("a database blip must not leave a seq gap: %+v", rows)
		}
		if rows[i].Kind == GapKind {
			gap = &rows[i]
		}
	}
	if gap == nil {
		t.Fatalf("the loss must be written into the chain: %+v", rows)
	}
	if gap.Payload["dropped"] == nil || !strings.Contains(gap.Payload["message"].(string), "LOST") {
		t.Errorf("gap payload: %v", gap.Payload)
	}
	if v := r.VerifyEpisode(context.Background(), "ep", rows); !v.Valid || !v.Verified {
		t.Errorf("chain with a gap record is intact and incomplete: %+v", v)
	}
}

func TestSecretsAreScrubbedAtAnyDepthAndKeysStayDistinct(t *testing.T) {
	r := started(t, testDB(t))
	r.Record("ep", "act", map[string]any{
		"cmd":   "kubectl get pods --token=abcdefghij1234567890abcdefghij1234567890",
		"steps": []any{map[string]any{"description": "password: hunter2"}},
		"attributes": map[string]any{
			"a --token=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA": "ok",
			"b --token=BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB": "ok",
		},
		"token": "plain",
	})
	rows := waitRows(t, r, "ep", 1)
	b := rows[0].MarshalPayload()
	for _, leak := range []string{"hunter2", "AAAAAAAA", "BBBBBBBB", "abcdefghij1234"} {
		if strings.Contains(b, leak) {
			t.Errorf("%q leaked: %s", leak, b)
		}
	}
	attrs := rows[0].Payload["attributes"].(map[string]any)
	if len(attrs) != 2 {
		t.Errorf("two keys that redact alike must stay two: %v", attrs)
	}
	if _, ok := rows[0].Payload["token"]; !ok {
		t.Error("the ordinary field name token must survive")
	}
	if !VerifyChain(rows, 0, "") {
		t.Error("a scrubbed row must still verify")
	}
}

func TestDeepNestingIsReplacedNotLeaked(t *testing.T) {
	deep := any("password: hunter2 secret")
	for i := 0; i < 9; i++ {
		deep = map[string]any{"k": deep}
	}
	got := scrub(map[string]any{"x": deep})
	if strings.Contains(mustJSON(t, got), "hunter2") {
		t.Error("deep payload leaked")
	}
	if !strings.Contains(mustJSON(t, got), tooDeep) {
		t.Error("a subtree past the bound is replaced")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	s, err := marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDisabledRecorderDoesNothing(t *testing.T) {
	db := testDB(t)
	r := New(db, false, true)
	r.Start(context.Background())
	r.Record("ep", "e", map[string]any{"a": 1})
	time.Sleep(200 * time.Millisecond)
	rows, _ := r.FetchEpisode(context.Background(), "ep")
	if len(rows) != 0 {
		t.Error("nothing should be written when disabled")
	}
	if s := r.Status(); s["state"] != "flag" || s["enabled"] != false {
		t.Errorf("status %v", s)
	}
}

func TestChainResumesAcrossRestartsAndPastAHead(t *testing.T) {
	db := testDB(t)
	r := started(t, db)
	r.Record("ep", "e", map[string]any{"n": 1})
	r.Record("ep", "e", map[string]any{"n": 2})
	waitRows(t, r, "ep", 2)
	r.Close()

	r2 := New(db, true, true)
	r2.Start(context.Background())
	defer r2.Close()
	r2.Record("ep", "e", map[string]any{"n": 3})
	rows := waitRows(t, r2, "ep", 3)
	if !VerifyChain(rows, 0, "") || rows[2].Seq != 2 {
		t.Errorf("chain must continue from the persisted head: %+v", rows)
	}
}

func TestStatusReportsLossWhileDown(t *testing.T) {
	db := testDB(t)
	r := New(db, true, true)
	r.mu.Lock()
	r.state, r.reason = "unavailable", "connection refused"
	r.mu.Unlock()
	r.Record("ep", "e", map[string]any{"a": 1})
	r.Record("ep", "e", map[string]any{"a": 2})
	s := r.Status()
	if s["lost_while_down"].(int64) != 2 || s["enabled"] != false {
		t.Errorf("status %v", s)
	}
	r.mu.Lock()
	g := r.pending["ep"]
	r.mu.Unlock()
	if g.count != 2 || g.reason != "connection refused" {
		t.Errorf("gap ledger %+v", g)
	}
}

func TestHashIsStableAndOrderIndependent(t *testing.T) {
	a := ComputeHash("p", "e", 1, "k", map[string]any{"a": 1, "b": "x"})
	b := ComputeHash("p", "e", 1, "k", map[string]any{"b": "x", "a": 1})
	if a != b || len(a) != 64 {
		t.Errorf("%s %s", a, b)
	}
	if a == ComputeHash("q", "e", 1, "k", map[string]any{"a": 1, "b": "x"}) {
		t.Error("prev hash must matter")
	}
}
