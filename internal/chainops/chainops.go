// Package chainops is what an operator does with the hash chains: take a verifiable
// copy off the box, check one with no database present, remove exactly what an
// archive holds after declaring the gap, and prove after a restore that everything
// came back.
//
// The retention pass refuses to touch decision_log and memory_audit because they are
// tamper evidence: deleting the newest rows breaks no link. This is the deliberate,
// manual path that makes shortening one legitimate.
package chainops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/DinethShakya23/kube-sre/internal/recorder"
	"github.com/DinethShakya23/kube-sre/internal/schema"
	"github.com/DinethShakya23/kube-sre/internal/store"
)

const ArchiveVersion = 1

// ArchiveLimit states what a content hash is and is not, so "signed export" cannot
// imply more than it does. Nothing here manages a signing key, and inventing one
// would add distribution, rotation and revocation to a retention feature.
const ArchiveLimit = "A content hash proves this archive has not been edited since it was written. It does NOT prove who wrote it: anyone who can " +
	"rewrite the archive can recompute the hash. Store the archive where the database's own operators cannot silently replace it — that, not this " +
	"field, is what makes it evidence."

// Chain describes one hash chained ledger: its rows, its anchor and what scopes it.
type Chain struct{ Table, Anchor, Key string }

var Chains = map[string]Chain{
	"decision_log": {"decision_log", "decision_log_head", "episode_id"},
	"memory_audit": {"memory_audit", "memory_chain_head", "cluster_id"},
}

func chainNames() string {
	var n []string
	for k := range Chains {
		n = append(n, k)
	}
	sort.Strings(n)
	return strings.Join(n, ", ")
}

// Result is a verification outcome. Every problem is reported, not the first.
type Result struct {
	OK       bool     `json:"ok"`
	Problems []string `json:"problems"`
	Checked  int      `json:"checked"`
}

func marshal(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(b.Bytes(), "\n")
}

