package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ---- placements ----

type sqlitePlacements struct{ db *sql.DB }

func (s *sqlitePlacements) Upsert(ctx context.Context, p Placement) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO model_placements(node_id, model_id, status, last_seen, cold)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(node_id, model_id) DO UPDATE SET
		   status=excluded.status,
		   last_seen=excluded.last_seen,
		   cold=excluded.cold`,
		p.NodeID, p.ModelID, p.Status, p.LastSeen.Unix(), boolToInt(p.Cold))
	return err
}

func (s *sqlitePlacements) GetByModel(ctx context.Context, modelID string) ([]Placement, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT node_id, model_id, status, last_seen, cold FROM model_placements
		 WHERE model_id = ? AND status = 'ready'`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPlacements(rows)
}

func (s *sqlitePlacements) GetByNode(ctx context.Context, nodeID string) ([]Placement, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT node_id, model_id, status, last_seen, cold FROM model_placements
		 WHERE node_id = ?`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPlacements(rows)
}

// PlacementDraining marks a placement the leader is taking out of rotation:
// GetByModel does not return it, so the router sends it nothing new, while
// requests already on it finish.
const PlacementDraining = "draining"

// ReplaceForNode atomically replaces the placement set for a node — what a
// worker reports as loaded on every heartbeat — with one thing carried over:
// a row marked PlacementDraining keeps that mark while its model stays in the
// set. The mark is the leader's word (SetStatus), not something a worker
// reports, so the report must not erase it; it ends when SetStatus sets the
// row back, or when the model leaves the worker's report and the row goes
// with it. Read and write share one transaction, so a mark set a moment
// before a heartbeat is never lost to it.
func (s *sqlitePlacements) ReplaceForNode(ctx context.Context, nodeID string, ps []Placement) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	draining := map[string]bool{}
	if len(ps) > 0 {
		rows, err := tx.QueryContext(ctx,
			`SELECT model_id FROM model_placements WHERE node_id = ? AND status = ?`, nodeID, PlacementDraining)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			draining[id] = true
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM model_placements WHERE node_id = ?`, nodeID); err != nil {
		return err
	}
	for _, p := range ps {
		status := p.Status
		if draining[p.ModelID] {
			status = PlacementDraining
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO model_placements(node_id, model_id, status, last_seen, cold) VALUES(?,?,?,?,?)
			 ON CONFLICT(node_id, model_id) DO UPDATE SET
			   status=excluded.status, last_seen=excluded.last_seen, cold=excluded.cold`,
			p.NodeID, p.ModelID, status, p.LastSeen.Unix(), boolToInt(p.Cold)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *sqlitePlacements) Delete(ctx context.Context, nodeID, modelID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM model_placements WHERE node_id = ? AND model_id = ?`, nodeID, modelID)
	return err
}

func (s *sqlitePlacements) SetStatus(ctx context.Context, nodeID, modelID, status string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE model_placements SET status = ?, last_seen = ? WHERE node_id = ? AND model_id = ?`,
		status, time.Now().Unix(), nodeID, modelID)
	if err != nil {
		return fmt.Errorf("set placement status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no placement for %s on %s", modelID, nodeID)
	}
	return nil
}

func (s *sqlitePlacements) ResetStatus(ctx context.Context, from, to string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE model_placements SET status = ? WHERE status = ?`, to, from)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanPlacements(rows *sql.Rows) ([]Placement, error) {
	var out []Placement
	for rows.Next() {
		var p Placement
		var ts, cold int64
		if err := rows.Scan(&p.NodeID, &p.ModelID, &p.Status, &ts, &cold); err != nil {
			return nil, err
		}
		p.LastSeen = time.Unix(ts, 0)
		p.Cold = cold != 0
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- desired placements ----

type sqliteDesiredPlacements struct{ db *sql.DB }

func (s *sqliteDesiredPlacements) Upsert(ctx context.Context, d DesiredPlacement) error {
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO desired_placements(node_id, model_id, priority, pinned, created_at)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(node_id, model_id) DO UPDATE SET
		   priority=excluded.priority,
		   pinned=excluded.pinned`,
		d.NodeID, d.ModelID, d.Priority, boolToInt(d.Pinned), d.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("upsert desired placement: %w", err)
	}
	return nil
}

func (s *sqliteDesiredPlacements) Get(ctx context.Context, nodeID, modelID string) (*DesiredPlacement, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT node_id, model_id, priority, pinned, created_at
		 FROM desired_placements WHERE node_id = ? AND model_id = ?`, nodeID, modelID)
	var d DesiredPlacement
	var pinned int
	var created int64
	if err := row.Scan(&d.NodeID, &d.ModelID, &d.Priority, &pinned, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan desired placement: %w", err)
	}
	d.Pinned = pinned != 0
	d.CreatedAt = time.Unix(created, 0)
	return &d, nil
}

func (s *sqliteDesiredPlacements) ListByNode(ctx context.Context, nodeID string) ([]DesiredPlacement, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT node_id, model_id, priority, pinned, created_at
		 FROM desired_placements WHERE node_id = ?
		 ORDER BY priority DESC, created_at ASC`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("query desired placements: %w", err)
	}
	defer rows.Close()
	var out []DesiredPlacement
	for rows.Next() {
		var d DesiredPlacement
		var pinned int
		var created int64
		if err := rows.Scan(&d.NodeID, &d.ModelID, &d.Priority, &pinned, &created); err != nil {
			return nil, fmt.Errorf("scan desired placement: %w", err)
		}
		d.Pinned = pinned != 0
		d.CreatedAt = time.Unix(created, 0)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *sqliteDesiredPlacements) Delete(ctx context.Context, nodeID, modelID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM desired_placements WHERE node_id = ? AND model_id = ?`, nodeID, modelID)
	return err
}
