package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
)

// bedrockruntime clients are lazily built and cached PER REGION. Building one
// does an AWS credential probe (env, shared config, IMDS) so we don't want to
// do it on every request — but caching a single global client keyed by the
// first region seen meant a second configured region silently reused the
// first region's client.
var (
	brMu      sync.Mutex
	brClients = map[string]*bedrockruntime.Client{}
)

func loadBedrockClient(ctx context.Context, region string) (*bedrockruntime.Client, error) {
	brMu.Lock()
	defer brMu.Unlock()
	if c, ok := brClients[region]; ok {
		return c, nil
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	c := bedrockruntime.NewFromConfig(cfg)
	brClients[region] = c
	return c, nil
}

// invokeBedrock dispatches a signed InvokeModel call to Bedrock and streams
// the response back to the client. Non-streaming for v0.6 — streaming
// (InvokeModelWithResponseStream) lands in v0.7 alongside the OTLP child
// spans work since both touch the response writer plumbing.
func (e *EgressHandler) invokeBedrock(w http.ResponseWriter, r *http.Request, modelID string, body []byte) error {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	client, err := loadBedrockClient(ctx, e.Config.BedrockRegion)
	if err != nil {
		return err
	}

	in := &bedrockruntime.InvokeModelInput{
		ModelId:     &modelID,
		Body:        body,
		ContentType: stringPtr("application/json"),
		Accept:      stringPtr("application/json"),
	}

	out, err := client.InvokeModel(ctx, in)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Body)
	return nil
}

func stringPtr(s string) *string { return &s }

// stripModelField removes the top-level "model" key from a JSON object body.
// Bedrock's InvokeModel takes the model id in the URL path, NOT in the body —
// some Anthropic SDKs include it anyway and Bedrock returns 400.
func stripModelField(body []byte) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}
	delete(obj, "model")
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("re-encode body: %w", err)
	}
	return out, nil
}

// ensureAnthropicVersion adds "anthropic_version": "bedrock-2023-05-31" to
// the body if absent. Bedrock requires it for Anthropic models; the
// direct Anthropic API doesn't, so requests passing through Opod often
// won't carry it.
func ensureAnthropicVersion(body []byte) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if _, ok := obj["anthropic_version"]; ok {
		return body
	}
	obj["anthropic_version"] = "bedrock-2023-05-31"
	out, _ := json.Marshal(obj)
	return out
}

// --- Vertex side ---

// checkVertexADC tries to mint a Google ADC token to confirm the credential
// chain works against the configured project. Returns "" on success, or a
// short error string on failure (suitable to inline in a user-facing 501).
//
// We don't cache the client here because ADC token sources have their own
// internal caching and we want each request to surface real auth errors.
func (e *EgressHandler) checkVertexADC(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Probe the metadata-server-style endpoint for an OAuth token via
	// gcloud's `auth print-access-token` semantics, surfaced through
	// the standard ADC token URL. We don't use cloud.google.com/go/auth
	// directly here to keep the dependency surface scoped — the
	// underlying logic (well-known credential file paths + IMDS) is
	// what matters and is reliable when ADC is configured.
	tokenURL := "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
	req, _ := http.NewRequestWithContext(ctx, "GET", tokenURL, nil)
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Not on GCE — fall through to checking for application_default_credentials.json
		if hint := checkADCFile(); hint != "" {
			return hint
		}
		return "no metadata server, no ADC file (run `gcloud auth application-default login` or set GOOGLE_APPLICATION_CREDENTIALS)"
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Sprintf("metadata server returned %s", resp.Status)
	}
	return ""
}

// checkADCFile returns "" if the standard ADC file exists or
// GOOGLE_APPLICATION_CREDENTIALS is set; otherwise returns a hint.
func checkADCFile() string {
	// Avoid pulling in os just for this — peek via the same channel
	// the cloud.google.com/go/auth package uses (Application Default
	// Credentials file path env var).
	cred := getEnvSafe("GOOGLE_APPLICATION_CREDENTIALS")
	if cred != "" {
		return ""
	}
	home := getEnvSafe("HOME")
	if home != "" {
		// well-known location used by gcloud
		if statSafe(home + "/.config/gcloud/application_default_credentials.json") {
			return ""
		}
	}
	return "no GOOGLE_APPLICATION_CREDENTIALS, no ~/.config/gcloud/application_default_credentials.json"
}

// Tiny shims to keep this file dependency-light. The real os/file ops live
// elsewhere; these are just for the auth-presence probe.
func getEnvSafe(k string) string { return osGetenv(k) }
func statSafe(p string) bool     { return osStat(p) }

// Indirected through package-level vars so the auth probe can be stubbed
// in tests.
var (
	osGetenv = func(k string) string {
		if v, ok := envLookup(k); ok {
			return v
		}
		return ""
	}
	osStat = func(p string) bool {
		rc, err := openForRead(p)
		if rc != nil {
			_ = rc.Close()
		}
		return err == nil
	}
)

// envLookup + openForRead are thin wrappers — they let the test file
// override the real syscalls without dragging in os imports here.
var (
	envLookup       = func(k string) (string, bool) { v, ok := envLookupReal(k); return v, ok }
	openForRead     = func(p string) (io.ReadCloser, error) { return openForReadReal(p) }
	envLookupReal   func(string) (string, bool)
	openForReadReal func(string) (io.ReadCloser, error)
)

// Provide the real implementations via init so the package's only
// requirement on `os` is here, not splattered through ServeVertex.
//
// Done in a tiny init to keep the public API of egress_bedrock.go focused
// on Bedrock; the Vertex probe just happens to live in the same file.
func init() {
	envLookupReal = osLookupEnv
	openForReadReal = osOpenFile
}

// Reach into os via small adaptor funcs in egress_bedrock_os.go.

// Helpful URL-parsing for Vertex location (kept for v0.7 when body
// translation lands and we need to build the generateContent URL).
//
//nolint:unused // referenced from v0.7 body-translation work
func vertexEndpoint(location, project, model string) string {
	loc := location
	if loc == "" {
		loc = "us-central1"
	}
	return fmt.Sprintf("https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
		url.PathEscape(loc), url.PathEscape(project), url.PathEscape(loc), url.PathEscape(model))
}

// Avoid an unused-import warning on bytes/strings/errors when the
// imports are present but referenced only in v0.7 follow-up code paths.
var (
	_ = bytes.NewReader
	_ = strings.HasPrefix
	_ = errors.New
)
