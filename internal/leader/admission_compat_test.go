package leader

import (
	"context"
	"time"
)

// holdForCapacity is the question the ADR-082 tests ask of the gate: would
// this request be served now, or within its budget? It admits and gives the
// slot straight back, so a governed pool does not leak one per call. The
// request path itself uses admit and releases on completion (dispatch.go).
func (s *Server) holdForCapacity(ctx context.Context, model, _ string, started time.Time) string {
	release, why := s.admit(ctx, model, started)
	if why == "" {
		release()
	}
	return why
}
