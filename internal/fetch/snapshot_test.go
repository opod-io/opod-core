package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// snapshotHub is a Hub with one repo: the revision-info listing and the
// resolve route for each file. lfs names the files the listing gives a sha256.
type snapshotHub struct {
	*httptest.Server
	files    map[string]string // name → body
	lfs      map[string]bool
	wrongSum map[string]bool // listing declares a digest the bytes do not have
	gets     sync.Map        // name → *int32
	lists    int32
	slow     time.Duration
}

func newSnapshotHub(t *testing.T, repo, rev string, files map[string]string, lfs ...string) *snapshotHub {
	t.Helper()
	h := &snapshotHub{files: files, lfs: map[string]bool{}, wrongSum: map[string]bool{}}
	for _, n := range lfs {
		h.lfs[n] = true
	}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/"+repo+"/revision/"+rev {
			if r.URL.Query().Get("blobs") != "true" {
				http.Error(w, "sizes and digests need blobs=true", http.StatusBadRequest)
				return
			}
			atomic.AddInt32(&h.lists, 1)
			type lfsInfo struct {
				SHA256 string `json:"sha256"`
				Size   int64  `json:"size"`
			}
			type sibling struct {
				Name string   `json:"rfilename"`
				Size int64    `json:"size"`
				LFS  *lfsInfo `json:"lfs,omitempty"`
			}
			var sibs []sibling
			for name, body := range h.files {
				s := sibling{Name: name, Size: int64(len(body))}
				if h.lfs[name] {
					sum := sha256.Sum256([]byte(body))
					if h.wrongSum[name] {
						sum = sha256.Sum256([]byte("other bytes"))
					}
					s.LFS = &lfsInfo{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}
				}
				sibs = append(sibs, s)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": "c0ffee", "siblings": sibs})
			return
		}
		prefix := "/" + repo + "/resolve/" + rev + "/"
		name := strings.TrimPrefix(r.URL.Path, prefix)
		body, ok := h.files[name]
		if !strings.HasPrefix(r.URL.Path, prefix) || !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodHead {
			return
		}
		n, _ := h.gets.LoadOrStore(name, new(int32))
		atomic.AddInt32(n.(*int32), 1)
		time.Sleep(h.slow)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *snapshotHub) pulled() map[string]int {
	out := map[string]int{}
	h.gets.Range(func(k, v any) bool {
		out[k.(string)] = int(atomic.LoadInt32(v.(*int32)))
		return true
	})
	return out
}

func dirFiles(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, de := range des {
		if !strings.HasSuffix(de.Name(), MarkerSuffix) {
			out = append(out, de.Name())
		}
	}
	sort.Strings(out)
	return out
}

// A realistic repo: sharded safetensors with their index, the same tensors
// again as .bin and as one consolidated file, another runtime's copy in a
// subdirectory, and the usual repo furniture.
var llamaRepo = map[string]string{
	"config.json":                      `{"architectures":["LlamaForCausalLM"]}`,
	"generation_config.json":           `{}`,
	"tokenizer.json":                   `{"tok":1}`,
	"tokenizer_config.json":            `{"cfg":1}`,
	"tokenizer.model":                  "sentencepiece",
	"chat_template.jinja":              "{{ messages }}",
	"modeling_custom.py":               "class M: pass",
	"model.safetensors.index.json":     `{"weight_map":{}}`,
	"model-00001-of-00002.safetensors": "shard-one-bytes",
	"model-00002-of-00002.safetensors": "shard-two-bytes!",
	"consolidated.safetensors":         "the same tensors, one file",
	"pytorch_model.bin":                "the same tensors again, pickled",
	"original/consolidated.00.pth":     "the vendor's own format",
	"onnx/model.onnx":                  "another runtime",
	"README.md":                        "# model card",
	".gitattributes":                   "*.safetensors filter=lfs",
	"banner.png":                       "png",
	"empty.json":                       "",
}

