package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ---- shards ----

// shardCols is the column list every shard query selects, in the order
// scanShard/scanShards read them. One constant so a new column cannot be added
// to one query and forgotten in another.
const shardCols = `id, model_id, gang_id, role, node_id, address, process_id, status, config_json, created_at, last_seen`

// gangOr defaults an unnamed gang. A stored row always carries its gang; this
// is only for a caller that passed "".
func gangOr(gangID string) string {
	if gangID == "" {
		return DefaultGangID
	}
	return gangID
}

type sqliteShards struct{ db *sql.DB }

func (s *sqliteShards) Create(ctx context.Context, sh Shard) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO shards(`+shardCols+`)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		sh.ID, sh.ModelID, sh.Gang(), sh.Role, sh.NodeID, sh.Address, sh.ProcessID,
		sh.Status, sh.ConfigJSON, sh.CreatedAt.Unix(), sh.LastSeen.Unix())
	if err != nil {
		return fmt.Errorf("insert shard: %w", err)
	}
	return nil
}

func (s *sqliteShards) Get(ctx context.Context, id string) (*Shard, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+shardCols+` FROM shards WHERE id = ?`, id)
	return scanShard(row)
}

func scanShard(row *sql.Row) (*Shard, error) {
	var sh Shard
	var created, lastSeen int64
	if err := row.Scan(&sh.ID, &sh.ModelID, &sh.GangID, &sh.Role, &sh.NodeID, &sh.Address,
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
		`SELECT `+shardCols+`
		 FROM shards WHERE model_id = ? ORDER BY gang_id, role, created_at`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanShards(rows)
}

func (s *sqliteShards) GetByGang(ctx context.Context, modelID, gangID string) ([]Shard, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+shardCols+`
		 FROM shards WHERE model_id = ? AND gang_id = ? ORDER BY role, created_at`,
		modelID, gangOr(gangID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanShards(rows)
}

// GangsOf lists the model's gangs, oldest first — the order a caller that adds
// or removes "a gang" should see them in.
func (s *sqliteShards) GangsOf(ctx context.Context, modelID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT gang_id, MIN(created_at) AS born FROM shards
		 WHERE model_id = ? GROUP BY gang_id ORDER BY born, gang_id`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var gang string
		var born int64
		if err := rows.Scan(&gang, &born); err != nil {
			return nil, err
		}
		out = append(out, gang)
	}
	return out, rows.Err()
}

func (s *sqliteShards) List(ctx context.Context) ([]Shard, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+shardCols+`
		 FROM shards ORDER BY model_id, gang_id, role, created_at`)
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
		if err := rows.Scan(&sh.ID, &sh.ModelID, &sh.GangID, &sh.Role, &sh.NodeID, &sh.Address,
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

func (s *sqliteShards) DeleteByGang(ctx context.Context, modelID, gangID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM shards WHERE model_id = ? AND gang_id = ?`, modelID, gangOr(gangID))
	return err
}

// GangKey identifies one gang globally. The MODEL is part of it on purpose:
// gang ids are per model, so "g0" alone names a different gang for every
// sharded model, and a map keyed on it would silently merge them.
type GangKey struct {
	Model string
	Gang  string
}

// GroupGangs groups parts by the gang they belong to. Accepts parts of any
// number of models.
//
// Every question about whether something can SERVE is asked of one gang: a
// gang is a complete copy of the model, and its parts stand or fall together.
// Asking it of a model's parts as one set was the old shape and it answered
// wrongly in both directions — one gang losing a part looked like the model
// losing a part, and one gang's ready coordinator made a broken sibling look
// routable.
func GroupGangs(shards []Shard) map[GangKey][]Shard {
	out := map[GangKey][]Shard{}
	for _, s := range shards {
		k := GangKey{Model: s.ModelID, Gang: s.Gang()}
		out[k] = append(out[k], s)
	}
	return out
}