// DecodeDoc reads an archive keeping numbers exact, so a verifier hashes the bytes
// that were hashed at export.
func DecodeDoc(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// ArchiveHash is a SHA-256 over the archive's content minus the hash field, as
// canonical JSON (sorted keys, no slack) so the same archive hashes the same anywhere.
func ArchiveHash(doc map[string]any) string {
	body := make(map[string]any, len(doc))
	for k, v := range doc {
		if k != "archive_hash" {
			body[k] = v
		}
	}
	sum := sha256.Sum256(marshal(body))
	return hex.EncodeToString(sum[:])
}

// archived is one row of an archive.
type archived struct {
	Seq      int64
	Kind     string
	Payload  map[string]any
	PrevHash string
	Hash     string
}

func rowsOf(doc map[string]any) ([]archived, error) {
	list, _ := doc["rows"].([]any)
	out := make([]archived, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("a row is not an object")
		}
		seq, err := asInt(m["seq"])
		if err != nil {
			return nil, err
		}
		payload, _ := m["payload"].(map[string]any)
		if payload == nil {
			payload = map[string]any{}
		}
		out = append(out, archived{seq, str(m["kind"]), payload, str(m["prev_hash"]), str(m["hash"])})
	}
	return out, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func asInt(v any) (int64, error) {
	switch n := v.(type) {
	case json.Number:
		return n.Int64()
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case float64:
		return int64(n), nil
	}
	return 0, fmt.Errorf("%v is not an integer", v)
}

// VerifySegment recomputes the links of a slice of a chain. It is not the whole
// chain verifier, which starts at seq 0 with an empty prev hash and is right for a
// whole chain and wrong for a slice: an archive of seqs 40 to 99 verifies against the
// hash of seq 39, which the archive records.
func VerifySegment(rows []archived, scope, startPrev string) bool {
	prev, want := startPrev, int64(-1)
	for _, r := range rows {
		if want >= 0 && r.Seq != want {
			return false
		}
		if r.PrevHash != prev || recorder.ComputeHash(prev, scope, r.Seq, r.Kind, r.Payload) != r.Hash {
			return false
		}
		prev, want = r.Hash, r.Seq+1
	}
	return true
}

// BuildExport reads one chain, or a prefix of it up to throughSeq, and returns a
// self verifying archive. throughSeq bounds it inclusively: the shape a truncation
// needs, since only rows up to a chosen point are ever removed. It is read only.
func BuildExport(ctx context.Context, db *store.DB, chain, scope, takenAt string, throughSeq *int64, note string) (map[string]any, error) {
	spec, ok := Chains[chain]
	if !ok {
		return nil, fmt.Errorf("unknown chain %q — known: %s", chain, chainNames())
	}
	q := `SELECT seq, kind, payload, prev_hash, hash FROM ` + spec.Table + ` WHERE ` + spec.Key + ` = ?`
	args := []any{scope}
	if throughSeq != nil {
		q += ` AND seq <= ?`
		args = append(args, *throughSeq)
	}
	rs, err := db.QueryContext(ctx, db.Q(q+` ORDER BY seq`), args...)
	if err != nil {
		return nil, err
	}
	var rows []archived
	for rs.Next() {
		var r archived
		var raw []byte
		if err := rs.Scan(&r.Seq, &r.Kind, &raw, &r.PrevHash, &r.Hash); err != nil {
			rs.Close()
			return nil, err
		}
		dec, err := DecodeDoc(raw)
		if err != nil {
			rs.Close()
			return nil, fmt.Errorf("payload of seq %d is not JSON: %w", r.Seq, err)
		}
		r.Payload = dec
		rows = append(rows, r)
	}
	rs.Close()

	var aseq sql.NullInt64
	var ahash sql.NullString
	err = db.QueryRowContext(ctx, db.Q(`SELECT seq, hash FROM `+spec.Anchor+` WHERE `+spec.Key+` = ?`), scope).Scan(&aseq, &ahash)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}

	items := make([]any, len(rows))
	for i, r := range rows {
		items[i] = map[string]any{"seq": r.Seq, "kind": r.Kind, "payload": r.Payload, "prev_hash": r.PrevHash, "hash": r.Hash}
	}
	startPrev, endHash := "", ""
	var from, through any
	if len(rows) > 0 {
		startPrev, endHash = rows[0].PrevHash, rows[len(rows)-1].Hash
		from, through = rows[0].Seq, rows[len(rows)-1].Seq
	}
	var anchor any
	if aseq.Valid {
		anchor = map[string]any{"seq": aseq.Int64, "hash": ahash.String}
	}
	var bounded any
	if throughSeq != nil {
		bounded = *throughSeq
	}
	doc := map[string]any{
		"archive_version": ArchiveVersion, "chain": chain, "scope_key": spec.Key, "scope_id": scope, "taken_at": takenAt, "note": note,
		"from_seq": from, "through_seq": through, "row_count": len(rows), "bounded_at": bounded,
		"start_prev_hash": startPrev, "end_hash": endHash, "anchor": anchor,
		"links_verified_at_export": VerifySegment(rows, scope, startPrev),
		"rows":                     items, "limit": ArchiveLimit,
	}
	// Round trip so the hash covers exactly the bytes a reader will decode.
	round, err := DecodeDoc(marshal(doc))
	if err != nil {
		return nil, err
	}
	round["archive_hash"] = ArchiveHash(round)
	return round, nil
}

