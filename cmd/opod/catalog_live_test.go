package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/models"
)

// TestCatalogSourcesReachable does a lightweight HEAD against the upstream
// for every catalog entry to confirm its `source:` actually exists. Catches
// typos (wrong Ollama tag, renamed HF repo) before users hit a 404 at
// `opod model add`.
//
// Gated behind CATALOG_LIVE_CHECK=1 so the default test suite stays fast
// and offline. Run as:
//
//	CATALOG_LIVE_CHECK=1 go test -run TestCatalogSourcesReachable ./cmd/opod/
//
// CI invokes this on a schedule (separate job) rather than every commit so
// upstream flakiness doesn't block merges.
func TestCatalogSourcesReachable(t *testing.T) {
	if os.Getenv("CATALOG_LIVE_CHECK") != "1" {
		t.Skip("set CATALOG_LIVE_CHECK=1 to run upstream HEAD probes")
	}

	repoRoot := findRepoRoot(t)
	cat, err := models.LoadCatalog(filepath.Join(repoRoot, "catalog"))
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	type result struct {
		id      string
		ok      bool
		reason  string
		skipped string
	}
	results := make(chan result, len(cat))

	// Limit concurrency — be a polite citizen against Ollama + HF.
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup

	for _, e := range cat {
		e := e
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			r := result{id: e.ID}
			// "file" sources are local-only — skip rather than probe so
			// CI (which has no model files) doesn't fail them.
			if e.Source.Type == "file" {
				r.skipped = "source.type=file (local-only; can't verify without the file)"
				results <- r
				return
			}
			verdict, reason := models.ProbeSource(ctx, client, &e)
			switch verdict {
			case models.ProbeOK:
				r.ok = true
			case models.ProbeSkipped:
				r.skipped = reason
			default:
				// The nightly probe treats "couldn't verify" the same as
				// "missing" — its whole job is to make a human look.
				r.reason = reason
			}
			results <- r
		}()
	}
	go func() { wg.Wait(); close(results) }()

	var failed, skipped int
	for r := range results {
		switch {
		case r.skipped != "":
			t.Logf("SKIP %-30s — %s", r.id, r.skipped)
			skipped++
		case !r.ok:
			t.Errorf("UNREACHABLE %-30s — %s", r.id, r.reason)
			failed++
		default:
			t.Logf("OK    %-30s", r.id)
		}
	}
	t.Logf("catalog live check: %d entries, %d skipped, %d failed", len(cat), skipped, failed)
}

// The HEAD-probe implementations live in internal/models/probe.go —
// shared with `opod model add`'s pre-flight check and the admin
// install endpoint, so a source this test would flag is also refused
// at install time.
