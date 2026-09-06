package store

// event_log: the leader's durable typed lifecycle stream (§13 item 1).
// AUTOINCREMENT id is the cursor, same contract as the usage table.

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type LogEvent struct {
	ID      int64
	TS      time.Time
	Type    string
	Subject string
	Data    map[string]any
}

type EventLogStore interface {
	Append(typ, subject string, data map[string]any) error
	After(ctx context.Context, afterID int64, limit int) ([]LogEvent, error)
	// Trim keeps only the newest keep rows (bounded ring; the manager pulls by cursor).
	Trim(ctx context.Context, keep int) (int64, error)
}

const eventLogSchema = `
CREATE TABLE IF NOT EXISTS event_log (
  id      INTEGER PRIMARY KEY AUTOINCREMENT,
  ts      INTEGER NOT NULL,
  type    TEXT NOT NULL,
  subject TEXT NOT NULL DEFAULT '',
  data    TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_event_log_type ON event_log(type);
`

type sqliteEventLog struct{ db *sql.DB }

func (s *sqliteEventLog) Append(typ, subject string, data map[string]any) error {
	doc, _ := json.Marshal(data)
	_, err := s.db.Exec(`INSERT INTO event_log(ts, type, subject, data) VALUES(?,?,?,?)`,
		time.Now().Unix(), typ, subject, string(doc))
	return err
}

func (s *sqliteEventLog) After(ctx context.Context, afterID int64, limit int) ([]LogEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts, type, subject, data FROM event_log WHERE id > ? ORDER BY id ASC LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LogEvent{}
	for rows.Next() {
		var e LogEvent
		var ts int64
		var data string
		if err := rows.Scan(&e.ID, &ts, &e.Type, &e.Subject, &data); err != nil {
			return nil, err
		}
		e.TS = time.Unix(ts, 0).UTC()
		_ = json.Unmarshal([]byte(data), &e.Data)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Trim deletes every event older than the newest keep rows.
func (s *sqliteEventLog) Trim(ctx context.Context, keep int) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM event_log WHERE id < COALESCE((SELECT id FROM event_log ORDER BY id DESC LIMIT 1 OFFSET ?), 0)`, max(keep, 1)-1)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
