package controlplane

// Fallback forward (P12-2): when this leader has no serving capacity and the
// policy snapshot names a fallback target, the request is forwarded there
// instead of answering 503 "waking". The target is any OpenAI-compatible base
// URL — typically another opod endpoint's Service, or a remote endpoint
// (P12-4). Streaming passes through untouched; the response carries
// X-Opod-Fallback so callers and the usage stream can tell.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/httpsafe"
)

const fallbackTimeout = 10 * time.Minute // long generations; the client's own timeout still applies

// forwardToFallback proxies one chat request to fb. Returns false when no
// forward was attempted (caller answers its usual 503).
func (s *Server) forwardToFallback(w http.ResponseWriter, r *http.Request, body []byte, fb *PolicyRouting) bool {
	if fb == nil || fb.FallbackURL == "" {
		return false
	}
	if fb.FallbackModel != "" {
		body = setModel(body, fb.FallbackModel)
	}
	ctx, cancel := context.WithTimeout(r.Context(), fallbackTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fb.FallbackURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if a := r.Header.Get("Accept"); a != "" {
		req.Header.Set("Accept", a)
	}
	switch {
	case fb.FallbackKey != "":
		req.Header.Set("Authorization", "Bearer "+fb.FallbackKey)
	case r.Header.Get("Authorization") != "":
		req.Header.Set("Authorization", r.Header.Get("Authorization"))
	}
	req.Header.Set("X-Opod-Forwarded-By", s.cfg.Router.DefaultModel)
	client := httpsafe.NewClient(fallbackTimeout, false) // in-cluster targets are the point
	resp, err := client.Do(req)
	if err != nil {
		s.log.Warn("fallback unreachable — answering 503", "target", fb.FallbackURL, "err", err)
		s.logEvent("routing.fallback_failed", fb.FallbackURL, map[string]any{"err": err.Error()})
		return false
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Cache-Control", "X-Request-Id"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("X-Opod-Fallback", fb.FallbackURL)
	w.WriteHeader(resp.StatusCode)
	streaming := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	if fl, ok := w.(http.Flusher); ok && streaming {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					break
				}
				fl.Flush()
			}
			if rerr != nil {
				break
			}
		}
	} else {
		_, _ = io.Copy(w, resp.Body)
	}
	s.logEvent("routing.fallback", fb.FallbackURL, map[string]any{"status": resp.StatusCode, "model": fb.FallbackModel})
	return true
}
