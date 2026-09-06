package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ---- nodes ----

type sqliteNodes struct{ db *sql.DB }

func (s *sqliteNodes) Upsert(ctx context.Context, n Node) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO nodes(id, hostname, os, arch, ram_gb, address, worker_token, bound_key_id, hardware_json, last_heartbeat, state)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   hostname=excluded.hostname,
		   os=excluded.os,
		   arch=excluded.arch,
		   ram_gb=excluded.ram_gb,
		   address=excluded.address,
		   worker_token=CASE WHEN excluded.worker_token != '' THEN excluded.worker_token ELSE nodes.worker_token END,
		   bound_key_id=CASE WHEN excluded.bound_key_id != '' THEN excluded.bound_key_id ELSE nodes.bound_key_id END,
		   hardware_json=excluded.hardware_json,
		   last_heartbeat=excluded.last_heartbeat,
		   state=excluded.state`,
		n.ID, n.Hostname, n.OS, n.Arch, n.RAMGB, n.Address, n.WorkerToken, n.BoundKeyID, n.HardwareJSON, n.LastHeartbeat.Unix(), n.State)
	return err
}

func (s *sqliteNodes) Get(ctx context.Context, id string) (*Node, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, hostname, os, arch, ram_gb, address, worker_token, bound_key_id, hardware_json, last_heartbeat, state FROM nodes WHERE id = ?`, id)
	var n Node
	var ts int64
	if err := row.Scan(&n.ID, &n.Hostname, &n.OS, &n.Arch, &n.RAMGB, &n.Address, &n.WorkerToken, &n.BoundKeyID, &n.HardwareJSON, &ts, &n.State); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	n.LastHeartbeat = time.Unix(ts, 0)
	return &n, nil
}

func (s *sqliteNodes) List(ctx context.Context) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, hostname, os, arch, ram_gb, address, worker_token, bound_key_id, hardware_json, last_heartbeat, state FROM nodes ORDER BY hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		var ts int64
		if err := rows.Scan(&n.ID, &n.Hostname, &n.OS, &n.Arch, &n.RAMGB, &n.Address, &n.WorkerToken, &n.BoundKeyID, &n.HardwareJSON, &ts, &n.State); err != nil {
			return nil, err
		}
		n.LastHeartbeat = time.Unix(ts, 0)
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *sqliteNodes) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, id)
	return err
}
