package leader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/scheduler"
	"github.com/opod-io/opod/internal/store"
)

// The salt per mode and per caller: team mode keys a team's keys alike and
// teams apart, a key with no team is its own team, key mode keys every key
// apart, none and an anonymous request carry no salt, and a salt never
// carries the id it was made from.
func TestRequestCacheSaltPerMode(t *testing.T) {
	srv := drainedLeader(t)
	srv.cfg.Auth.AdminToken = "endpoint-admin-token"
	if err := srv.applyAuthSnapshot(context.Background(), &AuthSnapshot{Revision: "a1", Keys: []SnapshotKey{
		{ID: "ka1", Name: "a1", Hash: "h-a1", Team: "alpha"},
		{ID: "ka2", Name: "a2", Hash: "h-a2", Team: "alpha"},
		{ID: "kb1", Name: "b1", Hash: "h-b1", Team: "beta"},
		{ID: "ksolo", Name: "solo", Hash: "h-solo"},
	}}); err != nil {
		t.Fatal(err)
	}
	salt := func(keyID string) string {
		ctx := context.Background()
		if keyID != "" {
			ctx = auth.WithTestKey(ctx, &store.APIKey{ID: "k_cp_" + keyID})
		}
		return srv.requestCacheSalt(ctx)
	}
	for _, tc := range []struct {
		mode                  string
		sameTeam, otherTeam   bool // a1 vs a2 equal? a1 vs b1 equal?
		soloSalted, anonEmpty bool
	}{
		{mode: "", sameTeam: true, otherTeam: false, soloSalted: true, anonEmpty: true}, // the default is team
		{mode: "team", sameTeam: true, otherTeam: false, soloSalted: true, anonEmpty: true},
		{mode: "TEAM ", sameTeam: true, otherTeam: false, soloSalted: true, anonEmpty: true},
		{mode: "key", sameTeam: false, otherTeam: false, soloSalted: true, anonEmpty: true},
		{mode: "bogus", sameTeam: true, otherTeam: false, soloSalted: true, anonEmpty: true}, // unknown = team
	} {
		t.Run("mode="+tc.mode, func(t *testing.T) {
			srv.applyPolicySnapshot(&PolicySnapshot{Revision: "p-" + tc.mode, Routing: PolicyRouting{CacheIsolation: tc.mode}})
			a1, a2, b1, solo, anon := salt("ka1"), salt("ka2"), salt("kb1"), salt("ksolo"), salt("")
			if a1 == "" || b1 == "" {
				t.Fatalf("a keyed caller must be salted: a1=%q b1=%q", a1, b1)
			}
			if (a1 == a2) != tc.sameTeam {
				t.Errorf("two keys of one team: equal=%v, want %v", a1 == a2, tc.sameTeam)
			}
			if (a1 == b1) != tc.otherTeam {
				t.Errorf("keys of two teams: equal=%v, want %v", a1 == b1, tc.otherTeam)
			}
			if (solo != "") != tc.soloSalted || solo == a1 || solo == b1 {
				t.Errorf("a key with no team is its own team: %q", solo)
			}
			if (anon == "") != tc.anonEmpty {
				t.Errorf("no key, no caller to separate: %q", anon)
			}
			for _, s := range []string{a1, a2, b1, solo} {
				for _, raw := range []string{"alpha", "beta", "ka1", "ka2", "kb1", "ksolo", "k_cp_", "endpoint-admin-token"} {
					if strings.Contains(s, raw) {
						t.Errorf("salt %q carries %q", s, raw)
					}
				}
				if len(s) != 32 {
					t.Errorf("salt %q: want 32 hex chars", s)
				}
			}
		})
	}
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "p-none", Routing: PolicyRouting{CacheIsolation: "none"}})
	if s := salt("ka1"); s != "" {
		t.Fatalf("mode none must add nothing, got %q", s)
	}

	// Every front of the endpoint holds the same admin token, so a door keys
	// the same caller exactly as the leader does; another endpoint does not.
	door := &Server{cfg: srv.cfg} // a door of the same endpoint: same admin token, its own process
	if got := door.requestCacheSalt(auth.WithTestKey(context.Background(), &store.APIKey{ID: "k_cp_ka1"})); got != cacheSalt([]byte("endpoint-admin-token"), "key\x00k_cp_ka1") {
		t.Fatalf("a door must key a caller as the leader does: %q", got)
	}
	if cacheSalt([]byte("endpoint-admin-token"), "team\x00alpha") == cacheSalt([]byte("other-endpoint"), "team\x00alpha") {
		t.Fatal("two endpoints must not salt one team alike")
	}
}

