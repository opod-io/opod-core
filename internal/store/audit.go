package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ---- audit ----

type sqliteAudit struct{ db *sql.DB }

func (s *sqliteAudit) Record(ctx context.Context, e AuditEntry) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log(ts, actor, action, target, metadata_json)
		 VALUES(?,?,?,?,?)`,
		e.TS.Unix(), e.Actor, e.Action, e.Target, e.Metadata)
	return err
}

func (s *sqliteAudit) Recent(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts, actor, action, target, metadata_json
		 FROM audit_log ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts int64
		if err := rows.Scan(&e.ID, &ts, &e.Actor, &e.Action, &e.Target, &e.Metadata); err != nil {
			return nil, err
		}
		e.TS = time.Unix(ts, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- budgets ----

type sqliteBudgets struct{ db *sql.DB }

func (s *sqliteBudgets) Create(ctx context.Context, b Budget) (int64, error) {
	if b.ResetAt.IsZero() {
		b.ResetAt = NextBudgetReset(b.Window, time.Now())
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO budgets(api_key_id, window, limit_unit, limit_value, current_value, reset_at, created_at)
		 VALUES(?,?,?,?,?,?,?)`,
		b.APIKeyID, b.Window, b.LimitUnit, b.LimitValue, b.CurrentValue,
		b.ResetAt.Unix(), b.CreatedAt.Unix())
	if err != nil {
		return 0, fmt.Errorf("insert budget: %w", err)
	}
	return res.LastInsertId()
}

func (s *sqliteBudgets) ListByKey(ctx context.Context, apiKeyID string) ([]Budget, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, api_key_id, window, limit_unit, limit_value, current_value, reset_at, created_at
		 FROM budgets WHERE api_key_id = ? ORDER BY id`, apiKeyID)
	if err != nil {
		return nil, fmt.Errorf("query budgets: %w", err)
	}
	defer rows.Close()
	var out []Budget
	for rows.Next() {
		var b Budget
		var resetAt, createdAt int64
		if err := rows.Scan(&b.ID, &b.APIKeyID, &b.Window, &b.LimitUnit, &b.LimitValue,
			&b.CurrentValue, &resetAt, &createdAt); err != nil {
			return nil, fmt.Errorf("scan budget: %w", err)
		}
		b.ResetAt = time.Unix(resetAt, 0)
		b.CreatedAt = time.Unix(createdAt, 0)
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *sqliteBudgets) Delete(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM budgets WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete budget: %w", err)
	}
	return nil
}

func (s *sqliteBudgets) Increment(ctx context.Context, id int64, delta float64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE budgets SET current_value = current_value + ? WHERE id = ?`,
		delta, id)
	if err != nil {
		return fmt.Errorf("increment budget: %w", err)
	}
	return nil
}

// ResetExpired walks every budget for this key and, if its reset_at is
// in the past, zeroes current_value and advances reset_at to the next
// window boundary. The lazy-on-read pattern keeps us out of cron
// territory — fresh state is observed on the very next admission
// check, however long the leader was offline.
func (s *sqliteBudgets) ResetExpired(ctx context.Context, apiKeyID string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx,
		`SELECT id, window, reset_at FROM budgets WHERE api_key_id = ? AND reset_at <= ?`,
		apiKeyID, now.Unix())
	if err != nil {
		return fmt.Errorf("scan expired budgets: %w", err)
	}
	type expiry struct {
		ID     int64
		Window string
	}
	var toReset []expiry
	for rows.Next() {
		var e expiry
		var ts int64
		if err := rows.Scan(&e.ID, &e.Window, &ts); err != nil {
			rows.Close()
			return err
		}
		toReset = append(toReset, e)
	}
	rows.Close()
	for _, e := range toReset {
		next := NextBudgetReset(e.Window, now)
		if _, err := tx.ExecContext(ctx,
			`UPDATE budgets SET current_value = 0, reset_at = ? WHERE id = ?`,
			next.Unix(), e.ID); err != nil {
			return fmt.Errorf("reset budget %d: %w", e.ID, err)
		}
	}
	return tx.Commit()
}

// NextBudgetReset returns the unix time at which a budget with the
// given window should next roll. UTC is used so resets land at the
// same wall-clock moment for all admins; future work can make the
// timezone configurable per-budget.
//
//	day   → next 00:00 UTC
//	week  → next Monday 00:00 UTC
//	month → next 1st of month 00:00 UTC
//	(default) treated as "day" — same logic as misconfigured rows
func NextBudgetReset(window string, now time.Time) time.Time {
	now = now.UTC()
	switch window {
	case "month":
		return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	case "week":
		// Days until next Monday (1..7).
		offset := int(time.Monday-now.Weekday()+7) % 7
		if offset == 0 {
			offset = 7
		}
		next := time.Date(now.Year(), now.Month(), now.Day()+offset, 0, 0, 0, 0, time.UTC)
		return next
	default: // "day"
		return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	}
}
