package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ---- api_keys ----

type sqliteAPIKeys struct{ db *sql.DB }

func (s *sqliteAPIKeys) Create(ctx context.Context, k APIKey) error {
	// A key is an identity: the per-key policy columns left with the policy
	// (droppedColumns, sqlite.go).
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO api_keys(id, hash, name, scope, user_id, expires_at, created_at, revoked)
		 VALUES(?,?,?,?,?,?,?,?)`,
		k.ID, k.Hash, k.Name, k.Scope, k.UserID,
		unixOrZero(k.ExpiresAt), k.CreatedAt.Unix(), boolToInt(k.Revoked)); err != nil {
		return fmt.Errorf("insert api_key: %w", err)
	}
	return nil
}

func (s *sqliteAPIKeys) GetByHash(ctx context.Context, hash string) (*APIKey, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, hash, name, scope, user_id, expires_at, created_at, revoked
		 FROM api_keys WHERE hash = ?`, hash)
	return scanKey(row)
}

func (s *sqliteAPIKeys) GetByID(ctx context.Context, id string) (*APIKey, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, hash, name, scope, user_id, expires_at, created_at, revoked
		 FROM api_keys WHERE id = ?`, id)
	return scanKey(row)
}

func scanKey(row *sql.Row) (*APIKey, error) {
	var k APIKey
	var ts int64
	var expiresAt int64
	var rev int
	if err := row.Scan(&k.ID, &k.Hash, &k.Name, &k.Scope, &k.UserID, &expiresAt, &ts, &rev); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan api_key: %w", err)
	}
	k.CreatedAt = time.Unix(ts, 0)
	if expiresAt > 0 {
		k.ExpiresAt = time.Unix(expiresAt, 0)
	}
	k.Revoked = rev != 0
	return &k, nil
}

func (s *sqliteAPIKeys) List(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, hash, name, scope, user_id, expires_at, created_at, revoked
		 FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("query api_keys: %w", err)
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		var ts int64
		var expiresAt int64
		var rev int
		if err := rows.Scan(&k.ID, &k.Hash, &k.Name, &k.Scope, &k.UserID, &expiresAt, &ts, &rev); err != nil {
			return nil, fmt.Errorf("scan api_key: %w", err)
		}
		k.CreatedAt = time.Unix(ts, 0)
		if expiresAt > 0 {
			k.ExpiresAt = time.Unix(expiresAt, 0)
		}
		k.Revoked = rev != 0
		out = append(out, k)
	}
	return out, rows.Err()
}

// unixOrZero converts a time to its unix timestamp, returning 0 for the
// zero time. Used at write time so a "never expires" record stores 0
// in the column rather than a sentinel like INT_MAX.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func (s *sqliteAPIKeys) Revoke(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE api_keys SET revoked = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("revoke api_key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("api_key %s not found", id)
	}
	return nil
}

func (s *sqliteAPIKeys) UpdateExpiresAt(ctx context.Context, id string, expiresAt time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET expires_at = ? WHERE id = ?`,
		unixOrZero(expiresAt), id)
	if err != nil {
		return fmt.Errorf("update expires_at: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("api_key %s not found", id)
	}
	return nil
}
