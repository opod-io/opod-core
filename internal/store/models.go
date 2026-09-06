package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ---- models ----

type sqliteModels struct{ db *sql.DB }

func (s *sqliteModels) Upsert(ctx context.Context, m Model) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO models(id, catalog_id, source, status, size_bytes, installed_at)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   catalog_id=excluded.catalog_id,
		   source=excluded.source,
		   status=excluded.status,
		   size_bytes=excluded.size_bytes,
		   installed_at=excluded.installed_at`,
		m.ID, m.CatalogID, m.Source, m.Status, m.SizeBytes, m.InstalledAt.Unix())
	if err != nil {
		return fmt.Errorf("upsert model: %w", err)
	}
	return nil
}

func (s *sqliteModels) Get(ctx context.Context, id string) (*Model, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, catalog_id, source, status, size_bytes, installed_at FROM models WHERE id = ?`, id)
	var m Model
	var ts int64
	if err := row.Scan(&m.ID, &m.CatalogID, &m.Source, &m.Status, &m.SizeBytes, &ts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan model: %w", err)
	}
	m.InstalledAt = time.Unix(ts, 0)
	return &m, nil
}

func (s *sqliteModels) List(ctx context.Context) ([]Model, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, catalog_id, source, status, size_bytes, installed_at FROM models ORDER BY installed_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("query models: %w", err)
	}
	defer rows.Close()
	var out []Model
	for rows.Next() {
		var m Model
		var ts int64
		if err := rows.Scan(&m.ID, &m.CatalogID, &m.Source, &m.Status, &m.SizeBytes, &ts); err != nil {
			return nil, fmt.Errorf("scan model: %w", err)
		}
		m.InstalledAt = time.Unix(ts, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *sqliteModels) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM models WHERE id = ?`, id)
	return err
}
