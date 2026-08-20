package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/DinethShakya23/kube-sre/internal/store"
)

// DBCheckpoints keeps one state row per session in the database.
type DBCheckpoints struct{ DB *store.DB }

var CheckpointMigrations = []store.Migration{{
	Version: 30, Name: "agent checkpoints",
	SQL: `
CREATE TABLE IF NOT EXISTS agent_threads (
    thread_id  TEXT PRIMARY KEY,
    state      {{JSON}} NOT NULL,
    updated_at {{TS}} NOT NULL DEFAULT CURRENT_TIMESTAMP
);`,
}}

// Load returns the saved state, or nil for a session never seen.
func (c *DBCheckpoints) Load(ctx context.Context, session string) (*State, error) {
	var raw []byte
	err := c.DB.QueryRowContext(ctx, c.DB.Q(`SELECT state FROM agent_threads WHERE thread_id = ?`), session).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Save writes the state, replacing the previous one.
func (c *DBCheckpoints) Save(ctx context.Context, session string, st *State) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = c.DB.ExecContext(ctx, c.DB.Q(`INSERT INTO agent_threads (thread_id, state, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT (thread_id) DO UPDATE SET state = excluded.state, updated_at = CURRENT_TIMESTAMP`), session, string(raw))
	return err
}