// VerifyExport checks an archive with no database present. Three failures are three
// different sentences because they need three responses: an archive edited after it
// was written, an archive whose rows do not chain, and one this build is too old to read.
func VerifyExport(doc map[string]any) Result {
	var problems []string
	checked := 0
	if v, err := asInt(doc["archive_version"]); err == nil && v > ArchiveVersion {
		return Result{false, []string{fmt.Sprintf("archive is version %d but this build reads v%d — verify it with the build that wrote it", v, ArchiveVersion)}, 0}
	}

	checked++
	stored := str(doc["archive_hash"])
	switch {
	case stored == "":
		problems = append(problems, "archive carries no archive_hash — it cannot be checked at all")
	case stored != ArchiveHash(doc):
		problems = append(problems, "archive_hash does not match the archive's content — this file was edited after it was written. Nothing below can be trusted from this copy.")
	}

	rows, err := rowsOf(doc)
	if err != nil {
		return Result{false, append(problems, "the archived rows cannot be read: "+err.Error()), checked}
	}
	checked++
	linksOK := VerifySegment(rows, str(doc["scope_id"]), str(doc["start_prev_hash"]))
	if !linksOK {
		problems = append(problems, fmt.Sprintf("the %d archived row(s) do not chain — recomputing their hashes from start_prev_hash disagrees with what they carry", len(rows)))
	}

	checked++
	switch at, _ := doc["links_verified_at_export"].(bool); {
	case doc["links_verified_at_export"] == false && linksOK:
		problems = append(problems, "the archive records that its links did NOT verify when it was taken, yet they verify now — the rows were repaired after export, which is a rewrite of evidence")
	case at && !linksOK:
		problems = append(problems, "the archive claims its links verified at export and they do not now — either the rows were altered in place, or the archive was assembled from a different chain")
	}

	checked++
	anchor, _ := doc["anchor"].(map[string]any)
	switch {
	case anchor == nil:
		problems = append(problems, "no anchor was recorded at export — this archive proves its rows chain, but not that they are all of them (that is the truncation the anchor exists to catch)")
	case len(rows) > 0:
		aseq, _ := asInt(anchor["seq"])
		through, _ := asInt(doc["through_seq"])
		switch {
		case aseq < through:
			problems = append(problems, fmt.Sprintf("the recorded anchor stops at seq %d while the archive holds rows through %d — the source chain was already ahead of its own head "+
				"when this was taken (an append that crashed between the row and the anchor leaves exactly this, and so does a forged row)", aseq, through))
		case aseq > through && doc["bounded_at"] == nil:
			problems = append(problems, fmt.Sprintf("the source chain ended at seq %d but its anchor records seq %d — %d entries were missing from the chain when this archive was taken. "+
				"The archived rows chain perfectly; that is what a truncation looks like from the rows alone", through, aseq, aseq-through))
		}
	}
	return Result{len(problems) == 0, nonNil(problems), checked}
}

func nonNil(p []string) []string {
	if p == nil {
		return []string{}
	}
	return p
}

// TruncationPrerequisites is what a truncation must have before a row may be removed.
var TruncationPrerequisites = []string{
	"A verified archive covering exactly the rows to be removed: VerifyExport ok, and its through_seq equal to the highest seq being deleted.",
	"The archive stored somewhere the database's own operators cannot silently replace, per ArchiveLimit. An archive kept beside the rows it justifies deleting proves nothing.",
	"A durable truncation record in the database (chain, scope, through_seq, the archive's hash, and the surviving chain's new first prev_hash) so the gap is DECLARED rather than discovered. An undeclared gap is exactly what tampering looks like.",
	"A verifier that reads that record. A plain whole chain verifier starts at seq 0 with an empty prev hash, so a legitimately truncated chain fails it; without this change every pruned install's own housekeeping would be a permanent tamper alarm.",
}

// ErrRefused is a truncation that did not happen. Every refusal is a precondition:
// nothing has been written or deleted when it is returned.
type ErrRefused struct{ Reason string }

func (e *ErrRefused) Error() string { return e.Reason }

func refuse(format string, a ...any) error { return &ErrRefused{fmt.Sprintf(format, a...)} }

// Truncation is what a successful truncation did.
type Truncation struct {
	Chain          string `json:"chain"`
	ScopeID        string `json:"scope_id"`
	ThroughSeq     int64  `json:"through_seq"`
	ResumeSeq      int64  `json:"resume_seq"`
	ResumePrevHash string `json:"resume_prev_hash"`
	ArchiveHash    string `json:"archive_hash"`
	RowsRemoved    int    `json:"rows_removed"`
}

