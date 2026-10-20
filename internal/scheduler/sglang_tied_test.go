package scheduler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/opod-io/opod/internal/fetch"
)

// fakeHub serves configs[repo] at /<repo>/resolve/main/config.json and 404s
// everything else.
func fakeHub(t *testing.T, configs map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for repo, body := range configs {
			if r.URL.Path == "/"+repo+"/resolve/main/config.json" {
				_, _ = w.Write([]byte(body))
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const (
	tiedLlama   = `{"architectures":["LlamaForCausalLM"],"tie_word_embeddings":true,"num_hidden_layers":16}`
	untiedLlama = `{"architectures":["LlamaForCausalLM"],"tie_word_embeddings":false}`
	tiedQwen2   = `{"architectures":["Qwen2ForCausalLM"],"tie_word_embeddings":true}`
)

// SGLang's Llama model file gives a tied output layer to the first pipeline
// stage only, so the last stage crashes loading it. That shape is refused by
// name before the gang is formed — and only that shape.
func TestSGLangPipelineOfATiedLlamaIsRefusedByName(t *testing.T) {
	hub := fakeHub(t, map[string]string{
		"org/tied-llama":   tiedLlama,
		"org/untied-llama": untiedLlama,
		"org/tied-qwen2":   tiedQwen2,
	})
	cases := []struct {
		name    string
		repo    string
		parts   int
		par     Parallelism
		refused bool
	}{
		{"llama tied, pp=2", "org/tied-llama", 2, Parallelism{PP: 2}, true},
		{"llama tied, default split over two parts is pp=2", "org/tied-llama", 2, Parallelism{}, true},
		{"llama tied, pp=1 (one part)", "org/tied-llama", 1, Parallelism{}, false},
		{"llama tied, tp=2 over two parts is pp=1", "org/tied-llama", 2, Parallelism{TP: 2}, false},
		{"qwen2 tied, pp=2 (its model file hands the embedding on)", "org/tied-qwen2", 2, Parallelism{PP: 2}, false},
		{"llama untied, pp=2", "org/untied-llama", 2, Parallelism{PP: 2}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, entry, _ := sglangFixture(t)
			o.HFEndpoint = hub.URL
			entry.Source.Repo = tc.repo
			err := o.checkSGLangPipeline(context.Background(), entry, tc.parts, tc.par)
			if !tc.refused {
				if err != nil {
					t.Fatalf("refused a shape SGLang serves: %v", err)
				}
				return
			}
			var refused *RefusedError
			if !errors.As(err, &refused) || refused.Reason != ReasonSGLangPPTiedLlama {
				t.Fatalf("want a %s refusal, got %v", ReasonSGLangPPTiedLlama, err)
			}
			if !errors.Is(err, ErrUnplaceable) {
				t.Errorf("a refusal before anything is touched is an ErrUnplaceable: %v", err)
			}
			for _, want := range []string{"LlamaForCausalLM", "tie_word_embeddings", "pp=2", "tensor parallelism", "vllm"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal must say %q: %v", want, err)
				}
			}
		})
	}
}

// A config that cannot be read — the Hub down, a 404, a refused token — never
// blocks a create: the gang forms as it did before the check existed.
func TestSGLangPipelineCheckProceedsWhenTheConfigCannotBeRead(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream trouble", http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(refusing.Close)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close() // nothing listens

	for name, endpoint := range map[string]string{
		"hub answers 500": failing.URL, "hub refuses the token": refusing.URL, "hub unreachable": gone.URL,
		"no config at all": fakeHub(t, nil).URL, "not json": fakeHub(t, map[string]string{"org/tied-llama": "<html>"}).URL,
	} {
		t.Run(name, func(t *testing.T) {
			o, entry, _ := sglangFixture(t)
			o.HFEndpoint = endpoint
			entry.Source.Repo = "org/tied-llama"
			if err := o.checkSGLangPipeline(context.Background(), entry, 2, Parallelism{PP: 2}); err != nil {
				t.Fatalf("an unreadable config must not block the create: %v", err)
			}
		})
	}
}

// The check costs no Hub call when it cannot matter (another engine, one
// stage), and none when the model's snapshot is already on disk.
func TestSGLangPipelineCheckReadsOnlyWhatItNeeds(t *testing.T) {
	var calls atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(tiedLlama))
	}))
	t.Cleanup(hub.Close)

	o, entry, _ := sglangFixture(t)
	o.HFEndpoint = hub.URL
	entry.Source.Repo = "org/tied-llama"

	vllm := entry
	vllm.Sharding.Engine = "vllm"
	if err := o.checkSGLangPipeline(context.Background(), vllm, 2, Parallelism{PP: 2}); err != nil {
		t.Fatalf("another engine is not this check's: %v", err)
	}
	if err := o.checkSGLangPipeline(context.Background(), entry, 1, Parallelism{}); err != nil {
		t.Fatalf("one stage has no last stage to lose: %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d Hub calls for creates the check cannot refuse", n)
	}

	// A complete snapshot under ModelsDir answers without the Hub.
	sdir := fetch.SnapshotDir(o.ModelsDir, entry.Source.Repo, "")
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdir, "config.json"), []byte(tiedLlama), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `{"repo":"org/tied-llama","revision":"main","files":[{"name":"config.json","size":` +
		strconv.Itoa(len(tiedLlama)) + `}]}`
	if err := os.WriteFile(filepath.Join(sdir, fetch.SnapshotManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	err := o.checkSGLangPipeline(context.Background(), entry, 2, Parallelism{PP: 2})
	if !errors.Is(err, ErrUnplaceable) {
		t.Fatalf("the snapshot's config says tied Llama; want a refusal, got %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d Hub calls although the snapshot was on disk", n)
	}

	// A local model directory is read in place.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(tiedLlama), 0o644); err != nil {
		t.Fatal(err)
	}
	local := entry
	local.Source.Repo, local.Source.Path = "", dir
	if err := o.checkSGLangPipeline(context.Background(), local, 2, Parallelism{PP: 2}); !errors.Is(err, ErrUnplaceable) {
		t.Fatalf("a local tied-Llama directory must be refused too, got %v", err)
	}
}

// The refusal comes before the gang's guard, its teardown and any process:
// a refused create changes nothing.
func TestARefusedSGLangPipelineTouchesNothing(t *testing.T) {
	w0, w1 := newFakeWorker(t, 0), newFakeWorker(t, 0)
	o, entry, nodes := sglangFixture(t, w0, w1)
	o.HFEndpoint = fakeHub(t, map[string]string{"org/tied-llama": tiedLlama}).URL
	entry.Source.Repo = "org/tied-llama"

	err := o.CreateSharded(context.Background(), entry, "g0", 0, []string{nodes[0].ID, nodes[1].ID}, Parallelism{})
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Reason != ReasonSGLangPPTiedLlama {
		t.Fatalf("want a %s refusal, got %v", ReasonSGLangPPTiedLlama, err)
	}
	if n := len(w0.commands()) + len(w1.commands()); n != 0 {
		t.Errorf("a refused create started %d processes", n)
	}
	release, err := o.forms.claim(entry.ID, "")
	if err != nil {
		t.Fatalf("a refused create left the model's guard held: %v", err)
	}
	release()
}
