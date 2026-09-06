package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ---- api_keys ----

type sqliteAPIKeys struct{ db *sql.DB }

func (s *sqliteAPIKeys) Create(ctx context.Context, k APIKey) error {
	allowed, err := marshalAllowed(k.AllowedModels)
	if err != nil {
		return fmt.Errorf("encode allowed_models: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO api_keys(id, hash, name, scope, user_id, quota_daily_tokens, rpm_limit, tpm_limit, allowed_models, expires_at, created_at, revoked)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		k.ID, k.Hash, k.Name, k.Scope, k.UserID, k.QuotaDailyTokens, k.RPMLimit, k.TPMLimit, allowed,
		unixOrZero(k.ExpiresAt), k.CreatedAt.Unix(), boolToInt(k.Revoked)); err != nil {
		return fmt.Errorf("insert api_key: %w", err)
	}
	return nil
}

func (s *sqliteAPIKeys) GetByHash(ctx context.Context, hash string) (*APIKey, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, hash, name, scope, user_id, quota_daily_tokens, rpm_limit, tpm_limit, allowed_models, expires_at, created_at, revoked
		 FROM api_keys WHERE hash = ?`, hash)
	return scanKey(row)
}

func (s *sqliteAPIKeys) GetByID(ctx context.Context, id string) (*APIKey, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, hash, name, scope, user_id, quota_daily_tokens, rpm_limit, tpm_limit, allowed_models, expires_at, created_at, revoked
		 FROM api_keys WHERE id = ?`, id)
	return scanKey(row)
}

func scanKey(row *sql.Row) (*APIKey, error) {
	var k APIKey
	var ts int64
	var expiresAt int64
	var rev int
	var allowed sql.NullString
	if err := row.Scan(&k.ID, &k.Hash, &k.Name, &k.Scope, &k.UserID, &k.QuotaDailyTokens, &k.RPMLimit, &k.TPMLimit, &allowed, &expiresAt, &ts, &rev); err != nil {
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
	list, err := unmarshalAllowed(allowed)
	if err != nil {
		return nil, fmt.Errorf("decode allowed_models: %w", err)
	}
	k.AllowedModels = list
	return &k, nil
}

func (s *sqliteAPIKeys) List(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, hash, name, scope, user_id, quota_daily_tokens, rpm_limit, tpm_limit, allowed_models, expires_at, created_at, revoked
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
		var allowed sql.NullString
		if err := rows.Scan(&k.ID, &k.Hash, &k.Name, &k.Scope, &k.UserID, &k.QuotaDailyTokens, &k.RPMLimit, &k.TPMLimit, &allowed, &expiresAt, &ts, &rev); err != nil {
			return nil, fmt.Errorf("scan api_key: %w", err)
		}
		k.CreatedAt = time.Unix(ts, 0)
		if expiresAt > 0 {
			k.ExpiresAt = time.Unix(expiresAt, 0)
		}
		k.Revoked = rev != 0
		list, err := unmarshalAllowed(allowed)
		if err != nil {
			return nil, fmt.Errorf("decode allowed_models: %w", err)
		}
		k.AllowedModels = list
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

func (s *sqliteAPIKeys) UpdateRateLimits(ctx context.Context, id string, rpm, tpm int) error {
	if rpm < 0 || tpm < 0 {
		return fmt.Errorf("rpm/tpm must be >= 0 (0 = unlimited)")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET rpm_limit = ?, tpm_limit = ? WHERE id = ?`,
		rpm, tpm, id)
	if err != nil {
		return fmt.Errorf("update rate limits: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("api_key %s not found", id)
	}
	return nil
}

func (s *sqliteAPIKeys) UpdateAllowedModels(ctx context.Context, id string, allowed []string) error {
	encoded, err := marshalAllowed(allowed)
	if err != nil {
		return fmt.Errorf("encode allowed_models: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE api_keys SET allowed_models = ? WHERE id = ?`, encoded, id)
	if err != nil {
		return fmt.Errorf("update allowed_models: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("api_key %s not found", id)
	}
	return nil
}

// marshalAllowed encodes the allowlist for storage.
//
//   - nil  → SQL NULL ("no restriction") — preserves the pre-allowlist
//     default for keys created before the column existed.
//   - []   → "[]" (JSON empty array) → "deny every model". A real string
//     in the column distinguishes this from the nil case.
//   - list → "[\"id1\",\"id2\"]" — explicit allowlist.
func marshalAllowed(list []string) (sql.NullString, error) {
	if list == nil {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(list)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

func unmarshalAllowed(v sql.NullString) ([]string, error) {
	if !v.Valid {
		return nil, nil
	}
	if v.String == "" {
		return []string{}, nil
	}
	var list []string
	if err := json.Unmarshal([]byte(v.String), &list); err != nil {
		return nil, err
	}
	if list == nil {
		// JSON `null` round-trips to a nil slice, but a stored Valid row
		// represents "explicit empty" — normalize to []string{} so the
		// caller sees deny-all rather than unrestricted.
		return []string{}, nil
	}
	return list, nil
}
