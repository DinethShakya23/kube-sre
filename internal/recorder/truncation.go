package recorder

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	"github.com/DinethShakya23/kube-sre/internal/store"
)

// Declared gaps: the record that separates housekeeping from tampering.
//
// A chain says "rows are missing" the same way whether an attacker removed them
// or the operator did: it is short, and the anchor says so. A row in
// chain_truncation says entries up to through_seq were removed on purpose, the
// surviving chain resumes at resume_seq and must chain from resume_prev_hash,
// and the removed rows are kept in an archive with content hash archive_hash.
// Verifiers consult it before reporting a short chain, and only then: with no
// matching row a short chain is still tampered.
//
// It is not prevention (someone with full database write can add a row as easily
// as delete chain rows) and it cannot hide an edit: resume_prev_hash is checked
// against the first surviving row, so a forged record has to be consistent with
// the rows, which means it can only excuse a deletion the operator could have
// made anyway.

// DeclaredStartInfo says where a verifier should begin, given what was declared
// removed. Read=false means the lookup could not run at all, which must never be
// treated as "nothing declared": a legitimately pruned chain would read as
// tampered the moment its own database hiccuped.
type DeclaredStartInfo struct {
	Seq         int64
	PrevHash    string
	Found       bool
	Read        bool
	ArchiveHash string
}

// DeclaredStart never returns an error.
func DeclaredStart(ctx context.Context, db *store.DB, chain, scope string) DeclaredStartInfo {
	if db == nil {
		return DeclaredStartInfo{}
	}
	var seq int64
	var prev, archive string
	err := db.QueryRowContext(ctx, db.Q(`SELECT resume_seq, resume_prev_hash, archive_hash FROM chain_truncation
		WHERE chain = ? AND scope_id = ? ORDER BY through_seq DESC LIMIT 1`), chain, scope).Scan(&seq, &prev, &archive)
	if errors.Is(err, sql.ErrNoRows) {
		return DeclaredStartInfo{Read: true}
	}
	if err != nil {
		// A missing table is the normal state of an install that never truncated.
		slog.Debug("chain truncation lookup failed", "chain", chain, "scope", scope, "err", err)
		return DeclaredStartInfo{}
	}
	return DeclaredStartInfo{Seq: seq, PrevHash: prev, Found: true, Read: true, ArchiveHash: archive}
}