var llamaWant = []string{
	SnapshotManifestName,
	"chat_template.jinja", "config.json", "generation_config.json",
	"model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors", "model.safetensors.index.json",
	"modeling_custom.py", "tokenizer.json", "tokenizer.model", "tokenizer_config.json",
}

func TestSnapshotFetchesWhatAnEngineLoadsAndNothingElse(t *testing.T) {
	hub := newSnapshotHub(t, "org/llama", "v1", llamaRepo, "model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors", "tokenizer.model")
	dir := t.TempDir()
	sdir, err := Snapshot(context.Background(), "org/llama", dir, Options{Endpoint: hub.URL, Revision: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "org-llama@v1"); sdir != want {
		t.Fatalf("snapshot dir = %s, want the revision directory %s", sdir, want)
	}
	got := dirFiles(t, sdir)
	sort.Strings(llamaWant)
	if strings.Join(got, " ") != strings.Join(llamaWant, " ") {
		t.Fatalf("snapshot holds\n  %v\nwant\n  %v", got, llamaWant)
	}
	// Same marker as a GGUF: every file is reclaimable by the cache, by name.
	for _, f := range got {
		if f == SnapshotManifestName {
			continue
		}
		m, err := readMarker(filepath.Join(sdir, f))
		if err != nil || m.Repo != "org/llama" || m.File != f {
			t.Errorf("%s: marker = %+v, %v", f, m, err)
		}
	}
	var man SnapshotManifest
	b, _ := os.ReadFile(filepath.Join(sdir, SnapshotManifestName))
	if err := json.Unmarshal(b, &man); err != nil || man.Commit != "c0ffee" || man.Revision != "v1" || len(man.Files) != len(llamaWant)-1 {
		t.Fatalf("manifest = %+v, %v", man, err)
	}
	if p, ok := SnapshotPath(dir, "org/llama", "v1"); !ok || p != sdir {
		t.Fatalf("SnapshotPath = %q, %v — a complete snapshot must be found", p, ok)
	}
	if _, ok := SnapshotPath(dir, "org/llama", "v2"); ok {
		t.Fatal("another revision is not this snapshot")
	}
}

func TestSnapshotSecondCallDownloadsNothing(t *testing.T) {
	hub := newSnapshotHub(t, "org/llama", "v1", llamaRepo, "model-00001-of-00002.safetensors")
	dir := t.TempDir()
	opt := Options{Endpoint: hub.URL, Revision: "v1"}
	if _, err := Snapshot(context.Background(), "org/llama", dir, opt); err != nil {
		t.Fatal(err)
	}
	first := hub.pulled()
	if _, err := Snapshot(context.Background(), "org/llama", dir, opt); err != nil {
		t.Fatal(err)
	}
	for name, n := range hub.pulled() {
		if n != first[name] || n != 1 {
			t.Errorf("%s was pulled %d times over two calls, want once", name, n)
		}
	}
}

func TestSnapshotConcurrentCallersPullEachFileOnce(t *testing.T) {
	hub := newSnapshotHub(t, "org/llama", "v1", llamaRepo, "model-00001-of-00002.safetensors")
	hub.slow = 50 * time.Millisecond
	dir := t.TempDir()
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = Snapshot(context.Background(), "org/llama", dir, Options{Endpoint: hub.URL, Revision: "v1", LockWait: 20 * time.Second})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	for name, n := range hub.pulled() {
		if n != 1 {
			t.Errorf("%s was pulled %d times by three concurrent callers, want once (same lock as a GGUF)", name, n)
		}
	}
}

func TestSnapshotWrongDigestIsNotASnapshot(t *testing.T) {
	hub := newSnapshotHub(t, "org/llama", "v1", llamaRepo, "model-00001-of-00002.safetensors", "model-00002-of-00002.safetensors")
	hub.wrongSum["model-00002-of-00002.safetensors"] = true
	dir := t.TempDir()
	_, err := Snapshot(context.Background(), "org/llama", dir, Options{Endpoint: hub.URL, Revision: "v1"})
	if err == nil || !strings.Contains(err.Error(), "model-00002-of-00002.safetensors") {
		t.Fatalf("err = %v, want the shard that does not hash to the Hub's digest", err)
	}
	sdir := SnapshotDir(dir, "org/llama", "v1")
	if _, err := os.Stat(filepath.Join(sdir, "model-00002-of-00002.safetensors")); !os.IsNotExist(err) {
		t.Error("the wrong shard survived to be found complete by the next call")
	}
	if _, ok := SnapshotPath(dir, "org/llama", "v1"); ok {
		t.Error("a snapshot with a bad shard was reported complete")
	}
}

func TestSnapshotMissingAFileIsNotComplete(t *testing.T) {
	hub := newSnapshotHub(t, "org/llama", "v1", llamaRepo)
	dir := t.TempDir()
	sdir, err := Snapshot(context.Background(), "org/llama", dir, Options{Endpoint: hub.URL, Revision: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	// What a cache prune does to a shard nobody referenced.
	if err := os.Remove(filepath.Join(sdir, "model-00001-of-00002.safetensors")); err != nil {
		t.Fatal(err)
	}
	if _, ok := SnapshotPath(dir, "org/llama", "v1"); ok {
		t.Fatal("a snapshot missing a shard was handed out as complete")
	}
	if _, err := Snapshot(context.Background(), "org/llama", dir, Options{Endpoint: hub.URL, Revision: "v1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := SnapshotPath(dir, "org/llama", "v1"); !ok {
		t.Fatal("fetching again did not repair the snapshot")
	}
	if n := hub.pulled()["model-00001-of-00002.safetensors"]; n != 2 {
		t.Errorf("the missing shard was pulled %d times in total, want 2", n)
	}
	if n := hub.pulled()["model-00002-of-00002.safetensors"]; n != 1 {
		t.Errorf("the intact shard was pulled %d times, want 1", n)
	}
}

func TestSnapshotFallsBackToPyTorchWeights(t *testing.T) {
	hub := newSnapshotHub(t, "org/old", "main", map[string]string{
		"config.json":       `{}`,
		"pytorch_model.bin": "pickled tensors",
		"training_args.bin": "left by the trainer",
		"optimizer.pt":      "left by the trainer",
	})
	dir := t.TempDir()
	sdir, err := Snapshot(context.Background(), "org/old", dir, Options{Endpoint: hub.URL})
	if err != nil {
		t.Fatal(err)
	}
	// Unpinned still gets its own directory: every repo has a config.json.
	if want := filepath.Join(dir, "org-old@main"); sdir != want {
		t.Fatalf("snapshot dir = %s, want %s", sdir, want)
	}
	if got := strings.Join(dirFiles(t, sdir), " "); got != SnapshotManifestName+" config.json pytorch_model.bin" {
		t.Fatalf("snapshot holds %q", got)
	}
}

func TestSnapshotRefusesWhatItCannotHonour(t *testing.T) {
	hub := newSnapshotHub(t, "org/gguf-only", "main", map[string]string{"config.json": `{}`, "m.gguf": "GGUF"})
	dir := t.TempDir()
	if _, err := Snapshot(context.Background(), "org/gguf-only", dir, Options{Endpoint: hub.URL}); err == nil || !strings.Contains(err.Error(), "no safetensors") {
		t.Errorf("a repo with no engine weights: err = %v", err)
	}
	if _, err := Snapshot(context.Background(), "org/gguf-only", dir, Options{Endpoint: hub.URL, SHA256: "abc"}); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("one digest for a file set: err = %v", err)
	}
	if _, err := Snapshot(context.Background(), "org/gguf-only", dir, Options{Endpoint: hub.URL, Revision: "../../etc"}); err == nil {
		t.Error("a revision that leaves the cache was accepted")
	}
	if _, err := Snapshot(context.Background(), "org/gguf-only", dir, Options{Endpoint: hub.URL, Revision: "nope"}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("an unknown revision: err = %v", err)
	}
}
