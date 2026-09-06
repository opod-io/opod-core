package controlplane

// Stateless leader (managed mode): the store is a rebuildable cache, not a
// record. Two things make that safe for the manager pulling the streams:
//
//   - bootUnix rides every stream batch. Cursor ids restart at 1 when the
//     process (and its in-memory store) restarts; a manager that sees a new
//     boot value rewinds its cursor instead of waiting forever after an id
//     that will never come.
//   - the usage and event tables are bounded rings: a trimmer keeps the
//     newest rows only. The manager pulls within seconds, so the rings are
//     generous; a leader that nobody pulls from still cannot grow without
//     bound.

import (
	"context"
	"time"
)

var bootUnix = time.Now().Unix()

const (
	keepUsageRows = 200_000
	keepEventRows = 50_000
)

// StartTrimmer bounds the usage + event tables every minute.
func (s *Server) StartTrimmer(ctx context.Context) {
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				tctx, cancel := context.WithTimeout(ctx, 20*time.Second)
				if n, err := s.store.Usage().Trim(tctx, keepUsageRows); err == nil && n > 0 {
					s.log.Info("usage ring trimmed", "deleted", n, "keep", keepUsageRows)
				}
				if n, err := s.store.EventLog().Trim(tctx, keepEventRows); err == nil && n > 0 {
					s.log.Info("event ring trimmed", "deleted", n, "keep", keepEventRows)
				}
				cancel()
			}
		}
	}()
}
