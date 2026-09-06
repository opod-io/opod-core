package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ---- shards ----

type sqliteShards struct{ db *sql.DB }

func (s *sqliteShards) Create(ctx context.Context, sh Shard) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO shards(id, model_id, role, node_id, address, process_id, status, config_json, created_at, last_seen)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		sh.ID, sh.ModelID, sh.Role, sh.NodeID, sh.Address, sh.ProcessID,
		sh.Status, sh.ConfigJSON, sh.CreatedAt.Unix(), sh.LastSeen.Unix())
	if err != nil {
		return fmt.Errorf("insert shard: %w", err)
	}
	return nil
}

func (s *sqliteShards) Get(ctx context.Context, id string) (*Shard, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, model_id, role, node_id, address, process_id, status, config_json, created_at, last_seen
		 FROM shards WHERE id = ?`, id)
	return scanShard(row)
}

func scanShard(row *sql.Row) (*Shard, error) {
	var sh Shard
	var created, lastSeen int64
	if err := row.Scan(&sh.ID, &sh.ModelID, &sh.Role, &sh.NodeID, &sh.Address,
		&sh.ProcessID, &sh.Status, &sh.ConfigJSON, &created, &lastSeen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan shard: %w", err)
	}
	sh.CreatedAt = time.Unix(created, 0)
	sh.LastSeen = time.Unix(lastSeen, 0)
	return &sh, nil
}

func (s *sqliteShards) GetByModel(ctx context.Context, modelID string) ([]Shard, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, model_id, role, node_id, address, process_id, status, config_json, created_at, last_seen
		 FROM shards WHERE model_id = ? ORDER BY role, created_at`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanShards(rows)
}

func (s *sqliteShards) List(ctx context.Context) ([]Shard, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, model_id, role, node_id, address, process_id, status, config_json, created_at, last_seen
		 FROM shards ORDER BY model_id, role, created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanShards(rows)
}

func scanShards(rows *sql.Rows) ([]Shard, error) {
	var out []Shard
	for rows.Next() {
		var sh Shard
		var created, lastSeen int64
		if err := rows.Scan(&sh.ID, &sh.ModelID, &sh.Role, &sh.NodeID, &sh.Address,
			&sh.ProcessID, &sh.Status, &sh.ConfigJSON, &created, &lastSeen); err != nil {
			return nil, err
		}
		sh.CreatedAt = time.Unix(created, 0)
		sh.LastSeen = time.Unix(lastSeen, 0)
		out = append(out, sh)
	}
	return out, rows.Err()
}

func (s *sqliteShards) UpdateStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE shards SET status = ?, last_seen = ? WHERE id = ?`,
		status, time.Now().Unix(), id)
	return err
}

func (s *sqliteShards) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM shards WHERE id = ?`, id)
	return err
}

func (s *sqliteShards) DeleteByModel(ctx context.Context, modelID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM shards WHERE model_id = ?`, modelID)
	return err
}
