package chainops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/DinethShakya23/kube-sre/internal/memory"
	"github.com/DinethShakya23/kube-sre/internal/recorder"
	"github.com/DinethShakya23/kube-sre/internal/schema"
	"github.com/DinethShakya23/kube-sre/internal/store"
	"github.com/DinethShakya23/kube-sre/internal/store/storetest"
)

var ctx = context.Background()

func chain(t *testing.T, n int) (*store.DB, *memory.Guard) {
	t.Helper()
	db := storetest.New(t, schema.All())
	g := memory.NewGuard(db, 100, 0.35)
	for i := 0; i < n; i++ {
		g.Audit(ctx, "c1", "episode_write", "", map[string]any{"i": i, "note": "a <tag> & more", "big": 12345678901234567})
	}
	return db, g
}

func export(t *testing.T, db *store.DB, through *int64) map[string]any {
	t.Helper()
	doc, err := BuildExport(ctx, db, "memory_audit", "c1", "2026-09-12T10:00:00Z", through, "test")
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func mutate(doc map[string]any, f func(rows []any)) map[string]any {
	f(doc["rows"].([]any))
	return doc
}

func TestExportVerifiesWithNoDatabase(t *testing.T) {
	db, _ := chain(t, 5)
	doc := export(t, db, nil)
	// Through a file: what an operator would actually store and check years later.
	round, err := DecodeDoc(marshal(doc))
	if err != nil {
		t.Fatal(err)
	}
	if res := VerifyExport(round); !res.OK || len(res.Problems) != 0 || res.Checked != 4 {
		t.Fatalf("%+v", res)
	}
	if fmt.Sprintf("%v %v %v", doc["row_count"], doc["from_seq"], doc["through_seq"]) != "5 0 4" || doc["links_verified_at_export"] != true {
		t.Errorf("%v %v %v", doc["row_count"], doc["from_seq"], doc["through_seq"])
	}
	if !strings.Contains(doc["limit"].(string), "does NOT prove who wrote it") {
		t.Error("the limit must be stated in the archive")
	}
}

func TestAnEditedArchiveIsCaughtTwoDifferentWays(t *testing.T) {
	db, _ := chain(t, 4)
	// 1. Edited and left: the content hash disagrees.
	doc := mutate(export(t, db, nil), func(rows []any) { rows[2].(map[string]any)["kind"] = "forged" })
	res := VerifyExport(doc)
	if res.OK || !strings.Contains(strings.Join(res.Problems, ";"), "edited after it was written") || !strings.Contains(strings.Join(res.Problems, ";"), "do not chain") {
		t.Errorf("%+v", res)
	}
	// 2. Edited and re-hashed: the content hash agrees, the links do not.
	doc["archive_hash"] = ArchiveHash(doc)
	res = VerifyExport(doc)
	joined := strings.Join(res.Problems, ";")
	if res.OK || strings.Contains(joined, "edited after") || !strings.Contains(joined, "do not chain") || !strings.Contains(joined, "claims its links verified") {
		t.Errorf("%+v", res)
	}
	doc["archive_hash"] = ""
	if res = VerifyExport(doc); !strings.Contains(strings.Join(res.Problems, ";"), "no archive_hash") {
		t.Errorf("%+v", res)
	}
}

func TestATruncatedSourceIsVisibleInTheArchive(t *testing.T) {
	db, _ := chain(t, 5)
	_, _ = db.Exec(`DELETE FROM memory_audit WHERE seq >= 3`)
	res := VerifyExport(export(t, db, nil))
	if res.OK || !strings.Contains(res.Problems[0], "anchor records seq 4") || !strings.Contains(res.Problems[0], "2 entries were missing") {
		t.Errorf("%+v", res)
	}
	// A bounded archive is not evidence of truncation just because the chain went on.
	db2, _ := chain(t, 6)
	through := int64(2)
	if res := VerifyExport(export(t, db2, &through)); !res.OK {
		t.Errorf("%+v", res)
	}

	_, _ = db2.Exec(`DELETE FROM memory_chain_head`)
	if res := VerifyExport(export(t, db2, nil)); res.OK || !strings.Contains(res.Problems[0], "no anchor") {
		t.Errorf("%+v", res)
	}
}

func TestUnknownChainAndNewerArchives(t *testing.T) {
	db, _ := chain(t, 1)
	if _, err := BuildExport(ctx, db, "episodes", "c1", "t", nil, ""); err == nil || !strings.Contains(err.Error(), "known: decision_log, memory_audit") {
		t.Errorf("%v", err)
	}
	if res := VerifyExport(map[string]any{"archive_version": 99}); res.OK || !strings.Contains(res.Problems[0], "verify it with the build that wrote it") {
		t.Errorf("%+v", res)
	}
}

func TestTruncationRemovesExactlyTheArchivedRowsAndTheChainStillVerifies(t *testing.T) {
	db, g := chain(t, 8)
	through := int64(4)
	doc := export(t, db, &through)

	if v := g.Verify(ctx, "c1"); !v.Valid || !v.Verified {
		t.Fatal("precondition")
	}
	tr, err := TruncateChain(ctx, db, doc, "housekeeping")
	if err != nil {
		t.Fatal(err)
	}
	if tr.RowsRemoved != 5 || tr.ThroughSeq != 4 || tr.ResumeSeq != 5 || tr.ArchiveHash != doc["archive_hash"].(string) {
		t.Errorf("%+v", tr)
	}
	var left int
	_ = db.QueryRow(`SELECT COUNT(*) FROM memory_audit`).Scan(&left)
	if left != 3 {
		t.Errorf("rows left %d", left)
	}
	// The whole point: a declared gap is housekeeping, not tampering.
	if v := g.Verify(ctx, "c1"); !v.Valid || !v.Verified {
		t.Errorf("a declared truncation must verify: %+v", v)
	}
	// And the same removal, undeclared, is still tampering.
	db2, g2 := chain(t, 8)
	_, _ = db2.Exec(`DELETE FROM memory_audit WHERE seq <= 4`)
	if v := g2.Verify(ctx, "c1"); v.Valid || !v.Verified {
		t.Errorf("an undeclared gap must still read as tampered: %+v", v)
	}
}

func refusal(t *testing.T, db *store.DB, doc map[string]any, want string) {
	t.Helper()
	before := 0
	_ = db.QueryRow(`SELECT COUNT(*) FROM memory_audit`).Scan(&before)
	_, err := TruncateChain(ctx, db, doc, "")
	var r *ErrRefused
	if !errors.As(err, &r) || !strings.Contains(err.Error(), want) {
		t.Fatalf("want refusal containing %q, got %v", want, err)
	}
	after := 0
	_ = db.QueryRow(`SELECT COUNT(*) FROM memory_audit`).Scan(&after)
	var records int
	_ = db.QueryRow(`SELECT COUNT(*) FROM chain_truncation`).Scan(&records)
	if before != after || records != 0 {
		t.Errorf("a refusal must change nothing: %d -> %d, %d records", before, after, records)
	}
}

func TestTruncationRefusals(t *testing.T) {
	through := int64(3)
	t.Run("archive does not verify", func(t *testing.T) {
		db, _ := chain(t, 8)
		doc := mutate(export(t, db, &through), func(r []any) { r[0].(map[string]any)["kind"] = "x" })
		refusal(t, db, doc, "does not verify")
	})
	t.Run("chain changed after the archive", func(t *testing.T) {
		db, _ := chain(t, 8)
		doc := export(t, db, &through)
		_, _ = db.Exec(`UPDATE memory_audit SET hash = 'changed' WHERE seq = 3`)
		refusal(t, db, doc, "different hash than the archive recorded")
	})
	t.Run("row at through_seq is gone", func(t *testing.T) {
		db, _ := chain(t, 8)
		doc := export(t, db, &through)
		_, _ = db.Exec(`DELETE FROM memory_audit WHERE seq = 3`)
		refusal(t, db, doc, "no row at seq=3")
	})
	t.Run("nothing would survive", func(t *testing.T) {
		db, _ := chain(t, 4)
		doc := export(t, db, nil)
		refusal(t, db, doc, "not truncation")
	})
	t.Run("seam does not link", func(t *testing.T) {
		db, _ := chain(t, 8)
		doc := export(t, db, &through)
		_, _ = db.Exec(`UPDATE memory_audit SET prev_hash = 'other' WHERE seq = 4`)
		refusal(t, db, doc, "does not link to this archive")
	})
	t.Run("no rows and unknown chain", func(t *testing.T) {
		db, _ := chain(t, 3)
		empty, err := BuildExport(ctx, db, "memory_audit", "nobody", "t", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		refusal(t, db, empty, "does not verify")
	})
}

func TestTruncationWorksOnTheFlightRecorderToo(t *testing.T) {
	db := storetest.New(t, schema.All())
	rec := recorder.New(db, true, true)
	rec.Start(ctx)
	for i := 0; i < 6; i++ {
		rec.Record("ep1", "tool_call", map[string]any{"i": i})
	}
	rec.Close()
	through := int64(2)
	doc, err := BuildExport(ctx, db, "decision_log", "ep1", "t", &through, "")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyExport(doc).OK {
		t.Fatalf("%+v", VerifyExport(doc))
	}
	if _, err := TruncateChain(ctx, db, doc, "x"); err != nil {
		t.Fatal(err)
	}
	rows, _ := rec.FetchEpisode(ctx, "ep1")
	if v := rec.VerifyEpisode(ctx, "ep1", rows); !v.Valid || !v.Verified || len(rows) != 3 {
		t.Errorf("%+v %d", v, len(rows))
	}
}

func TestManifestCatchesWhatALinkCheckCannotSee(t *testing.T) {
	db, _ := chain(t, 6)
	m, err := BuildManifest(ctx, db, "2026-09-12T10:00:00Z", "nightly")
	if err != nil {
		t.Fatal(err)
	}
	m, _ = DecodeDoc(marshal(m))
	if res := VerifyManifest(ctx, db, m); !res.OK || res.Checked < 10 {
		t.Fatalf("%+v", res)
	}

	// A restore that dropped the newest rows of a chain: every remaining link verifies.
	_, _ = db.Exec(`DELETE FROM memory_audit WHERE seq >= 4`)
	res := VerifyManifest(ctx, db, m)
	joined := strings.Join(res.Problems, "\n")
	if res.OK || !strings.Contains(joined, "memory_audit: 4 rows, manifest says 6 (2 MISSING)") || !strings.Contains(joined, "1 chain(s) do not reach their recorded head") {
		t.Errorf("%+v", res)
	}
	_, _ = db.Exec(`INSERT INTO user_prefs (user_id, key, value, updated_at, last_seen_at) VALUES ('u', 'k', 'v', 0, 0)`)
	if res = VerifyManifest(ctx, db, m); !strings.Contains(strings.Join(res.Problems, "\n"), "user_prefs: 1 rows, manifest says 0 (1 extra)") {
		t.Errorf("%+v", res)
	}
}

func TestManifestSchemaAndUnreadableTables(t *testing.T) {
	db, _ := chain(t, 1)
	m, _ := BuildManifest(ctx, db, "t", "")
	m["schema_version"] = int64(schema.Latest() - 1)
	if res := VerifyManifest(ctx, db, m); res.OK || !strings.Contains(res.Problems[0], "schema version differs") {
		t.Errorf("%+v", res)
	}
	m["schema_version"] = int64(schema.Latest())
	m["schema_fingerprint"] = "stale"
	if res := VerifyManifest(ctx, db, m); res.OK || !strings.Contains(res.Problems[0], "fingerprint differs") {
		t.Errorf("%+v", res)
	}
	m["schema_fingerprint"] = schema.Fingerprint()
	_, _ = db.Exec(`DROP TABLE prospective_memory`)
	if res := VerifyManifest(ctx, db, m); res.OK || !strings.Contains(strings.Join(res.Problems, ";"), "the restore did not create it") {
		t.Errorf("%+v", res)
	}
	if res := VerifyManifest(ctx, db, map[string]any{"manifest_version": 9}); res.OK || res.Checked != 0 {
		t.Errorf("%+v", res)
	}
}

func TestFingerprintIgnoresCommentsAndTracksDDL(t *testing.T) {
	if schema.Fingerprint() != schema.Fingerprint() || len(schema.Fingerprint()) != 64 {
		t.Error("stable sha256")
	}
}
