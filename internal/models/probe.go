// Source reachability probes — lightweight HEAD checks that a catalog
// entry's upstream actually exists. Shared by `opod model add` (pre-
// flight, so a typo'd hf:owner/repo fails at add time instead of at
// llama-server launch), the admin POST /admin/v1/models endpoint, and
// the nightly catalog live check.
package models

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// ProbeVerdict classifies a source probe.
type ProbeVerdict int

const (
	// ProbeOK — upstream confirmed to exist.
	ProbeOK ProbeVerdict = iota
	// ProbeNotFound — upstream definitively missing (404 / missing local
	// file). Callers should refuse the install: the failure is certain
	// and the later error would be worse (deferred to engine launch).
	ProbeNotFound
	// ProbeIndeterminate — couldn't verify (network error, timeout,
	// 5xx). Callers should warn and proceed: a flaky network must not
	// block an install that may well succeed.
	ProbeIndeterminate
	// ProbeSkipped — source type has nothing to probe.
	ProbeSkipped
)

// ProbeTimeout bounds a single pre-flight HEAD. Short on purpose — an
// offline machine should degrade to a warning in seconds, not hang.
const ProbeTimeout = 5 * time.Second

// Registry bases are package vars so tests can point them at an
// httptest server. Production code never changes them.
var (
	ollamaRegistryBase = "https://registry.ollama.ai"
	huggingFaceBase    = "https://huggingface.co"
)

// ProbeSource HEAD-checks the entry's upstream. client may be nil (a
// default with ProbeTimeout is used). The string is a human-readable
// reason for any verdict other than ProbeOK.
//
// Set OPOD_SKIP_SOURCE_CHECK=1 to bypass entirely (air-gapped mirrors,
// custom registries) — returns ProbeSkipped.
func ProbeSource(ctx context.Context, client *http.Client, e *Entry) (ProbeVerdict, string) {
	if os.Getenv("OPOD_SKIP_SOURCE_CHECK") == "1" {
		return ProbeSkipped, "OPOD_SKIP_SOURCE_CHECK=1"
	}
	if client == nil {
		client = &http.Client{Timeout: ProbeTimeout}
	}
	switch e.Source.Type {
	case "ollama":
		return probeOllama(ctx, client, e.Source.OllamaName)
	case "huggingface":
		return probeHuggingFace(ctx, client, e.Source.Repo, e.Source.File)
	case "file":
		if e.Source.Path == "" {
			return ProbeNotFound, "source.type=file requires source.path"
		}
		if _, err := os.Stat(e.Source.Path); err != nil {
			return ProbeNotFound, fmt.Sprintf("local file %s does not exist", e.Source.Path)
		}
		return ProbeOK, ""
	default:
		return ProbeSkipped, fmt.Sprintf("source.type=%q has no probe", e.Source.Type)
	}
}

// probeOllama checks that an Ollama tag exists in the public registry.
// Tag format is "name:tag" or just "name" (which means name:latest).
func probeOllama(ctx context.Context, client *http.Client, fullName string) (ProbeVerdict, string) {
	if fullName == "" {
		return ProbeNotFound, "empty ollama_name"
	}
	name, tag, found := strings.Cut(fullName, ":")
	if !found {
		tag = "latest"
	}
	// Library namespace for unscoped names; scoped names look like "owner/name".
	registryPath := name
	if !strings.Contains(name, "/") {
		registryPath = "library/" + name
	}
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", ollamaRegistryBase, registryPath, tag)
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	// Ollama registry needs an Accept header for the manifest media type.
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	resp, err := client.Do(req)
	if err != nil {
		return ProbeIndeterminate, fmt.Sprintf("HEAD %s: %v", url, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return ProbeOK, ""
	case http.StatusNotFound:
		return ProbeNotFound, fmt.Sprintf("%s not found in the Ollama registry (HEAD %s → 404)", fullName, url)
	default:
		return ProbeIndeterminate, fmt.Sprintf("HEAD %s → %s", url, resp.Status)
	}
}

// probeHuggingFace checks that an HF repo (and optional specific file)
// exists. Gated repos (401/403 — license-acceptance walls) count as OK:
// the purpose is catching typos and removed repos, not auth.
func probeHuggingFace(ctx context.Context, client *http.Client, repo, file string) (ProbeVerdict, string) {
	if repo == "" {
		return ProbeNotFound, "empty source.repo"
	}
	target := file
	if target == "" {
		target = "README.md"
	}
	url := fmt.Sprintf("%s/%s/resolve/main/%s", huggingFaceBase, repo, target)
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return ProbeIndeterminate, fmt.Sprintf("HEAD %s: %v", url, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusFound:
		return ProbeOK, ""
	case http.StatusUnauthorized, http.StatusForbidden:
		// Repo exists but is gated (requires accepting a license or HF
		// login) — exists, so the install path can deal with auth.
		return ProbeOK, ""
	case http.StatusNotFound:
		return ProbeNotFound, fmt.Sprintf("huggingface.co/%s does not exist (HEAD %s → 404) — check the repo name", repo, url)
	default:
		return ProbeIndeterminate, fmt.Sprintf("HEAD %s → %s", url, resp.Status)
	}
}
