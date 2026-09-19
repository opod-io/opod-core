package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"syscall"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/engines"
)

// Agent is the per-node loop on a worker. It registers with the leader on
// startup, then sends a heartbeat at HeartbeatInterval carrying the list of
// models currently loaded on the local engine.
type Agent struct {
	NodeID    string
	LeaderURL string
	Token     string
	Address   string
	// BootID names THIS process (R10.1): minted once per agent start and sent
	// on register and every heartbeat, so the leader tells a recreated pod
	// or a restarted container from the incarnation it recorded work on —
	// the node id alone is stable across both. Empty = minted by Loop.
	BootID       string
	Capabilities Capabilities
	Engine       engines.Engine // local engine; queried for loaded_models
	// Aliases translates the engine's native model names into the ids this
	// worker was asked to load, so the leader's placements are keyed by the
	// identity the plan named. Shared with the Server; nil is safe.
	Aliases *Aliases

	HTTP              *http.Client
	HeartbeatInterval time.Duration
	Log               *slog.Logger
}

// MaxRefusedRegisters is how many consecutive re-registers the leader may
// refuse (401/403) before the worker exits (R10.2). A heartbeat 401 is
// answered by a re-register with the token this process started with — the
// path that survives a stateless leader restart and must stay cheap. When
// the re-register ITSELF is refused this many times (≈30 s at the default
// interval) the token is dead: rotated, or minted by an endpoint that no
// longer exists. Only a restart re-reads the mounted Secret, so one exit
// replaces a 401 storm that would never end.
const MaxRefusedRegisters = 6

// NewBootID mints the identity of one agent process.
func NewBootID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Register POSTs node info to /admin/v1/nodes/register on the leader.
func (a *Agent) Register(ctx context.Context) error {
	_, err := a.register(ctx)
	return err
}

func (a *Agent) register(ctx context.Context) (int, error) {
	body, _ := json.Marshal(map[string]any{
		"id":            a.NodeID,
		"hostname":      a.Capabilities.Hostname,
		"os":            a.Capabilities.OS,
		"arch":          a.Capabilities.Arch,
		"ram_gb":        a.Capabilities.RAMGB,
		"address":       a.Address,
		"hardware_json": mustJSON(a.Capabilities),
		"boot_id":       a.BootID,
	})
	return a.post(ctx, "/admin/v1/nodes/register", body)
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
			loaded = a.Aliases.Resolve(m)
		} else if engineNotRunning(err) {
			// Nothing listens where the engine would: a definite report — nothing is
			// loaded — not the absence of one. "No report" (nil) is for an engine that
			// did not answer in time or answered with an error. The difference
			// matters: the leader takes a worker with no report for too long out of
			// rotation, and a worker whose engine is launched BY a load (vLLM, SGLang,
			// llama.cpp), or a llama.cpp RPC part (an rpc-server, never an engine),
			// has nothing listening by design.
			loaded = []string{}
		}
	}
	hb := map[string]any{
		"id":            a.NodeID,
		"loaded_models": loaded,
		"boot_id":       a.BootID,
	}
	// loaded_models is what this worker answers a request for. For an engine
	// that keeps installed models and loads one on its first request (Ollama)
	// that is more than what is in memory, so such an engine ALSO says which of
	// them are resident right now — sent even when empty, omitted when the
	// engine cannot tell the two apart (then everything listed is resident).
	if rl, ok := a.Engine.(engines.ResidentLister); ok && a.Engine != nil {
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if resident, err := rl.Resident(rctx); err == nil {
			names := make([]string, 0, len(resident))
			for _, m := range resident {
				names = append(names, m.Name)
			}
			hb["resident_models"] = a.Aliases.Resolve(names)
		}
		cancel()
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
//   - 401 / 403 on a heartbeat: re-register with the token this process
//     holds (a leader restart answers 401 until its auth warms up). When the
//     re-register is refused MaxRefusedRegisters times in a row the token is
//     dead → return an error so the supervisor restarts the process and it
//     re-reads its Secret (R10.2). Never exit over one 401.
//   - 404: node was forgotten by the leader → try to re-register.
//   - other (network errors, 5xx): exponential backoff up to 15 s.
func (a *Agent) Loop(ctx context.Context) error {
	if a.HTTP == nil {
		a.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	if a.HeartbeatInterval == 0 {
		a.HeartbeatInterval = 5 * time.Second
	}
	if a.BootID == "" {
		a.BootID = NewBootID()
	}
	refused := 0 // consecutive refused re-registers
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
				refused = 0
				backoff = a.HeartbeatInterval
				t.Reset(backoff)
				continue
			}
			switch code {
			case http.StatusUnauthorized, http.StatusForbidden:
				// 401/403 can be TRANSIENT during a leader restart (auth still
				// warming up, or the node row not reloaded yet). Re-register and
				// keep trying — a transient blip self-heals. Only a re-register
				// that is itself refused, MaxRefusedRegisters times running,
				// proves the token dead (R10.2).
				a.Log.Warn("heartbeat unauthorized; re-registering", "code", code, "err", err)
				rcode, rerr := a.register(ctx)
				switch {
				case rerr == nil:
					refused = 0
				case rcode == http.StatusUnauthorized || rcode == http.StatusForbidden:
					refused++
					a.Log.Warn("re-register refused", "code", rcode, "err", rerr, "refused", refused, "max", MaxRefusedRegisters)
					if refused >= MaxRefusedRegisters {
						return fmt.Errorf("join token refused %d times in a row: the token this worker started with is dead (rotated, or minted by an endpoint that is gone) — exiting so a restart reads the current one", refused)
					}
				default:
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

// engineNotRunning: the connection was refused — no process listens on the
// engine's address. A timeout is NOT this: a hung engine may hold a model.
func engineNotRunning(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