// TruncateChain deletes the rows an archive holds, after recording why the gap is
// legitimate. The order is the point: the record is written first and the DELETE last,
// in one transaction. A crash between them leaves a declared gap that does not exist
// yet, which verifies fine because the rows are still there; the opposite order would
// leave an undeclared gap, a permanent alarm on the operator's own housekeeping.
//
// It refuses unless all of these hold, each checked against the live database and not
// the archive's claims about it: the archive verifies and covers at least one row; the
// row at through_seq is still present and still carries the archive's end hash;
// rows survive past it (removing a chain entirely is deletion, and the anchor would
// say so forever); and the surviving chain links to the archive, which is what lets a
// verifier resume and why a forged record cannot launder an edit.
func TruncateChain(ctx context.Context, db *store.DB, doc map[string]any, note string) (*Truncation, error) {
	if res := VerifyExport(doc); !res.OK {
		return nil, refuse("the archive does not verify, so it cannot justify deleting anything: %s", strings.Join(res.Problems, "; "))
	}
	chain := str(doc["chain"])
	spec, ok := Chains[chain]
	if !ok {
		return nil, refuse("unknown chain %q — known: %s", chain, chainNames())
	}
	rows, err := rowsOf(doc)
	if err != nil || len(rows) == 0 {
		return nil, refuse("the archive holds no rows — there is nothing to remove")
	}
	through, _ := asInt(doc["through_seq"])
	scope, endHash, archive := str(doc["scope_id"]), str(doc["end_hash"]), str(doc["archive_hash"])

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var live string
	err = tx.QueryRowContext(ctx, db.Q(`SELECT hash FROM `+spec.Table+` WHERE `+spec.Key+` = ? AND seq = ?`), scope, through).Scan(&live)
	if err == sql.ErrNoRows {
		return nil, refuse("the chain has no row at seq=%d, which the archive says it ends at — this archive does not describe the chain as it stands now", through)
	}
	if err != nil {
		return nil, err
	}
	if live != endHash {
		return nil, refuse("the row at seq=%d carries a different hash than the archive recorded — the chain changed after the archive was taken, so the archive is not a copy of what would be deleted", through)
	}

	var resumeSeq int64
	var resumePrev string
	err = tx.QueryRowContext(ctx, db.Q(`SELECT seq, prev_hash FROM `+spec.Table+` WHERE `+spec.Key+` = ? AND seq > ? ORDER BY seq LIMIT 1`), scope, through).Scan(&resumeSeq, &resumePrev)
	if err == sql.ErrNoRows {
		return nil, refuse("nothing survives past seq=%d — removing every row is not truncation, and the head anchor would report the chain as entirely removed, correctly", through)
	}
	if err != nil {
		return nil, err
	}
	if resumePrev != endHash {
		return nil, refuse("the surviving chain does not link to this archive: the row at seq=%d chains from a different hash than the archive's last row. Either the archive is of a "+
			"different chain, or the chain is already broken at that seam — verify it before removing anything", resumeSeq)
	}

	if _, err = tx.ExecContext(ctx, db.Q(`INSERT INTO chain_truncation (chain, scope_id, through_seq, resume_seq, resume_prev_hash, archive_hash, note)
		VALUES (?, ?, ?, ?, ?, ?, ?)`), chain, scope, through, resumeSeq, resumePrev, archive, note); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, db.Q(`DELETE FROM `+spec.Table+` WHERE `+spec.Key+` = ? AND seq <= ?`), scope, through); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &Truncation{chain, scope, through, resumeSeq, resumePrev, archive, len(rows)}, nil
}

// ── backup manifests ─────────────────────────────────────────────────────────

const ManifestVersion = 1

// CountedTables are the tables whose loss is a data loss event.
var CountedTables = []string{
	"episodes", "decision_log", "memory_audit", "kg_entities", "kg_edges", "semantic_rules", "detectors",
	"failure_patterns", "rca_outcomes", "prospective_memory", "user_prefs",
}

func count(ctx context.Context, db *store.DB, q string) (int64, error) {
	var n sql.NullInt64
	err := db.QueryRowContext(ctx, q).Scan(&n)
	return n.Int64, err
}

func chainQueries(spec Chain) (anchors, unreached string) {
	return `SELECT COUNT(*) FROM ` + spec.Anchor,
		`SELECT COUNT(*) FROM ` + spec.Anchor + ` h LEFT JOIN (SELECT ` + spec.Key + ` AS k, MAX(seq) AS s FROM ` + spec.Table + ` GROUP BY ` + spec.Key +
			`) c ON c.k = h.` + spec.Key + ` WHERE c.s IS NULL OR c.s <> h.seq`
}

