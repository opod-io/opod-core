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
		`INSERT INTO nodes(id, hostname, os, arch, ram_gb, address, worker_token, bound_key_id, hardware_json, last_heartbeat, state, boot_id, engine_silent_since)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
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
		   state=excluded.state,
		   boot_id=CASE WHEN excluded.boot_id != '' THEN excluded.boot_id ELSE nodes.boot_id END,
		   engine_silent_since=excluded.engine_silent_since`,
		n.ID, n.Hostname, n.OS, n.Arch, n.RAMGB, n.Address, n.WorkerToken, n.BoundKeyID, n.HardwareJSON, n.LastHeartbeat.Unix(), n.State, n.BootID, unixOrZero(n.EngineSilentSince))
	return err
}

func (s *sqliteNodes) Get(ctx context.Context, id string) (*Node, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, hostname, os, arch, ram_gb, address, worker_token, bound_key_id, hardware_json, last_heartbeat, state, boot_id, engine_silent_since FROM nodes WHERE id = ?`, id)
	var n Node
	var ts, silent int64
	if err := row.Scan(&n.ID, &n.Hostname, &n.OS, &n.Arch, &n.RAMGB, &n.Address, &n.WorkerToken, &n.BoundKeyID, &n.HardwareJSON, &ts, &n.State, &n.BootID, &silent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	n.LastHeartbeat = time.Unix(ts, 0)
	n.EngineSilentSince = timeOrZero(silent)
	return &n, nil
}

func (s *sqliteNodes) List(ctx context.Context) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, hostname, os, arch, ram_gb, address, worker_token, bound_key_id, hardware_json, last_heartbeat, state, boot_id, engine_silent_since FROM nodes ORDER BY hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		var ts, silent int64
		if err := rows.Scan(&n.ID, &n.Hostname, &n.OS, &n.Arch, &n.RAMGB, &n.Address, &n.WorkerToken, &n.BoundKeyID, &n.HardwareJSON, &ts, &n.State, &n.BootID, &silent); err != nil {
			return nil, err
		}
		n.LastHeartbeat = time.Unix(ts, 0)
		n.EngineSilentSince = timeOrZero(silent)
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *sqliteNodes) SetState(ctx context.Context, id, state string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE nodes SET state = ? WHERE id = ?`, state, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *sqliteNodes) Heartbeat(ctx context.Context, id string, at time.Time, bootID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE nodes SET
		   last_heartbeat = ?,
		   boot_id = CASE WHEN ? != '' THEN ? ELSE boot_id END,
		   state = CASE WHEN state = ? THEN ? ELSE state END
		 WHERE id = ?`,
		at.Unix(), bootID, bootID, NodeStateJoining, NodeStateReady, id)
	return err
}

// NoteEngineReport records whether a heartbeat carried the engine's report.
// reported clears the mark; the FIRST heartbeat without one stamps it, and the
// ones after it leave that stamp alone — it says since when the engine has
// been silent. A targeted write, like Heartbeat: no other column is touched.
func (s *sqliteNodes) NoteEngineReport(ctx context.Context, id string, reported bool, at time.Time) error {
	if reported {
		_, err := s.db.ExecContext(ctx, `UPDATE nodes SET engine_silent_since = 0 WHERE id = ?`, id)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE nodes SET engine_silent_since = ? WHERE id = ? AND engine_silent_since = 0`, at.Unix(), id)
	return err
}

// timeOrZero is unixOrZero's inverse (keys.go).
func timeOrZero(unix int64) time.Time {
	if unix == 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0)
}

func (s *sqliteNodes) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, id)
	return err
}
