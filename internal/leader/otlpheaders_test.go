package leader

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

// tokenCollector is a collector behind a path prefix that answers only a
// request carrying its token (PLAN T21.18): what a leader could not reach
// while it sent no header and dropped the URL's path.
type tokenCollector struct {
	*httptest.Server
	mu    sync.Mutex
	got   map[string]int // path → accepted exports
	extra []string       // X-Scope values seen on accepted exports
}

func newTokenCollector(t *testing.T, token string) *tokenCollector {
	t.Helper()
	c := &tokenCollector{got: map[string]int{}}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		var resp proto.Message
		switch r.URL.Path {
		case "/otel/v1/traces":
			resp = &coltracepb.ExportTraceServiceResponse{}
		case "/otel/v1/logs":
			resp = &collogspb.ExportLogsServiceResponse{}
		default:
			http.NotFound(w, r)
			return
		}
		c.mu.Lock()
		c.got[r.URL.Path]++
		c.extra = append(c.extra, r.Header.Get("X-Scope"))
		c.mu.Unlock()
		out, _ := proto.Marshal(resp)
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(out)
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *tokenCollector) accepted(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got[path]
}

const otlpTestHeaders = "Authorization=Bearer s3cr=t, X-Scope=tenant-a"

func TestTracesReachATokenCollectorBehindAPathPrefix(t *testing.T) {
	c := newTokenCollector(t, "s3cr=t")
	shutdown, err := initTracing(context.Background(), c.URL+"/otel/v1/traces", otlpTestHeaders, "", "dev", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.GetTracerProvider().Tracer("t").Start(context.Background(), "root")
	span.End()
	if err := shutdown(context.Background()); err != nil { // shutdown flushes the batch
		t.Fatalf("shutdown: %v — the collector refused the export", err)
	}
	if n := c.accepted("/otel/v1/traces"); n == 0 {
		t.Fatal("no span reached /otel/v1/traces with the token: the path or the header was dropped")
	}
	if c.extra[0] != "tenant-a" {
		t.Errorf("X-Scope = %q, want every header of OPOD_OTLP_HEADERS sent", c.extra[0])
	}
}

func TestLogsReachATokenCollectorBehindAPathPrefix(t *testing.T) {
	c := newTokenCollector(t, "s3cr=t")
	base := slog.New(slog.NewTextHandler(io.Discard, nil))
	log, shutdown, err := ExportLogs(context.Background(), c.URL+"/otel", otlpTestHeaders, "dev", base) // a prefix: /v1/logs is added after it
	if err != nil {
		t.Fatal(err)
	}
	log.Warn("worker went quiet")
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v — the collector refused the export", err)
	}
	if n := c.accepted("/otel/v1/logs"); n == 0 {
		t.Fatal("no log record reached /otel/v1/logs with the token: the path or the header was dropped")
	}
}

// The path rule is the control plane's own OTLP exporters' (adapters.OTLPURL):
// the signal's path is added after the URL's unless it already ends in it.
func TestOTLPTargetKeepsThePathAndReadsTheHeaders(t *testing.T) {
	for _, tc := range []struct {
		endpoint, signal               string
		wantHost, wantScheme, wantPath string
	}{
		{"http://otel-collector:4318", "/v1/traces", "otel-collector:4318", "http", "/v1/traces"},
		{"http://otel-collector:4318/", "/v1/logs", "otel-collector:4318", "http", "/v1/logs"},
		{"otel-collector:4318", "/v1/logs", "otel-collector:4318", "http", "/v1/logs"},
		{"https://gw.example.com/otel/v1/traces", "/v1/traces", "gw.example.com", "https", "/otel/v1/traces"},
		{"https://gw.example.com/otel", "/v1/logs", "gw.example.com", "https", "/otel/v1/logs"},
		{"https://gw.example.com/otel/", "/v1/traces", "gw.example.com", "https", "/otel/v1/traces"},
		{"otel-collector:4318/prefix/v1/logs", "/v1/logs", "otel-collector:4318", "http", "/prefix/v1/logs"},
		{"otel-collector:4318/prefix", "/v1/logs", "otel-collector:4318", "http", "/prefix/v1/logs"},
	} {
		got, err := otlpTarget(tc.endpoint, "", tc.signal)
		if err != nil {
			t.Errorf("%s: %v", tc.endpoint, err)
			continue
		}
		if got.host != tc.wantHost || got.scheme != tc.wantScheme || got.path != tc.wantPath {
			t.Errorf("%s → host %q scheme %q path %q, want %q %q %q", tc.endpoint, got.host, got.scheme, got.path, tc.wantHost, tc.wantScheme, tc.wantPath)
		}
	}
	got, err := otlpTarget("http://c:4318", otlpTestHeaders, "/v1/traces")
	if err != nil {
		t.Fatal(err)
	}
	if got.headers["Authorization"] != "Bearer s3cr=t" || got.headers["X-Scope"] != "tenant-a" {
		t.Errorf("headers = %v, want both, a value keeping its '='", got.headers)
	}
	if names := strings.Join(got.headerNames(), ","); names != "Authorization,X-Scope" {
		t.Errorf("headerNames = %q: the enabled line logs names, sorted, never values", names)
	}
	for _, bad := range []string{"ftp://c:4318", "http://", "http://bad host:4318"} {
		if _, err := otlpTarget(bad, "", "/v1/traces"); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// A malformed OPOD_OTLP_HEADERS, or an endpoint that is no URL, never stops
// the leader — telemetry is not on its critical path — and is named where it
// was made: tracing turns off with a warning, log export says why it is off.
func TestAnUnusableOTLPSettingTurnsExportOffAndSaysWhy(t *testing.T) {
	for _, tc := range []struct{ endpoint, headers, want string }{
		{"http://c:4318", "Authorization", "OPOD_OTLP_HEADERS"},
		{"ftp://c:4318", "", "ftp://c:4318"},
	} {
		var out bytes.Buffer
		shutdown, err := initTracing(context.Background(), tc.endpoint, tc.headers, "", "dev", slog.New(slog.NewTextHandler(&out, nil)))
		if err != nil {
			t.Errorf("%s / %s: the leader would not start: %v", tc.endpoint, tc.headers, err)
			continue
		}
		_ = shutdown(context.Background())
		if !strings.Contains(out.String(), "tracing disabled") || !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s / %s: want a warning naming %q, got:\n%s", tc.endpoint, tc.headers, tc.want, out.String())
		}
	}
	base := slog.New(slog.NewTextHandler(io.Discard, nil))
	log, _, err := ExportLogs(context.Background(), "http://c:4318", "=v", "dev", base)
	if err == nil || !strings.Contains(err.Error(), "OPOD_OTLP_HEADERS") {
		t.Errorf("logs: err = %v, want one naming OPOD_OTLP_HEADERS (the caller warns with it)", err)
	}
	if log != base {
		t.Error("a refused log export must hand back the local logger unchanged")
	}
}
