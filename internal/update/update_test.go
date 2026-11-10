package update

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// T24.80: a GitHub refusal is its message, and a rate limit says when to
// retry and how to go around it — never the raw JSON body.
func TestLatestVersionSaysWhatGitHubSaid(t *testing.T) {
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(20*time.Minute).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for 192.0.2.1.","documentation_url":"https://docs.github.com"}`))
	}))
	defer limited.Close()
	_, err := latestFrom(limited.URL)
	if err == nil || !strings.Contains(err.Error(), "rate-limited") || !strings.Contains(err.Error(), "try again in 20m") || !strings.Contains(err.Error(), "--version") || strings.Contains(err.Error(), "documentation_url") {
		t.Fatalf("rate limit: %v", err)
	}
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer gone.Close()
	if _, err := latestFrom(gone.URL); err == nil || err.Error() != "GitHub answered 404 Not Found: Not Found" {
		t.Fatalf("404: %v", err)
	}
}

func TestVersionOrder(t *testing.T) {
	v := func(s string) Version {
		p, ok := ParseVersion(s)
		if !ok {
			t.Fatalf("ParseVersion(%q) failed", s)
		}
		return p
	}
	if Compare(v("v0.4.5"), v("0.4.5")) != 0 || Compare(v("0.4.10"), v("0.4.9")) != 1 || Compare(v("0.4.5-rc1"), v("0.4.5")) != -1 || Compare(v("1.0.0-b"), v("1.0.0-a")) != 1 {
		t.Fatal("semver order")
	}
	if _, ok := ParseVersion("dev-1a2b3c"); ok {
		t.Fatal("a commit name is not a version")
	}
}