// BuildManifest measures a live database, to be taken beside the dump. It records
// what the database held: the schema version and fingerprint, exact row counts for the
// tables whose loss is a data loss event and, the part no row count can replace, how
// far each hash chain got. A restore that silently drops the newest rows of a chain
// breaks no link, so the surviving rows verify perfectly while the record is short;
// the anchors exist to make that visible and only work if something compares them.
func BuildManifest(ctx context.Context, db *store.DB, takenAt, note string) (map[string]any, error) {
	counts := map[string]any{}
	for _, t := range CountedTables {
		n, err := count(ctx, db, `SELECT COUNT(*) FROM `+t)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t, err)
		}
		counts[t] = n
	}
	chains := map[string]any{}
	for name, spec := range Chains {
		a, u := chainQueries(spec)
		na, err := count(ctx, db, a)
		if err != nil {
			return nil, err
		}
		nu, err := count(ctx, db, u)
		if err != nil {
			return nil, err
		}
		chains[name] = map[string]any{"anchors": na, "unreached_at_backup": nu}
	}
	return map[string]any{
		"manifest_version": ManifestVersion, "taken_at": takenAt, "note": note,
		"schema_version": schema.Latest(), "schema_fingerprint": schema.Fingerprint(),
		"row_counts": counts, "chains": chains,
	}, nil
}

// VerifyManifest re-measures a restored database against a manifest and reports every
// discrepancy. A missing table, a short table and a truncated chain tail are three
// different sentences, because they need three responses mid incident.
func VerifyManifest(ctx context.Context, db *store.DB, m map[string]any) Result {
	var problems []string
	if v, err := asInt(m["manifest_version"]); err == nil && v > ManifestVersion {
		return Result{false, []string{fmt.Sprintf("manifest is version %d but this build reads v%d — verify with the build that took it", v, ManifestVersion)}, 0}
	}
	checked := 1
	sv, _ := asInt(m["schema_version"])
	switch {
	case int(sv) != schema.Latest():
		problems = append(problems, fmt.Sprintf("schema version differs: backup v%d, this build v%d. Row counts below are still meaningful; a shape difference is not data loss, "+
			"but the restore target is not the shape the dump came from.", sv, schema.Latest()))
	case str(m["schema_fingerprint"]) != schema.Fingerprint():
		problems = append(problems, "schema fingerprint differs at the same version — the DDL changed without a version bump somewhere between taking this backup and restoring it")
	}

	counts, _ := m["row_counts"].(map[string]any)
	names := make([]string, 0, len(counts))
	for t := range counts {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		checked++
		want, _ := asInt(counts[t])
		got, err := count(ctx, db, `SELECT COUNT(*) FROM `+t)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: cannot be read (%v) — the restore did not create it", t, err))
		case got != want:
			dir, diff := "extra", got-want
			if got < want {
				dir, diff = "MISSING", want-got
			}
			problems = append(problems, fmt.Sprintf("%s: %d rows, manifest says %d (%d %s)", t, got, want, diff, dir))
		}
	}

	chains, _ := m["chains"].(map[string]any)
	for _, name := range []string{"decision_log", "memory_audit"} {
		rec, ok := chains[name].(map[string]any)
		if !ok {
			continue
		}
		checked += 2
		a, u := chainQueries(Chains[name])
		na, err1 := count(ctx, db, a)
		nu, err2 := count(ctx, db, u)
		if err1 != nil || err2 != nil {
			problems = append(problems, fmt.Sprintf("%s: chain check could not run (%v %v)", name, err1, err2))
			continue
		}
		if want, _ := asInt(rec["anchors"]); na != want {
			problems = append(problems, fmt.Sprintf("%s: %d anchor rows, manifest says %d", Chains[name].Anchor, na, want))
		}
		if was, _ := asInt(rec["unreached_at_backup"]); nu > was {
			problems = append(problems, fmt.Sprintf("%s: %d chain(s) do not reach their recorded head — a truncated tail. This is the failure a link check CANNOT see: "+
				"the surviving rows still hash correctly, so the record verifies while being short.", name, nu))
		}
	}
	return Result{len(problems) == 0, nonNil(problems), checked}
}
