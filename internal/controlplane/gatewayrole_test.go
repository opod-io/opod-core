package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/gateway"
	"github.com/opod-io/opod/internal/store"
)

// T11.1 slice 2, end to end in one process: a LEADER with a worker joined to
// it, and a DOOR that has never spoken to that worker. It is the whole claim of
// ADR-063 in one test — a second front for one brain:
//
//   - the door learns the worker from the leader (registry + placements), so it
//     can route without a heartbeat ever reaching it;
//   - the door serves /v1 and NOT /admin/v1, because the registry, the join
//     tokens and the gang calls belong to exactly one process;
//   - its usage lands in the LEADER's store, once, even when the push is
//     retried;
//   - the ceilings it enforces are the leader's numbers — a share of the rate,
//     and the key's whole day rather than this door's slice of it.
func TestAGatewayIsASecondFrontForOneBrain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// ---- the leader, with a worker ----
	leaderCfg := config.Default()
	leaderCfg.Listen = ":0"
	leaderCfg.Auth.RequireKeys = true
	leaderStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer leaderStore.Close()

	adminPlain, adminRec, err := auth.Generate("door-1", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := leaderStore.APIKeys().Create(ctx, adminRec); err != nil {
		t.Fatal(err)
	}
	nodePlain, nodeRec, err := auth.Generate("worker", "node", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := leaderStore.APIKeys().Create(ctx, nodeRec); err != nil {
		t.Fatal(err)
	}
	// The customer key: a rate of 2 requests a minute and a daily quota, so
	// both ceilings are observable.
	custPlain, custRec, err := auth.Generate("customer", "user", "u1")
	if err != nil {
		t.Fatal(err)
	}
	custRec.RPMLimit = 2
	custRec.QuotaDailyTokens = 1000
	if err := leaderStore.APIKeys().Create(ctx, custRec); err != nil {
		t.Fatal(err)
	}

	leader := NewServer(leaderCfg, leaderStore, &stubLeaderEngine{}, nil, log, nil)
	leaderHTTP := httptest.NewServer(leader.routes())
	defer leaderHTTP.Close()

	worker := &agent.Agent{
		NodeID:    "w1",
		LeaderURL: leaderHTTP.URL,
		Token:     nodePlain,
		Address:   "127.0.0.1:18081",
		Capabilities: agent.Capabilities{
			Hostname: "w1.local", OS: "linux", Arch: "amd64", RAMGB: 64,
		},
		Engine:            &stubWorkerEngine{loaded: []string{"qwen2.5-0.5b-gguf"}},
		HeartbeatInterval: 50 * time.Millisecond,
		Log:               log,
		HTTP:              &http.Client{Timeout: 5 * time.Second},
	}
	if err := worker.Register(ctx); err != nil {
		t.Fatalf("worker register: %v", err)
	}
	if _, err := worker.Heartbeat(ctx); err != nil {
		t.Fatalf("worker heartbeat: %v", err)
	}

	// ---- the door ----
	// Its store is its own, rebuildable cache. The customer key is seeded into
	// it directly: in production the control plane mounts the same auth.yaml on
	// every door (core's watched auth file), which is how a door authenticates
	// a key it never issued.
	doorCfg := config.Default()
	doorCfg.Listen = ":0"
	doorCfg.Auth.RequireKeys = true
	doorCfg.Auth.AdminToken = adminPlain
	doorCfg.Env.Role = "gateway"
	doorCfg.Env.LeaderURL = leaderHTTP.URL
	doorCfg.Env.GatewayID = "door-1"
	doorStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer doorStore.Close()
	if err := doorStore.APIKeys().Create(ctx, custRec); err != nil {
		t.Fatal(err)
	}

	door := NewServer(doorCfg, doorStore, &stubLeaderEngine{}, nil, log, nil)
	if err := door.StartGatewayRole(ctx); err != nil {
		t.Fatalf("StartGatewayRole: %v", err)
	}
	doorHTTP := httptest.NewServer(door.routes())
	defer doorHTTP.Close()

	// 1) The door learned the worker, and what it holds. Without the
	//    placements it would have a machine it cannot send one request to.
	if n, err := doorStore.Nodes().Get(ctx, "w1"); err != nil || n == nil || n.State != "ready" {
		t.Fatalf("the door mirrors the leader's registry: %+v (%v)", n, err)
	}
	holders, err := doorStore.Placements().GetByModel(ctx, "qwen2.5-0.5b-gguf")
	if err != nil || len(holders) != 1 || holders[0].NodeID != "w1" {
		t.Fatalf("the door can find a holder of the model: %+v (%v)", holders, err)
	}

	// 2) /v1/models is served — the door is a front.
	if body, code := get(t, doorHTTP.URL+"/v1/models", custPlain); code != http.StatusOK {
		t.Fatalf("a door serves /v1/models: %d %s", code, body)
	} else if !bytes.Contains(body, []byte("qwen2.5-0.5b-gguf")) {
		t.Errorf("the mirrored worker's model is listed: %s", body)
	}

	// 3) …and /admin/v1 is not there AT ALL, with the door's own admin token.
	//    Not 403 — absent: a door that could be talked out of its answer is a
	//    second place a worker could join or a gang be torn down.
	for _, path := range []string{"/admin/v1/nodes", "/admin/v1/tokens", "/admin/v1/shards", "/admin/v1/spend"} {
		if _, code := get(t, doorHTTP.URL+path, adminPlain); code != http.StatusNotFound {
			t.Errorf("%s on a door must not exist, got %d", path, code)
		}
	}
	// The probes stay: Kubernetes decides whether the door takes traffic.
	for _, path := range []string{"/healthz", "/readyz", "/loadz", "/gatewayz"} {
		if _, code := get(t, doorHTTP.URL+path, ""); code != http.StatusOK && code != http.StatusServiceUnavailable {
			t.Errorf("%s must answer on a door, got %d", path, code)
		}
	}

	// 4) Usage recorded at the door reaches the LEADER's store, exactly once
	//    even though the row is pushed twice.
	row := store.Usage{TS: time.Now(), APIKeyID: custRec.ID, UserID: "u1",
		Model: "qwen2.5-0.5b-gguf", Protocol: "openai", PromptTokens: 100,
		CompletionTokens: 20, LatencyMS: 42, Outcome: "ok", NodeID: "w1"}
	door.recordUsageForGateway(row, "req_deadbeef")
	if err := door.front.push.Flush(ctx, 100); err != nil {
		t.Fatalf("push: %v", err)
	}
	// The same row again: a gateway that retried after a half-written response
	// must not bill the customer twice.
	door.front.push.Add(gateway.RowFrom("door-1:req_deadbeef", row))
	if err := door.front.push.Flush(ctx, 100); err != nil {
		t.Fatalf("second push: %v", err)
	}
	used, err := leaderStore.Usage().SumTokensSince(ctx, custRec.ID, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if used != 120 {
		t.Errorf("the leader's store is the record, and the retry is deduplicated: tokens=%d want 120", used)
	}

	// 5) The leader now sees a door, and says so.
	body, code := get(t, leaderHTTP.URL+"/admin/v1/gateways", adminPlain)
	if code != http.StatusOK {
		t.Fatalf("GET /admin/v1/gateways: %d %s", code, body)
	}
	var doors struct {
		Doors     int `json:"doors"`
		LagBoundS int `json:"lag_bound_s"`
		Gateways  []struct {
			Gateway string `json:"gateway"`
			Live    bool   `json:"live"`
		} `json:"gateways"`
	}
	if err := json.Unmarshal(body, &doors); err != nil {
		t.Fatal(err)
	}
	if doors.Doors != 1 || len(doors.Gateways) != 1 || doors.Gateways[0].Gateway != "door-1" || !doors.Gateways[0].Live {
		t.Errorf("the leader names the door it has heard from: %s", body)
	}
	if doors.LagBoundS != 10 {
		t.Errorf("the quota lag bound is published, not documented: %d", doors.LagBoundS)
	}

	// 6) The ceilings the door enforces are the leader's. Poll the snapshot:
	//    one live door, so the share is the whole rate of 2/min…
	if err := door.front.spend.Poll(ctx); err != nil {
		t.Fatalf("spend poll: %v", err)
	}
	if rpm, _ := door.front.spend.Share(custRec.ID); rpm != 2 {
		t.Errorf("one door gets the whole rate: rpm=%d", rpm)
	}
	// …and with a second door heard from, half of it — a flat share would hand
	// a keep-alive client pinned to one door 1/N of what it bought, so the
	// number moves with the doors the leader can actually see.
	leader.gateways.note("door-2", "req_other", time.Now())
	if err := door.front.spend.Poll(ctx); err != nil {
		t.Fatalf("spend re-poll: %v", err)
	}
	if rpm, _ := door.front.spend.Share(custRec.ID); rpm != 1 {
		t.Errorf("two doors split the rate: rpm=%d want 1", rpm)
	}

	// 7) The daily quota is the key's WHOLE day, not this door's slice. The
	//    leader has 1000 of quota and 120 spent; push the rest and the door
	//    must refuse the next request — even though its own store holds one
	//    row of 120 tokens and would have let the key through.
	big := row
	big.PromptTokens, big.CompletionTokens = 900, 0
	door.recordUsageForGateway(big, "req_cafe")
	if err := door.front.push.Flush(ctx, 100); err != nil {
		t.Fatalf("push: %v", err)
	}
	if err := door.front.spend.Poll(ctx); err != nil {
		t.Fatalf("spend poll: %v", err)
	}
	if over, u, q := door.front.spend.QuotaExceeded(custRec.ID); !over || q != 1000 || u < 1020 {
		t.Fatalf("the door reads the key's whole day: over=%v used=%d quota=%d", over, u, q)
	}
	respBody, code := post(t, doorHTTP.URL+"/v1/chat/completions", custPlain,
		`{"model":"qwen2.5-0.5b-gguf","messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusTooManyRequests {
		t.Fatalf("a door over the key's daily quota refuses: %d %s", code, respBody)
	}
	if !bytes.Contains(respBody, []byte("quota")) {
		t.Errorf("and says which ceiling it hit: %s", respBody)
	}

	// 8) The door reports its own staleness and backlog — the only symptom a
	//    queue that is not draining otherwise has.
	st := door.GatewayStatus()
	if st["gateway"] != "door-1" || st["registry_fresh"] != true {
		t.Errorf("a door says who it is and whether its list is fresh: %+v", st)
	}
	if st["usage_queued"].(int) != 0 || st["spend_lag_bound_s"].(int) != 10 {
		t.Errorf("backlog and the published bound: %+v", st)
	}
}

// A door that cannot be configured must not start half-built: with no leader
// it would serve every request with a 503 that reads like an outage of the
// endpoint rather than a misconfiguration of the door.
func TestAGatewayRefusesToStartWithoutALeader(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Default()
	cfg.Env.Role = "gateway"
	cfg.Auth.AdminToken = "sk-orc-contract-test-admin"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := NewServer(cfg, st, &stubLeaderEngine{}, nil, log, nil)
	if err := s.StartGatewayRole(context.Background()); err == nil {
		t.Fatal("a gateway with no OPOD_LEADER_URL must refuse to start")
	}
	// And with a leader but no token: the registry read is admin-keyed like
	// every other /admin/v1 call.
	cfg.Env.LeaderURL = "http://127.0.0.1:1"
	cfg.Auth.AdminToken = ""
	s2 := NewServer(cfg, st, &stubLeaderEngine{}, nil, log, nil)
	if err := s2.StartGatewayRole(context.Background()); err == nil {
		t.Fatal("a gateway with no admin token must refuse to start")
	}
	// A leader that is there but unreachable is also a refusal, not a silent
	// door: the first mirror is the one sync a door cannot serve without.
	cfg.Auth.AdminToken = "sk-orc-contract-test-admin"
	s3 := NewServer(cfg, st, &stubLeaderEngine{}, nil, log, nil)
	if err := s3.StartGatewayRole(context.Background()); err == nil {
		t.Fatal("a gateway that cannot read the registry must refuse to start")
	}
}

func get(t *testing.T, url, token string) ([]byte, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode
}

func post(t *testing.T, url, token, body string) ([]byte, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode
}