// End to end through the leader's chat path to a worker: the salt reaches a
// worker whose engine takes one, follows the caller's team, and never
// reaches a llama.cpp worker; a caller's own cache_salt never gets through.
func TestCacheSaltReachesOnlySaltingWorkers(t *testing.T) {
	for _, tc := range []struct {
		engine   string
		mode     string
		wantSalt bool
	}{
		{"vllm", "team", true},
		{"sglang", "team", true},
		{"trtllm", "key", true},
		{"llamacpp", "team", false},
		{"vllm", "none", false},
		{"", "team", false}, // a worker that registered no engine: nobody read it
	} {
		t.Run(fmt.Sprintf("%s/%s", tc.engine, tc.mode), func(t *testing.T) {
			var mu sync.Mutex
			var bodies []map[string]any
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var got map[string]any
				_ = json.NewDecoder(r.Body).Decode(&got)
				mu.Lock()
				bodies = append(bodies, got)
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer worker.Close()

			ctx := context.Background()
			st, err := store.OpenSQLite(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			hw := "{}"
			if tc.engine != "" {
				hw = `{"Engine":"` + tc.engine + `"}`
			}
			if err := st.Nodes().Upsert(ctx, store.Node{ID: "w1", Hostname: "w1", Address: strings.TrimPrefix(worker.URL, "http://"), WorkerToken: "tok",
				State: store.NodeStateReady, LastHeartbeat: time.Now(), HardwareJSON: hw}); err != nil {
				t.Fatal(err)
			}
			if err := st.Placements().Upsert(ctx, store.Placement{NodeID: "w1", ModelID: "qwen", Status: "ready", LastSeen: time.Now()}); err != nil {
				t.Fatal(err)
			}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			cfg := config.Default()
			cfg.Listen = ":0"
			cfg.Auth.AdminToken = "endpoint-admin-token"
			srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, log, scheduler.New(st, nil, log, ""))
			srv.plan.modelID, srv.plan.present = "qwen", true
			plain := map[string]string{"a1": "sk-test-a1", "a2": "sk-test-a2", "b1": "sk-test-b1"}
			hash := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
			if err := srv.applyAuthSnapshot(ctx, &AuthSnapshot{Revision: "a1", RequireKeys: true, Keys: []SnapshotKey{
				{ID: "a1", Name: "a1", Hash: hash(plain["a1"]), Team: "alpha"},
				{ID: "a2", Name: "a2", Hash: hash(plain["a2"]), Team: "alpha"},
				{ID: "b1", Name: "b1", Hash: hash(plain["b1"]), Team: "beta"},
			}}); err != nil {
				t.Fatal(err)
			}
			srv.applyPolicySnapshot(&PolicySnapshot{Revision: "p1", Routing: PolicyRouting{CacheIsolation: tc.mode}})
			ts := httptest.NewServer(srv.routes())
			defer ts.Close()

			for _, k := range []string{"a1", "a2", "b1"} {
				// A caller's own cache_salt is the leader's key and never passes.
				req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
					bytes.NewBufferString(`{"model":"qwen","messages":[{"role":"user","content":"hi"}],"cache_salt":"chosen-by-caller","seed":7}`))
				req.Header.Set("Authorization", "Bearer "+plain[k])
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("key %s: %d %s", k, resp.StatusCode, b)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if len(bodies) != 3 {
				t.Fatalf("the worker saw %d requests, want 3", len(bodies))
			}
			salts := make([]string, 3)
			for i, b := range bodies {
				if b["seed"] != float64(7) {
					t.Errorf("request %d: the caller's other fields must still pass: %v", i, b)
				}
				s, present := b["cache_salt"].(string)
				if s == "chosen-by-caller" {
					t.Fatalf("request %d: the caller's own cache_salt reached the worker", i)
				}
				if present != tc.wantSalt {
					t.Fatalf("request %d: cache_salt present=%v, want %v (%v)", i, present, tc.wantSalt, b)
				}
				salts[i] = s
			}
			if !tc.wantSalt {
				return
			}
			switch tc.mode {
			case "team":
				if salts[0] != salts[1] || salts[0] == salts[2] {
					t.Fatalf("team mode: one team alike, two teams apart: %q", salts)
				}
			case "key":
				if salts[0] == salts[1] || salts[1] == salts[2] || salts[0] == salts[2] {
					t.Fatalf("key mode: every key apart: %q", salts)
				}
			}
		})
	}
}
