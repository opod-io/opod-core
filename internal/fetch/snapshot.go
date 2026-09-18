package fetch

// Snapshot mode: a Hub repo at a revision → the set of files a safetensors
// engine (vLLM, SGLang) loads, present in one directory it can be pointed at.
//
// A GGUF model is one file; a safetensors model is a directory — weight shards,
// their index, the config, the tokenizer, sometimes the repo's own modelling
// code. Left to itself the engine pulls all of it at first start, tens of GB
// inside the launch path where nothing checks the disk or the digest first.
// This makes the same set present BEFORE the engine starts, through the same
// step as a GGUF (`one`): one exclusive lock per file, a temp sibling renamed
// into place, the digest checked, a marker written so the cache can reclaim it.
//
// Layout: <dir>/<repo-slug>@<revision>/ — the directory R15.16 already gives a
// pinned revision, so two revisions of one repo coexist. An unpinned snapshot
// goes to …@main rather than the top of the cache, because a snapshot's file
// names (config.json, tokenizer.json) are the same in every repo.
//
// The directory counts as a snapshot only once its manifest is written, and the
// manifest is written last. SnapshotPath re-checks every file it names, so a
// pruned or half-written directory is never handed to an engine as complete.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SnapshotManifestName is the file that marks a snapshot directory complete.
const SnapshotManifestName = ".opod-snapshot.json"

// SnapshotManifest records what a complete snapshot holds.
type SnapshotManifest struct {
	Repo     string `json:"repo"`
	Revision string `json:"revision"` // what was asked for: a commit sha, tag or branch
	// Commit is the commit the Hub resolved Revision to when the snapshot was
	// taken — the one fact that says which bytes a moving tag or branch meant.
	Commit      string         `json:"commit,omitempty"`
	Files       []SnapshotFile `json:"files"`
	CompletedAt time.Time      `json:"completedAt"`
}

// SnapshotFile is one file of a snapshot.
type SnapshotFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// SHA256 is the digest the Hub declares for the file and the download was
	// checked against. Only LFS-tracked files (every weight shard) have one;
	// small text files are size-checked only.
	SHA256 string `json:"sha256,omitempty"`
}

// snapshotRevision is the revision a snapshot is taken at and cached under.
func snapshotRevision(revision string) string {
	if revision = strings.TrimSpace(revision); revision == "" {
		return "main"
	}
	return revision
}

// SnapshotDir is where the snapshot of repo at revision lives under dir.
func SnapshotDir(dir, repo, revision string) string {
	return RevisionDir(dir, repo, snapshotRevision(revision))
}

// HFRevisionInfoURL is the Hub's description of a repo at a revision, with each
// file's size and (for LFS files) sha256 — one call, no pagination.
func HFRevisionInfoURL(endpoint, repo, revision string) string {
	if endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/"); endpoint == "" {
		endpoint = DefaultHFEndpoint
	}
	return fmt.Sprintf("%s/api/models/%s/revision/%s?blobs=true", endpoint, repo, url.PathEscape(snapshotRevision(revision)))
}

// Snapshot makes the engine's file set of repo@revision present under dir and
// returns the snapshot directory. Files already complete are skipped, so a
// second call on a warm node costs one listing request and no download.
func Snapshot(ctx context.Context, repo, dir string, opt Options) (string, error) {
	if repo == "" {
		return "", errors.New("fetch: repo required")
	}
	if dir == "" {
		return "", errors.New("fetch: models directory required")
	}
	if opt.SHA256 != "" {
		// One digest cannot describe a set of files. A snapshot is pinned by its
		// revision — a commit sha fixes every file — and each weight file is
		// checked against the sha256 the Hub records for it.
		return "", errors.New("fetch: a snapshot takes no --sha256: pin the revision to a commit sha; each weight file is verified against the digest the Hub records for it")
	}
	if err := ValidRevision(opt.Revision); err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	opt = opt.withDefaults()
	opt.Revision = snapshotRevision(opt.Revision)

	commit, listed, err := listRevision(ctx, repo, opt)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	files := selectSnapshotFiles(listed)
	if !hasWeights(files) {
		return "", fmt.Errorf("fetch: %s@%s has no safetensors or PyTorch weights at its top level — nothing an engine could load", repo, opt.Revision)
	}
	sdir := SnapshotDir(dir, repo, opt.Revision)
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		return "", fmt.Errorf("fetch: mkdir %s: %w", sdir, err)
	}
	for _, f := range files {
		fopt := opt
		fopt.SHA256 = f.SHA256
		if _, err := one(ctx, repo, f.Name, sdir, f.Size, fopt); err != nil {
			return "", fmt.Errorf("fetch: snapshot %s@%s: %s: %w", repo, opt.Revision, f.Name, err)
		}
	}
	m := SnapshotManifest{Repo: repo, Revision: opt.Revision, Commit: commit, Files: files, CompletedAt: time.Now().UTC()}
	if err := writeManifest(sdir, m); err != nil {
		return "", fmt.Errorf("fetch: snapshot manifest: %w", err)
	}
	return sdir, nil
}

// SnapshotPath returns the snapshot directory of repo@revision when it is
// complete: the manifest is there and every file it names is present at its
// recorded size. Anything less — never fetched, still being fetched, a file
// reclaimed since — is "no", and the caller falls back to the engine's own pull.
func SnapshotPath(dir, repo, revision string) (string, bool) {
	if dir == "" || repo == "" || ValidRevision(revision) != nil {
		return "", false
	}
	sdir := SnapshotDir(dir, repo, revision)
	b, err := os.ReadFile(filepath.Join(sdir, SnapshotManifestName))
	if err != nil {
		return "", false
	}
	var m SnapshotManifest
	if json.Unmarshal(b, &m) != nil || m.Repo != repo || len(m.Files) == 0 {
		return "", false
	}
	for _, f := range m.Files {
		if ValidGGUFName(f.Name) != nil || !complete(filepath.Join(sdir, f.Name), f.Size, f.Size > 0) {
			return "", false
		}
	}
	for _, f := range m.Files {
		Touch(filepath.Join(sdir, f.Name)) // in use: not the least recently used
	}
	return sdir, true
}

