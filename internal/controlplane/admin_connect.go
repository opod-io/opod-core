package controlplane

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/opod-io/opod/internal/control"
)

// listConnectClients serves GET /admin/v1/connect/clients.
// Returns the list of supported tools that `opod connect` (and the
// dashboard Connect tab) can render snippets for.
func (s *Server) listConnectClients(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, control.Clients())
}

// renderConnectSnippet serves POST /admin/v1/connect/snippet.
// Body: {client, base_url?, token?, model?}. Falls back to the leader's
// own base URL and the calling request's bearer token if not supplied.
// Invokes control.ConnectSnippet — the same Go function `opod connect`
// uses, per the CLI-as-source-of-truth principle.
func (s *Server) renderConnectSnippet(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req struct {
		Client  string `json:"client"`
		BaseURL string `json:"base_url"`
		Token   string `json:"token"`
		Model   string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.BaseURL == "" {
		req.BaseURL = s.defaultBaseURL(r)
	}
	if req.Token == "" {
		req.Token = bearerFromRequest(r)
	}
	if req.Token == "" {
		writeJSONError(w, http.StatusBadRequest, "token required (either in body or via Authorization header)")
		return
	}
	out, err := control.ConnectSnippet(control.ConnectInput{
		Client:  req.Client,
		BaseURL: req.BaseURL,
		Token:   req.Token,
		Model:   req.Model,
	})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// defaultBaseURL returns the URL clients should embed in snippets. Uses
// cfg.ExternalURL if set, otherwise derives from the request's Host
// header (covers users who reach the dashboard via a reverse proxy or a
// tailnet hostname without setting external_url in config).
func (s *Server) defaultBaseURL(r *http.Request) string {
	if s.cfg.ExternalURL != "" {
		return strings.TrimRight(s.cfg.ExternalURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	// Honor forwarding headers only behind a trusted proxy. Otherwise a
	// spoofed X-Forwarded-Host/-Proto could mint an invite/connect card
	// pointing recipients at an attacker-controlled host (token phishing).
	if s.cfg.TrustProxyHeaders {
		if h := r.Header.Get("X-Forwarded-Proto"); h != "" {
			scheme = h
		}
		if h := r.Header.Get("X-Forwarded-Host"); h != "" {
			host = h
		}
	}
	if host == "" {
		host = "localhost" + s.cfg.Listen
	}
	return scheme + "://" + host
}

// bearerFromRequest extracts the API key from the Authorization header,
// or "" if it isn't a Bearer token. Used as a fallback in endpoints
// that can echo the caller's own token back in a generated snippet.
func bearerFromRequest(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
