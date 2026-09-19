package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

func capturedPost(t *testing.T, send func(a *Agent) error, eng engines.Engine, al *Aliases, caps Capabilities) map[string]json.RawMessage {
	t.Helper()
	var body map[string]json.RawMessage
	leader := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}))
	defer leader.Close()
	a := &Agent{NodeID: "n1", LeaderURL: leader.URL, Token: "sk", Aliases: al, Capabilities: caps,
		HTTP: leader.Client(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if eng != nil {
		a.Engine = eng
	}
	if err := send(a); err != nil {
		t.Fatal(err)
	}
	return body
}

// An engine that keeps installed models and loads on request reports both
// lists: loaded_models is still everything it answers for, resident_models is
// what is in memory now — sent even when empty. An engine that cannot tell
// the two apart sends no resident_models at all.
func TestHeartbeatCarriesResidencyBesideWhatIsServed(t *testing.T) {
	heartbeat := func(a *Agent) error { _, err := a.Heartbeat(context.Background()); return err }
	al := &Aliases{}
	al.Note("llama3.2:3b", "llama-3.2-3b")
	eng := &daemonStub{installed: []string{"llama3.2:3b", "qwen3:8b"}, resident: []string{"llama3.2:3b"}}

	body := capturedPost(t, heartbeat, eng, al, Capabilities{})
	var loaded, resident []string
	_ = json.Unmarshal(body["loaded_models"], &loaded)
	_ = json.Unmarshal(body["resident_models"], &resident)
	if !reflect.DeepEqual(loaded, []string{"llama-3.2-3b", "qwen3:8b"}) {
		t.Errorf("loaded_models is unchanged — everything the engine answers for, by id: %v", loaded)
	}
	if !reflect.DeepEqual(resident, []string{"llama-3.2-3b"}) {
		t.Errorf("resident_models is what is in memory, by id: %v", resident)
	}

	eng.resident = nil
	body = capturedPost(t, heartbeat, eng, al, Capabilities{})
	if string(body["resident_models"]) != "[]" {
		t.Errorf("nothing resident is an empty list, not an absent key: %s", body["resident_models"])
	}

	body = capturedPost(t, heartbeat, &externalStub{models: []string{"qwen3-8b"}}, al, Capabilities{})
	if _, present := body["resident_models"]; present {
		t.Errorf("an engine that cannot tell installed from resident must omit the key: %s", body["resident_models"])
	}
}

// The engine id travels inside hardware_json, after the fields that were
// already there, and is omitted when not stated — a node row written by an
// older worker decodes to the same document as before.
func TestRegisterCarriesTheEngine(t *testing.T) {
	register := func(a *Agent) error { return a.Register(context.Background()) }
	body := capturedPost(t, register, nil, nil, Capabilities{Hostname: "node-a", OS: "linux", Arch: "amd64", CPUCores: 8, RAMGB: 64,
		Role: "decode", PlanRevision: 7, Engine: "vllm"})
	var hw string
	if err := json.Unmarshal(body["hardware_json"], &hw); err != nil {
		t.Fatal(err)
	}
	const want = `{"Hostname":"node-a","OS":"linux","Arch":"amd64","CPUCores":8,"RAMGB":64,"GPUs":null,"Role":"decode","PlanRevision":7,"Engine":"vllm"}`
	if hw != want {
		t.Errorf("hardware_json\n got %s\nwant %s", hw, want)
	}
	body = capturedPost(t, register, nil, nil, Capabilities{Hostname: "node-a", OS: "linux", Arch: "amd64", CPUCores: 8, RAMGB: 64})
	_ = json.Unmarshal(body["hardware_json"], &hw)
	if hw != `{"Hostname":"node-a","OS":"linux","Arch":"amd64","CPUCores":8,"RAMGB":64,"GPUs":null}` {
		t.Errorf("no engine stated must leave the document as it was: %s", hw)
	}
}
