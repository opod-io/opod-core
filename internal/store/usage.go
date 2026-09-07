package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ---- usage ----

type sqliteUsage struct{ db *sql.DB }

func (s *sqliteUsage) Record(ctx context.Context, u Usage) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO usage(ts, api_key_id, user_id, model, protocol,
		    prompt_tokens, completion_tokens, latency_ms, outcome, cost_usd, node_id)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		u.TS.Unix(), u.APIKeyID, u.UserID, u.Model, u.Protocol,
		u.PromptTokens, u.CompletionTokens, u.LatencyMS, u.Outcome, u.CostUSD, u.NodeID)
	return err
}

func (s *sqliteUsage) SumTokensSince(ctx context.Context, apiKeyID string, since time.Time) (int64, error) {
	var sum sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(prompt_tokens + completion_tokens), 0)
		 FROM usage WHERE api_key_id = ? AND ts >= ?`,
		apiKeyID, since.Unix()).Scan(&sum)
	if err != nil {
		return 0, err
	}
	return sum.Int64, nil
}

func (s *sqliteUsage) LastUsedByModel(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT model, MAX(ts) FROM usage GROUP BY model`)
	if err != nil {
		return nil, fmt.Errorf("last used by model: %w", err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var model string
		var ts int64
		if err := rows.Scan(&model, &ts); err != nil {
			return nil, fmt.Errorf("scan last used: %w", err)
		}
		out[model] = time.Unix(ts, 0)
	}
	return out, rows.Err()
}

func (s *sqliteUsage) Recent(ctx context.Context, limit int) ([]Usage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts, api_key_id, user_id, model, protocol,
		        prompt_tokens, completion_tokens, latency_ms, outcome, cost_usd, node_id
		 FROM usage ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUsage(rows)
}

func (s *sqliteUsage) After(ctx context.Context, afterID int64, limit int) ([]Usage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts, api_key_id, user_id, model, protocol,
		        prompt_tokens, completion_tokens, latency_ms, outcome, cost_usd, node_id
		 FROM usage WHERE id > ? ORDER BY id ASC LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUsage(rows)
}

func (s *sqliteUsage) RecentByUser(ctx context.Context, userID string, limit int) ([]Usage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts, api_key_id, user_id, model, protocol,
		        prompt_tokens, completion_tokens, latency_ms, outcome, cost_usd, node_id
		 FROM usage WHERE user_id = ? ORDER BY ts DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUsage(rows)
}

// bucketExpr returns the SQL expression that maps the unix ts column to
// a bucket label. "total" collapses all rows into a single bucket so
// the grouping helpers below stay uniform.
func bucketExpr(bucket string) string {
	switch bucket {
	case "hour":
		return `strftime('%Y-%m-%d %H:00', ts, 'unixepoch')`
	case "month":
		return `strftime('%Y-%m', ts, 'unixepoch')`
	case "total":
		return `'all'`
	default: // "day" + unspecified
		return `strftime('%Y-%m-%d', ts, 'unixepoch')`
	}
}

// groupColumn maps the user-facing group_by token to the actual SQL
// column. Returning "", false signals an unknown token and the caller
// rejects the request with a 400.
func groupColumn(token string) (sqlCol string, ok bool) {
	switch token {
	case "user":
		return "user_id", true
	case "model":
		return "model", true
	case "protocol":
		return "protocol", true
	case "outcome":
		return "outcome", true
	}
	return "", false
}

func (s *sqliteUsage) Breakdown(ctx context.Context, opts BreakdownOpts) ([]BreakdownRow, BreakdownTotals, error) {
	if opts.Bucket == "" {
		opts.Bucket = "day"
	}
	// Normalize since/until with sensible defaults.
	now := time.Now()
	if opts.Since.IsZero() {
		opts.Since = now.AddDate(0, 0, -30)
	}
	if opts.Until.IsZero() {
		opts.Until = now
	}

	groupCols := make([]string, 0, len(opts.GroupBy))
	for _, g := range opts.GroupBy {
		col, ok := groupColumn(g)
		if !ok {
			return nil, BreakdownTotals{}, fmt.Errorf("unsupported group_by token %q (try user|model|protocol|outcome)", g)
		}
		groupCols = append(groupCols, col)
	}
	bucket := bucketExpr(opts.Bucket)

	// SELECT list: bucket label, each group col, then the aggregates.
	selectList := []string{bucket + " AS bucket"}
	selectList = append(selectList, groupCols...)
	selectList = append(selectList,
		"SUM(prompt_tokens) AS pt",
		"SUM(completion_tokens) AS ct",
		"COUNT(*) AS reqs",
		"COALESCE(SUM(cost_usd), 0) AS cost",
	)
	groupList := append([]string{"bucket"}, groupCols...)

	query := "SELECT " + strings.Join(selectList, ", ") +
		" FROM usage WHERE ts >= ? AND ts < ?" +
		" GROUP BY " + strings.Join(groupList, ", ") +
		" ORDER BY bucket DESC, reqs DESC"
	if opts.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", opts.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, opts.Since.Unix(), opts.Until.Unix())
	if err != nil {
		return nil, BreakdownTotals{}, fmt.Errorf("breakdown query: %w", err)
	}
	defer rows.Close()

	out := []BreakdownRow{}
	var totals BreakdownTotals
	for rows.Next() {
		var r BreakdownRow
		dest := []any{&r.Bucket}
		// One scan slot per group column; we route the value into the
		// matching BreakdownRow field in the same order as opts.GroupBy.
		groupVals := make([]string, len(opts.GroupBy))
		for i := range opts.GroupBy {
			dest = append(dest, &groupVals[i])
		}
		dest = append(dest, &r.PromptTokens, &r.CompletionTokens, &r.Requests, &r.CostUSD)
		if err := rows.Scan(dest...); err != nil {
			return nil, BreakdownTotals{}, fmt.Errorf("scan breakdown: %w", err)
		}
		for i, g := range opts.GroupBy {
			switch g {
			case "user":
				r.User = groupVals[i]
			case "model":
				r.Model = groupVals[i]
			case "protocol":
				r.Protocol = groupVals[i]
			case "outcome":
				r.Outcome = groupVals[i]
			}
		}
		totals.PromptTokens += r.PromptTokens
		totals.CompletionTokens += r.CompletionTokens
		totals.Requests += r.Requests
		totals.CostUSD += r.CostUSD
		out = append(out, r)
	}
	return out, totals, rows.Err()
}

func scanUsage(rows *sql.Rows) ([]Usage, error) {
	var out []Usage
	for rows.Next() {
		var u Usage
		var ts int64
		if err := rows.Scan(&u.ID, &ts, &u.APIKeyID, &u.UserID, &u.Model, &u.Protocol,
			&u.PromptTokens, &u.CompletionTokens, &u.LatencyMS, &u.Outcome, &u.CostUSD, &u.NodeID); err != nil {
			return nil, err
		}
		u.TS = time.Unix(ts, 0)
		out = append(out, u)
	}
	return out, rows.Err()
}

// Trim deletes every usage row older than the newest keep rows.
func (s *sqliteUsage) Trim(ctx context.Context, keep int) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM usage WHERE id < COALESCE((SELECT id FROM usage ORDER BY id DESC LIMIT 1 OFFSET ?), 0)`, max(keep, 1)-1)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
