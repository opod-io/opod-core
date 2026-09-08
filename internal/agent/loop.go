package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/engines"
)

// Agent is the per-node loop on a worker. It registers with the leader on
// startup, then sends a heartbeat at HeartbeatInterval carrying the list of
// models currently loaded on the local engine.
type Agent struct {
	NodeID       string
	LeaderURL    string
	Token        string
	Address      string
	Capabilities Capabilities
	Engine       engines.Engine // local engine; queried for loaded_models

	HTTP              *http.Client
	HeartbeatInterval time.Duration
	Log               *slog.Logger
}

// Register POSTs node info to /admin/v1/nodes/register on the leader.
func (a *Agent) Register(ctx context.Context) error {
	body, _ := json.Marshal(map[string]any{
		"id":            a.NodeID,
		"hostname":      a.Capabilities.Hostname,
		"os":            a.Capabilities.OS,
		"arch":          a.Capabilities.Arch,
		"ram_gb":        a.Capabilities.RAMGB,
		"address":       a.Address,
		"hardware_json": mustJSON(a.Capabilities),
	})
	_, err := a.post(ctx, "/admin/v1/nodes/register", body)
	return err
}

// Heartbeat sends a lightweight ping to keep the leader informed we're alive,
// including the list of models the local engine currently has loaded so the
// leader can update its placements table.
//
// Returns the HTTP status code so Loop can react differently to 401/404.
func (a *Agent) Heartbeat(ctx context.Context) (int, error) {
	var loaded []string
	if a.Engine != nil {
		// best-effort — a slow engine shouldn't block the heartbeat
		listCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if m, err := a.Engine.List(listCtx); err == nil {
			loaded = m
		}
	}
	hb := map[string]any{
		"id":            a.NodeID,
		"loaded_models": loaded,
	}
	// Sleep tier (build item 13): the engine's own word on whether it sleeps.
	if sl, ok := a.Engine.(engines.Sleeper); ok && a.Engine != nil {
		sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if sleeping, err := sl.Sleeping(sctx); err == nil && sleeping {
			hb["sleeping"] = true
		}
		cancel()
	}
	// Load signals (build item 14) ride the same heartbeat; best-effort too.
	if lr, ok := a.Engine.(engines.LoadReporter); ok && a.Engine != nil {
		lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if ld, err := lr.Load(lctx); err == nil {
			hb["load"] = ld
		}
		cancel()
	}
	body, _ := json.Marshal(hb)
	return a.post(ctx, "/admin/v1/nodes/heartbeat", body)
}

// Loop blocks running register + periodic heartbeat until ctx is done.
//
// Status-code handling:
//   - 401 / 403: token revoked → exit with error so the supervisor (systemd /
//     launchd / the user) can intervene. Burning CPU heartbeating an
//     unauthorized leader is worse than failing fast.
//   - 404: node was forgotten by the leader → try to re-register.
//   - other (network errors, 5xx): exponential backoff up to 1 minute.
func (a *Agent) Loop(ctx context.Context) error {
	if a.HTTP == nil {
		a.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	if a.HeartbeatInterval == 0 {
		a.HeartbeatInterval = 5 * time.Second
	}
	if err := a.Register(ctx); err != nil {
		a.Log.Warn("register failed", "err", err)
	} else {
		a.Log.Info("registered with leader", "leader", a.LeaderURL, "node", a.NodeID)
	}
	backoff := a.HeartbeatInterval
	t := time.NewTimer(backoff)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			code, err := a.Heartbeat(ctx)
			if err == nil {
				backoff = a.HeartbeatInterval
				t.Reset(backoff)
				continue
			}
			switch code {
			case http.StatusUnauthorized, http.StatusForbidden:
				// 401/403 can be TRANSIENT during a leader restart (auth still
				// warming up, or the node row not reloaded yet). Re-register and
				// keep trying instead of exiting — a real revoked token just keeps
				// retrying harmlessly (visible in logs) until it's re-issued, and a
				// transient blip self-heals. Never kill a worker over one 401.
				a.Log.Warn("heartbeat unauthorized; re-registering (was fatal, now retried)",
					"code", code, "err", err)
				if rerr := a.Register(ctx); rerr != nil {
					a.Log.Warn("re-register failed", "err", rerr)
				}
				backoff = a.HeartbeatInterval
			case http.StatusNotFound:
				a.Log.Warn("heartbeat 404; re-registering", "err", err)
				if rerr := a.Register(ctx); rerr != nil {
					a.Log.Warn("re-register failed", "err", rerr)
				}
				backoff = a.HeartbeatInterval
			default:
				a.Log.Warn("heartbeat failed", "code", code, "err", err, "next_backoff", backoff)
				// Cap the reconnect backoff low so a worker rejoins within seconds
				// of the leader coming back — not up to a minute later (which made
				// workers linger "stale" long after a leader restart).
				if backoff < 15*time.Second {
					backoff *= 2
				}
			}
			t.Reset(backoff)
		}
	}
}

// post returns the HTTP status code (0 if the request never reached upstream)
// and an error if one occurred.
//
// Two auth headers are stamped:
//   - Authorization: Bearer <token>     — legacy, accepted by the leader's
//     existing auth middleware for register / heartbeat scope=node tokens
//   - X-Opod-Auth: HMAC               — replaces transmitting the token in
//     clear text for any leader that prefers HMAC verification
//
// The leader's handler verifies HMAC first when the header is present and
// falls back to bearer only if HMAC verification fails (transition mode).
// In a future release the bearer header on these endpoints will go away.
func (a *Agent) post(ctx context.Context, path string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.LeaderURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.Token) // legacy / register path
	auth.SignRequest(req, a.NodeID, a.Token)
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, fmt.Errorf("%s %s: %s: %s", req.Method, path, resp.Status, string(b))
	}
	return resp.StatusCode, nil
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
