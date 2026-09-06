package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ---- cache ----

type sqliteCache struct{ db *sql.DB }

func (s *sqliteCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	var value []byte
	var expiresAt int64
	err := s.db.QueryRowContext(ctx,
		`SELECT value, expires_at FROM cache WHERE key = ?`, key).Scan(&value, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("cache get: %w", err)
	}
	if expiresAt > 0 && time.Now().Unix() > expiresAt {
		return nil, false, nil
	}
	return value, true, nil
}

func (s *sqliteCache) Set(ctx context.Context, key, namespace string, value []byte, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO cache(key, namespace, value, expires_at)
		 VALUES(?,?,?,?)
		 ON CONFLICT(key) DO UPDATE SET
		   namespace=excluded.namespace,
		   value=excluded.value,
		   expires_at=excluded.expires_at`,
		key, namespace, value, expiresAt.Unix())
	if err != nil {
		return fmt.Errorf("cache set: %w", err)
	}
	return nil
}

func (s *sqliteCache) Delete(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cache WHERE key = ?`, key)
	return err
}

func (s *sqliteCache) DeleteNamespace(ctx context.Context, namespace string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cache WHERE namespace = ?`, namespace)
	return err
}

func (s *sqliteCache) DeleteAll(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cache`)
	return err
}

func (s *sqliteCache) SweepExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM cache WHERE expires_at > 0 AND expires_at < ?`, now.Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *sqliteCache) Count(ctx context.Context) (int, int64, error) {
	var n int
	var bytes int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(LENGTH(value)), 0) FROM cache`).Scan(&n, &bytes)
	return n, bytes, err
}
