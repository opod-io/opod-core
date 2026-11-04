package leader

import (
	"net/http"

	"github.com/opod-io/opod/internal/config"
)

// ---- config view ----

// getConfig returns a sanitized view of the effective config (secrets
// redacted, config.ViewOf). Editing is file-based — too easy to brick a
// running cluster via a typo'd HTTP PUT.
func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, config.ViewOf(s.cfg))
}
