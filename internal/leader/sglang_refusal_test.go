package leader

// A create the scheduler refuses by name before touching anything — today an
// SGLang pipeline of a Llama-family model with tied embeddings — answers 409
// as every refusal does, and carries the name as error.code, so a manager
// reads a definite answer instead of watching the gang crash-loop. The heal
// loop's shard.heal_failed reads the same code (shardCreateFailure).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/scheduler"
	"github.com/opod-io/opod/internal/store"
)

func TestARefusedSGLangPipelineAnswers409WithItsName(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/org/tied-llama/resolve/main/config.json" {
			_, _ = io.WriteString(w, `{"architectures":["LlamaForCausalLM"],"tie_word_embeddings":true}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(hub.Close)

	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	entry := models.Entry{
		ID:       "tied-llama",
		Source:   models.SourceSpec{Type: "huggingface", Repo: "org/tied-llama"},
		Sharding: models.ShardingSpec{Required: true, Engine: "sglang", DefaultShards: 2},
	}
	orch := scheduler.New(st, nil, log, "")
	orch.HFEndpoint = hub.URL
	cfg := config.Default()
	cfg.Listen = ":0"
	srv := NewServer(cfg, st, &stubLeaderEngine{}, []models.Entry{entry}, log, orch)

	body := `{"model_id":"tied-llama","gang":"g1","shards":2,"pp":2}`
	rec := httptest.NewRecorder()
	srv.createShards(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/shards/create", strings.NewReader(body)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("a refusal before anything is touched is a 409: %d %s", rec.Code, rec.Body.String())
	}
	var e struct {
		Error struct{ Message, Code string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("the error body must be JSON: %v: %s", err, rec.Body.String())
	}
	if e.Error.Code != scheduler.ReasonSGLangPPTiedLlama {
		t.Fatalf("want error.code %q, got %+v", scheduler.ReasonSGLangPPTiedLlama, e.Error)
	}
	if !strings.Contains(e.Error.Message, "tensor parallelism") {
		t.Errorf("the message names the remedy: %q", e.Error.Message)
	}
}

func TestShardCreateFailureNamesARefusal(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &scheduler.RefusedError{Reason: scheduler.ReasonSGLangPPTiedLlama, Msg: "use tp"})
	if code, _ := shardCreateFailure(err); code != scheduler.ReasonSGLangPPTiedLlama {
		t.Fatalf("got code %q", code)
	}
	// A refusal with no name keeps the message it always had and no code.
	plain := fmt.Errorf("%w: not enough workers", scheduler.ErrUnplaceable)
	if code, _ := shardCreateFailure(plain); code != "" || !errors.Is(plain, scheduler.ErrUnplaceable) {
		t.Fatalf("an unnamed refusal has no code, got %q", code)
	}
}