// writeManifest is atomic like every other write here: a reader sees the old
// manifest, none, or the new one — never half of it.
func writeManifest(sdir string, m SnapshotManifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(sdir, SnapshotManifestName+".partial-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(append(b, '\n'))
	merr := f.Chmod(0o644) // CreateTemp is 0600; the worker that reads this may be another user
	cerr := f.Close()
	if werr != nil || merr != nil || cerr != nil {
		_ = os.Remove(tmp)
		return errors.Join(werr, merr, cerr)
	}
	if err := os.Rename(tmp, filepath.Join(sdir, SnapshotManifestName)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// listRevision asks the Hub what repo holds at the revision.
func listRevision(ctx context.Context, repo string, opt Options) (commit string, files []SnapshotFile, err error) {
	infoURL := HFRevisionInfoURL(opt.Endpoint, repo, opt.Revision)
	lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(lctx, http.MethodGet, infoURL, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("User-Agent", "opod-fetch")
	if opt.Token != "" {
		req.Header.Set("Authorization", "Bearer "+opt.Token)
	}
	resp, err := opt.HTTP.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("GET %s: %w", infoURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("GET %s → %s (a gated or private repo needs HF_TOKEN; an unknown revision is a 404)", infoURL, resp.Status)
	}
	var info struct {
		SHA      string `json:"sha"`
		Siblings []struct {
			Name string `json:"rfilename"`
			Size int64  `json:"size"`
			LFS  *struct {
				SHA256 string `json:"sha256"`
				Size   int64  `json:"size"`
			} `json:"lfs"`
		} `json:"siblings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&info); err != nil {
		return "", nil, fmt.Errorf("decode %s: %w", infoURL, err)
	}
	for _, s := range info.Siblings {
		f := SnapshotFile{Name: s.Name, Size: s.Size}
		if s.LFS != nil {
			f.SHA256 = s.LFS.SHA256
			if s.LFS.Size > 0 {
				f.Size = s.LFS.Size
			}
		}
		files = append(files, f)
	}
	return info.SHA, files, nil
}

// Weight formats, in the order an engine prefers them. Only the first one
// present is fetched: a repo that publishes both safetensors and .bin holds the
// same tensors twice, and the second copy is tens of GB nobody reads.
var weightSuffixes = [][]string{{".safetensors"}, {".bin", ".pt", ".pth"}}

// snapshotSidecars are the non-weight files an engine reads from a model
// directory: configs, tokenizer files in their several formats, chat
// templates, and the modelling code a trust-remote-code repo ships.
var snapshotSidecars = map[string]bool{
	".json": true, ".txt": true, ".model": true, ".tiktoken": true, ".jinja": true, ".py": true,
}

// notForInference are PyTorch-format files a training run leaves in a repo.
var notForInference = map[string]bool{
	"training_args.bin": true, "optimizer.bin": true, "optimizer.pt": true,
	"scheduler.pt": true, "scaler.pt": true, "rng_state.pth": true,
}

// selectSnapshotFiles picks what an engine loads out of what the repo holds:
//   - top-level files only. Engines glob the model directory itself, and what
//     repos keep in subdirectories (`original/`, `onnx/`, `metal/`, `gguf/`) is
//     the same model again in another runtime's format;
//   - one weight format — safetensors when there is any, PyTorch otherwise;
//   - `consolidated*.safetensors` is dropped when `model*.safetensors` exists:
//     it is the same tensors in one file, published beside the sharded copy;
//   - the sidecar files by extension; everything else (READMEs, images,
//     .gitattributes, other runtimes' weights, empty files) stays on the Hub.
func selectSnapshotFiles(listed []SnapshotFile) []SnapshotFile {
	var top []SnapshotFile
	for _, f := range listed {
		// Size 0 is an empty file or a listing that did not say: neither can be
		// checked for completeness, and no engine needs an empty file.
		if f.Size <= 0 || ValidGGUFName(f.Name) != nil || strings.HasPrefix(f.Name, ".") {
			continue
		}
		top = append(top, f)
	}
	var format []string
	for _, suffixes := range weightSuffixes {
		if anyFile(top, func(n string) bool { return hasSuffix(n, suffixes) && !notForInference[n] }) {
			format = suffixes
			break
		}
	}
	sharded := anyFile(top, func(n string) bool { return strings.HasPrefix(n, "model") && strings.HasSuffix(n, ".safetensors") })
	var out []SnapshotFile
	for _, f := range top {
		switch {
		case hasSuffix(f.Name, format) && !notForInference[f.Name]:
			if sharded && strings.HasPrefix(f.Name, "consolidated") {
				continue
			}
			out = append(out, f)
		case snapshotSidecars[path.Ext(f.Name)]:
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func hasWeights(files []SnapshotFile) bool {
	for _, suffixes := range weightSuffixes {
		if anyFile(files, func(n string) bool { return hasSuffix(n, suffixes) }) {
			return true
		}
	}
	return false
}

func anyFile(files []SnapshotFile, match func(name string) bool) bool {
	for _, f := range files {
		if match(f.Name) {
			return true
		}
	}
	return false
}

func hasSuffix(name string, suffixes []string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}
