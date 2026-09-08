package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

type sleepyEngine struct {
	engines.Engine
	asleep bool
}

func (e *sleepyEngine) Name() string                           { return "vllm" }
func (e *sleepyEngine) Sleep(context.Context) error            { e.asleep = true; return nil }
func (e *sleepyEngine) Resume(context.Context) error           { e.asleep = false; return nil }
func (e *sleepyEngine) Sleeping(context.Context) (bool, error) { return e.asleep, nil }

type plainEngine struct{ engines.Engine }

func (plainEngine) Name() string { return "llamacpp" }

// Build item 13, worker side: /v1/model/sleep|resume drive an engine's
// sleep mode; an engine without one answers 501 "unsupported" so the
// caller parks the pod instead of believing a fake sleep.
func TestSleepRoutes(t *testing.T) {
	const token = "sk-test"
	eng := &sleepyEngine{}
	srv := &Server{Token: token, Engine: eng}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/model/sleep", srv.auth(srv.modelSleep))
	mux.HandleFunc("/v1/model/resume", srv.auth(srv.modelResume))
	ts := httptest.NewServer(mux)
	defer ts.Close()
	call := func(path string) (int, string) {
		r, _ := http.NewRequest(http.MethodPost, ts.URL+path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b := make([]byte, 256)
		n, _ := resp.Body.Read(b)
		return resp.StatusCode, string(b[:n])
	}
	if code, body := call("/v1/model/sleep"); code != 200 || !strings.Contains(body, "sleeping") || !eng.asleep {
		t.Fatalf("sleep: %d %s asleep=%v", code, body, eng.asleep)
	}
	if code, body := call("/v1/model/resume"); code != 200 || !strings.Contains(body, "resumed") || eng.asleep {
		t.Fatalf("resume: %d %s asleep=%v", code, body, eng.asleep)
	}
	srv.Engine = plainEngine{}
	if code, body := call("/v1/model/sleep"); code != http.StatusNotImplemented || !strings.Contains(body, "unsupported") {
		t.Fatalf("no sleep mode must be 501 unsupported: %d %s", code, body)
	}
}
