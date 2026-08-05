package recorder

import "github.com/DinethShakya23/kube-sre/internal/store"

var Migrations = []store.Migration{{
	Version: 10, Name: "flight recorder",
	SQL: `
CREATE TABLE IF NOT EXISTS decision_log (
    id             {{PK}},
    episode_id     TEXT NOT NULL,
    seq            INTEGER NOT NULL,
    kind           TEXT NOT NULL,
    payload        {{JSON}} NOT NULL,
    prev_hash      TEXT NOT NULL DEFAULT '',
    hash           TEXT NOT NULL,
    created_at     {{TS}} NOT NULL DEFAULT CURRENT_TIMESTAMP,
    trace_id       TEXT,
    span_id        TEXT,
    parent_span_id TEXT,
    UNIQUE (episode_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_decision_log_trace ON decision_log (trace_id) WHERE trace_id IS NOT NULL;
CREATE TABLE IF NOT EXISTS decision_log_head (
    episode_id TEXT PRIMARY KEY,
    seq        BIGINT NOT NULL,
    hash       TEXT NOT NULL,
    updated_at {{TS}} NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS chain_truncation (
    chain            TEXT NOT NULL,
    scope_id         TEXT NOT NULL,
    through_seq      BIGINT NOT NULL,
    resume_seq       BIGINT NOT NULL,
    resume_prev_hash TEXT NOT NULL,
    archive_hash     TEXT NOT NULL,
    note             TEXT NOT NULL DEFAULT '',
    truncated_at     {{TS}} NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (chain, scope_id, through_seq)
);`,
}}
