package leader

// A part whose process gave up while the gang formed — the supervisor reports
// "failed", or "crashloop" after its restarts — is a definite answer too: the
// gang did not form and the create will not finish on its own. Its failed
// POST /admin/v1/shards/create carries error.code gang_part_failed, and the
// message carries the engine's own last lines, so a manager stops waiting on
// it the way it does on gang_part_lost instead of retrying until its budget
// runs out.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/scheduler"
)

func TestAFailedCoordinatorIsADefiniteAnswerWithItsOwnLines(t *testing.T) {
	srv, _, w, entry := formationFixture(t)
	const lines = "llama_init_from_model: failed to allocate KV cache of 4194304 tokens"
	w.mu.Lock()
	w.crashed = lines
	w.mu.Unlock()

	body := `{"model_id":"` + entry.ID + `","gang":"g1","shards":1,"nodes":["n1"]}`
	rec := httptest.NewRecorder()
	srv.createShards(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/shards/create", strings.NewReader(body)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("the failure keeps its status: %d %s", rec.Code, rec.Body.String())
	}
	var e struct {
		Error struct{ Message, Type, Code string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("the error body must be JSON: %v: %s", err, rec.Body.String())
	}
	if e.Error.Code != ShardCreatePartFailed || e.Error.Type != "server_error" {
		t.Fatalf("a crashlooping coordinator must carry code %q (type unchanged): %+v", ShardCreatePartFailed, e.Error)
	}
	for _, want := range []string{"gang g1", entry.ID, "n1", "crashloop", lines} {
		if !strings.Contains(e.Error.Message, want) {
			t.Errorf("the reason must name %q: %q", want, e.Error.Message)
		}
	}
	if strings.HasPrefix(e.Error.Message, "upstream") {
		t.Errorf("the reason is the failure itself, without the upstream prefix: %q", e.Error.Message)
	}
}

// The heal loop's shard.heal_failed reads the same code (shardCreateFailure).
func TestShardCreateFailureNamesAFailedPart(t *testing.T) {
	err := errors.Join(ErrUpstream, &scheduler.PartFailedError{Model: "m", Gang: "g1", Process: "s-m-g1-coord", Node: "n1", Status: "failed", ExitErr: "out of memory"})
	code, reason := shardCreateFailure(err)
	if code != ShardCreatePartFailed || !strings.Contains(reason, "out of memory") || strings.Contains(reason, "upstream") {
		t.Fatalf("got code %q reason %q", code, reason)
	}
	if code, _ := shardCreateFailure(errors.New("something else")); code != "" {
		t.Fatalf("an unnamed cause has no code, got %q", code)
	}
}
