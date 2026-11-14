package nodeapi

// The golden files under testdata/ were NOT produced from these types. Each
// one reproduces a body exactly as the opod binary built it before the types
// existed — from map literals and untagged structs — so a test here fails
// when a typed value would put different JSON on the wire than the binary
// that is already deployed: a renamed key, a key that used to be omitted and
// now travels as a zero, a number that became a string. There is
// deliberately no -update flag: a golden changes only when the wire does.
//
// Comparison is canonical (keys sorted at every depth). Key order is not
// part of this wire — the protocol's HMAC covers method, path and timestamp,
// never the body.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var (
	capsFull = Capabilities{
		Hostname: "node-a", OS: "linux", Arch: "amd64", CPUCores: 32, RAMGB: 128,
		GPUs: []GPU{{Name: "NVIDIA A100-SXM4-80GB", VRAMGB: 79}, {Name: "NVIDIA A100-SXM4-80GB", VRAMGB: 79}},
		Role: RoleDecode, PlanRevision: 7, Engine: "vllm",
	}
	capsMinimal = Capabilities{Hostname: "node-b", OS: "linux", Arch: "amd64", CPUCores: 8, RAMGB: 16}
	startedAt   = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
)

func mustHardware(t *testing.T, c Capabilities) string {
	t.Helper()
	s, err := EncodeHardware(c)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// wireCase is one golden file: the fully populated typed value that must
// marshal to it, and a pointer to a zero value to decode it into.
type wireCase struct {
	golden string
	value  any
	into   any
}

func wireCases(t *testing.T) []wireCase {
	hitTokens, queryTokens := int64(980), int64(1000)
	return []wireCase{
		{"register_request", RegisterRequest{
			ID: "n_worker-0", Hostname: "node-a", OS: "linux", Arch: "amd64", RAMGB: 128,
			Address: "192.0.2.10:8081", HardwareJSON: mustHardware(t, capsFull), BootID: "0123456789abcdef",
		}, &RegisterRequest{}},
		{"register_response", RegisterResponse{Status: StatusRegistered, ID: "n_worker-0"}, &RegisterResponse{}},
		{"capabilities", capsFull, &Capabilities{}},
		{"capabilities_minimal", capsMinimal, &Capabilities{}},
		{"capabilities_kv_peer", Capabilities{Hostname: "node-c", OS: "linux", Arch: "amd64", CPUCores: 16, RAMGB: 64,
			GPUs: []GPU{{Name: "NVIDIA RTX A6000", VRAMGB: 47}}, Engine: "vllm", KVTier: KVTierPeer}, &Capabilities{}},
		{"capabilities_kv_events", Capabilities{Hostname: "node-e", OS: "linux", Arch: "amd64", CPUCores: 16, RAMGB: 64,
			GPUs: []GPU{{Name: "NVIDIA RTX A6000", VRAMGB: 47}}, Engine: "vllm", KVEvents: true}, &Capabilities{}},
		{"capabilities_bind_host", Capabilities{Hostname: "node-d", OS: "linux", Arch: "amd64", CPUCores: 8, RAMGB: 32,
			GPUs: []GPU{{Name: "NVIDIA RTX A6000", VRAMGB: 47}}, Engine: "llamacpp", BindHost: "10.0.0.5"}, &Capabilities{}},
		{"capabilities_given_devices", Capabilities{Hostname: "opod-ep-chat-0", OS: "linux", Arch: "amd64", CPUCores: 32, RAMGB: 128,
			GPUs: []GPU{{Name: "NVIDIA RTX A6000", VRAMGB: 47}}, Engine: "vllm",
			GivenDevices: &[]GivenDevice{{Index: 0, Vendor: VendorNVIDIA, PCI: "0000:41:00.0", UUID: "GPU-6f1c2a3b-0000-4000-8000-000000000001"}}}, &Capabilities{}},
		{"capabilities_given_none", Capabilities{Hostname: "opod-ep-chat-1", OS: "linux", Arch: "amd64", CPUCores: 8, RAMGB: 32,
			GivenDevices: &[]GivenDevice{}}, &Capabilities{}},
		{"heartbeat", Heartbeat{
			ID: "n_worker-0", LoadedModels: []string{"qwen2.5-0.5b-gguf", "qwen2.5-0.5b-gguf:support"},
			BootID: "0123456789abcdef", Sleeping: true, ResidentModels: &[]string{"qwen2.5-0.5b-gguf"},
			Load: &EngineLoad{KVUsedPct: 62.5, QueueDepth: 3, TokensPerSec: 148.25, PrefixHitPct: 41, SampledAt: 1789732800},
		}, &Heartbeat{}},
		{"heartbeat_prefix_counters", Heartbeat{ID: "n_worker-0", BootID: "0123456789abcdef", LoadedModels: []string{"m"},
			Load: &EngineLoad{PrefixHitPct: 98, SampledAt: 1789732800, PrefixHitTokensTotal: &hitTokens, PrefixQueryTokensTotal: &queryTokens}}, &Heartbeat{}},
		{"heartbeat_kv_blocks", Heartbeat{ID: "n_worker-0", BootID: "0123456789abcdef", LoadedModels: []string{"m"},
			KVBlocks: &KVBlocks{Seq: 7, BlockSize: 16, Stored: []string{"0f1e2d3c4b5a6978"}, Removed: []string{"8796a5b4c3d2e1f0"}}}, &Heartbeat{}},
		{"heartbeat_kv_cpu_blocks", Heartbeat{ID: "n_worker-0", BootID: "0123456789abcdef", LoadedModels: []string{"m"},
			KVBlocks: &KVBlocks{Seq: 8, BlockSize: 16, Removed: []string{"0f1e2d3c4b5a6978"}, CPUStored: []string{"0f1e2d3c4b5a6978"}, CPURemoved: []string{"1a2b3c4d5e6f7081"}}}, &Heartbeat{}},
		{"tokenize_request", TokenizeRequest{Model: "m", Messages: []TokenizeMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}}, MaxTokens: 1024}, &TokenizeRequest{}},
		{"tokenize_response", TokenizeResponse{Tokens: []int{1, 2, 3}}, &TokenizeResponse{}},
		{"heartbeat_minimal", Heartbeat{ID: "n_worker-0", BootID: "0123456789abcdef"}, &Heartbeat{}},
		{"heartbeat_response", HeartbeatResponse{Status: StatusOK}, &HeartbeatResponse{}},
		{"load_model_request", LoadModelRequest{
			ID: "llama-3.2-3b-gguf", OllamaName: "llama3.2:3b", Repo: "example/Llama-3.2-3B-Instruct-GGUF",
			File: "Llama-3.2-3B-Instruct-Q4_K_M.gguf", Path: "/data/models/llama-3.2-3b.gguf", Pin: true,
		}, &LoadModelRequest{}},
		{"load_model_request_leader", LoadModelRequest{
			ID: "llama-3.2-3b-gguf", OllamaName: "llama3.2:3b", Repo: "example/Llama-3.2-3B-Instruct-GGUF",
			Path: "/data/models/llama-3.2-3b.gguf", Pin: true,
		}, &LoadModelRequest{}},
		{"load_model_response", LoadModelResponse{Status: StatusReady, Model: "llama-3.2-3b.gguf"}, &LoadModelResponse{}},
		{"unload_model_request", UnloadModelRequest{
			ID: "llama-3.2-3b-gguf", OllamaName: "llama3.2:3b", Repo: "example/Llama-3.2-3B-Instruct-GGUF",
			Path: "/data/models/llama-3.2-3b.gguf",
		}, &UnloadModelRequest{}},
		{"unload_model_response", UnloadModelResponse{Status: StatusUnloaded, Model: "llama-3.2-3b.gguf"}, &UnloadModelResponse{}},
		{"unload_model_response_noop", UnloadModelResponse{Status: StatusNoop, Model: "llama-3.2-3b.gguf", Reason: "not resident in this worker's engine"}, &UnloadModelResponse{}},
		{"unload_model_response_unsupported", UnloadModelResponse{Status: StatusUnsupported, Engine: "vllm", Reason: "the engine cannot unload a model and this worker did not start it; stop the engine, or the worker, instead"}, &UnloadModelResponse{}},
		{"sleep_response", SleepResponse{Status: StatusSleeping, Engine: "vllm"}, &SleepResponse{}},
		{"sleep_response_unsupported", SleepResponse{Status: StatusUnsupported, Engine: "llamacpp", Reason: "engine has no sleep mode; park the pod instead"}, &SleepResponse{}},
		{"load_adapter_request", LoadAdapterRequest{Base: "qwen3-8b", Name: "support", Source: "example/qwen3-8b-support-lora", Rank: 64}, &LoadAdapterRequest{}},
		{"load_adapter_request_minimal", LoadAdapterRequest{Base: "qwen3-8b", Name: "support", Source: "example/qwen3-8b-support-lora"}, &LoadAdapterRequest{}},
		{"unload_adapter_request", UnloadAdapterRequest{Base: "qwen3-8b", Name: "support"}, &UnloadAdapterRequest{}},
		{"adapter_response", AdapterResponse{Status: StatusReady, Model: "qwen3-8b:support"}, &AdapterResponse{}},
		{"held_adapters", []HeldAdapter{{ID: "qwen3-8b:support", Name: "support", Source: "example/qwen3-8b-support-lora"}}, &[]HeldAdapter{}},
		{"adapters_env", []Adapter{{Name: "support", Source: "example/qwen3-8b-support-lora", Rank: 64}, {Name: "legal", Source: "/data/adapters/legal"}}, &[]Adapter{}},
		{"process_spec", ProcessSpec{
			ID: "s-qwen-0", Command: "rpc-server", Args: []string{"-p", "50052"},
			Env: map[string]string{"CUDA_VISIBLE_DEVICES": "0"}, WorkDir: "/data",
			HealthPort: 50052, HealthHost: "0.0.0.0", LogLines: 400, HealthPath: "/health",
			ReadyTimeout: 15 * time.Minute, Restart: true, MaxRestarts: 5, RestartBackoff: time.Second,
		}, &ProcessSpec{}},
		{"process_spec_minimal", ProcessSpec{ID: "s-qwen-0", Command: "rpc-server"}, &ProcessSpec{}},
		{"process_info", ProcessInfo{
			ID: "s-qwen-0", Command: "rpc-server", Args: []string{"-p", "50052"}, PID: 4242, StartedAt: startedAt,
			Status: ProcessFailed, ExitErr: "exit status 1", Address: "192.0.2.10:50052", Restarts: 2,
		}, &ProcessInfo{}},
		{"process_info_minimal", ProcessInfo{
			ID: "s-qwen-0", Command: "rpc-server", Args: []string{"-p", "50052"}, PID: 4242, StartedAt: startedAt,
			Status: ProcessStarting,
		}, &ProcessInfo{}},
		{"stop_process_request", StopProcessRequest{ID: "s-qwen-0"}, &StopProcessRequest{}},
		{"start_process_error", StartProcessError{Error: "ProcessSpec.ID required"}, &StartProcessError{}},
		{"upload_response", UploadResponse{Path: "/data/models/llama-3.2-3b.gguf", SHA256: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", Size: 2019377696}, &UploadResponse{}},
	}
}

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// canonical re-encodes JSON with sorted keys at every depth, numbers kept as
// written.
func canonical(t *testing.T, raw []byte) string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(out) + "\n"
}

// A fully populated value of every type marshals to the body the binary
// wrote before the type existed.
func TestWireMatchesGolden(t *testing.T) {
	for _, c := range wireCases(t) {
		t.Run(c.golden, func(t *testing.T) {
			raw, err := json.Marshal(c.value)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := canonical(t, raw), string(readGolden(t, c.golden)); got != want {
				t.Errorf("wire drift in %s\n--- typed value marshals to\n%s--- the binary writes (testdata/%s.json)\n%s", c.golden, got, c.golden, want)
			}
		})
	}
}

// Every golden decodes into its type and encodes back to itself: no key of
// today's wire is dropped or renamed on the way through.
func TestGoldenRoundTrips(t *testing.T) {
	for _, c := range wireCases(t) {
		t.Run(c.golden, func(t *testing.T) {
			golden := readGolden(t, c.golden)
			if err := json.Unmarshal(golden, c.into); err != nil {
				t.Fatalf("decode: %v", err)
			}
			back, err := json.Marshal(c.into)
			if err != nil {
				t.Fatal(err)
			}
			if got := canonical(t, back); got != string(golden) {
				t.Errorf("%s does not survive a round trip\n--- got\n%s--- want\n%s", c.golden, got, golden)
			}
		})
	}
}

// Forward compatibility: a key this version does not know is ignored, at the
// top level and nested, and changes nothing that was decoded.
func TestUnknownFieldsAreIgnored(t *testing.T) {
	for _, c := range wireCases(t) {
		t.Run(c.golden, func(t *testing.T) {
			golden := readGolden(t, c.golden)
			var doc any
			if err := json.Unmarshal(golden, &doc); err != nil {
				t.Fatal(err)
			}
			extended, err := json.Marshal(withUnknownKeys(doc))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(extended, []byte("x_from_a_newer_version")) {
				t.Fatalf("%s: nothing to extend", c.golden)
			}
			plain := reflect.New(reflect.TypeOf(c.into).Elem()).Interface()
			if err := json.Unmarshal(golden, plain); err != nil {
				t.Fatal(err)
			}
			got := reflect.New(reflect.TypeOf(c.into).Elem()).Interface()
			if err := json.Unmarshal(extended, got); err != nil {
				t.Fatalf("an unknown key must not fail the decode: %v", err)
			}
			if !reflect.DeepEqual(plain, got) {
				t.Errorf("an unknown key changed the decoded value\n got %+v\nwant %+v", got, plain)
			}
		})
	}
}

// withUnknownKeys adds a key to every object in the document — except a
// ProcessSpec's Env, which is a caller's map, not a struct: a key added there
// is an environment variable, not an unknown field.
func withUnknownKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{"x_from_a_newer_version": map[string]any{"n": 1}}
		for k, e := range x {
			if k == "Env" {
				out[k] = e
				continue
			}
			out[k] = withUnknownKeys(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = withUnknownKeys(e)
		}
		return out
	}
	return v
}

// hardware_json is JSON inside a JSON string, and node rows keep the string
// as it arrived — so this one IS compared byte for byte, key order included.
func TestHardwareJSONIsByteExact(t *testing.T) {
	var golden struct {
		HardwareJSON string `json:"hardware_json"`
	}
	if err := json.Unmarshal(readGolden(t, "register_request"), &golden); err != nil {
		t.Fatal(err)
	}
	if got := mustHardware(t, capsFull); got != golden.HardwareJSON {
		t.Errorf("hardware_json\n got %s\nwant %s", got, golden.HardwareJSON)
	}
	back, err := RegisterRequest{HardwareJSON: golden.HardwareJSON}.Hardware()
	if err != nil || !reflect.DeepEqual(back, capsFull) {
		t.Errorf("Hardware() = %+v, %v", back, err)
	}
	if zero, err := DecodeHardware(""); err != nil || !reflect.DeepEqual(zero, Capabilities{}) {
		t.Errorf("an empty hardware_json is the zero value, got %+v, %v", zero, err)
	}
	if _, err := DecodeHardware("{not json"); err == nil {
		t.Error("a malformed hardware_json must be an error, not a silent zero")
	}
}

// What today's writers omit stays omitted, and what they always send stays
// present — a zero that starts travelling is a wire change too.
func TestOmittedVersusZero(t *testing.T) {
	has := func(v any, key string) bool {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		_, ok := m[key]
		return ok
	}
	for _, c := range []struct {
		v       any
		key     string
		present bool
	}{
		{Heartbeat{}, "loaded_models", true}, // null, but sent
		{Heartbeat{}, "boot_id", true},
		{Heartbeat{}, "sleeping", false},
		{Heartbeat{}, "load", false},
		{Heartbeat{}, "resident_models", false},
		{Heartbeat{ResidentModels: &[]string{}}, "resident_models", true}, // nothing in memory is said, not omitted
		{Capabilities{}, "Engine", false},
		{RegisterRequest{}, "boot_id", true},
		{RegisterRequest{}, "hardware_json", true},
		{Capabilities{}, "GPUs", true},
		{Capabilities{}, "Role", false},
		{Capabilities{}, "PlanRevision", false},
		{Capabilities{}, "GivenDevices", false},                              // no statement: an older worker
		{Capabilities{GivenDevices: &[]GivenDevice{}}, "GivenDevices", true}, // looked and was given none: said, not omitted
		{GivenDevice{}, "PCI", false},
		{GivenDevice{}, "UUID", false},
		{GivenDevice{}, "Index", true},
		{GivenDevice{}, "Vendor", true},
		{LoadModelRequest{}, "file", false},
		{LoadModelRequest{}, "ollama_name", true},
		{LoadModelRequest{}, "pin", true},
		{UnloadModelRequest{}, "ollama_name", true},
		{UnloadModelRequest{}, "repo", true},
		{UnloadModelRequest{}, "path", true},
		{UnloadModelResponse{}, "status", true},
		{UnloadModelResponse{}, "model", false},
		{UnloadModelResponse{}, "engine", false},
		{UnloadModelResponse{}, "reason", false},
		{SleepResponse{}, "reason", false},
		{Adapter{}, "rank", false},
		{LoadAdapterRequest{}, "rank", false},
		{LoadAdapterRequest{}, "source", true},
		{ProcessSpec{}, "ReadyTimeout", true},
		{ProcessSpec{}, "Env", true},
		{ProcessInfo{}, "exit_err", false},
		{ProcessInfo{}, "address", false},
		{ProcessInfo{}, "restarts", false},
		{StartProcessError{}, "info", true}, // null, but sent
	} {
		if got := has(c.v, c.key); got != c.present {
			t.Errorf("%T: key %q present = %v, want %v", c.v, c.key, got, c.present)
		}
	}
}

// The container entrypoint decides a load succeeded by finding the literal
// text "status":"ready" in the answer, so the compact encoding of that pair
// is wire too.
func TestLoadModelResponseKeepsTheLiteralAShellScriptGrepsFor(t *testing.T) {
	raw, err := json.Marshal(LoadModelResponse{Status: StatusReady, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"status":"ready"`) {
		t.Errorf(`%s does not contain "status":"ready"`, raw)
	}
}

// Durations travel as integer nanoseconds.
func TestProcessSpecDurationsAreNanoseconds(t *testing.T) {
	raw, err := json.Marshal(ProcessSpec{ReadyTimeout: 15 * time.Minute, RestartBackoff: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"ReadyTimeout":900000000000`, `"RestartBackoff":1000000000`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s does not contain %s", raw, want)
		}
	}
}

// Every golden file is exercised: a body added to testdata without a case
// would be a wire nobody checks.
func TestEveryGoldenHasACase(t *testing.T) {
	cases := map[string]bool{}
	for _, c := range wireCases(t) {
		cases[c.golden] = true
	}
	files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		if !cases[name] {
			t.Errorf("testdata/%s.json has no case", name)
		}
		delete(cases, name)
	}
	for name := range cases {
		t.Errorf("case %s has no golden file", name)
	}
}

// A pod can be Running while the engine inside it crash-loops. Everything that
// counts workers counts that pod as capacity; it is not. The field is additive
// and a reader that does not know it must be unharmed.
func TestEngineStateIsAdditiveAndOptional(t *testing.T) {
	// An older worker sends no engine block at all.
	var old Heartbeat
	if err := json.Unmarshal([]byte(`{"id":"n_w0","loaded_models":["m"],"boot_id":"b"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Engine != nil {
		t.Fatal("no statement must stay no statement, never a healthy one")
	}

	hb := Heartbeat{ID: "n_w0", LoadedModels: []string{"m"}, BootID: "b",
		Engine: &EngineState{State: EngineCrashLooping, Restarts: 4, Since: "2026-09-20T09:00:00Z", Detail: "exit status 1: CUDA error", Version: "0.33.0"}}
	b, err := json.Marshal(hb)
	if err != nil {
		t.Fatal(err)
	}
	var back Heartbeat
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Engine == nil || back.Engine.State != EngineCrashLooping || back.Engine.Restarts != 4 || back.Engine.Version != "0.33.0" {
		t.Fatalf("round trip lost the engine state: %s", b)
	}
	// It is omitted when absent, so an older leader sees exactly what it saw.
	if bytes.Contains(mustJSON(t, Heartbeat{ID: "n", LoadedModels: []string{}, BootID: "b"}), []byte("engine")) {
		t.Error("a worker with nothing to say must not send the key")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// NormalizePCI is the one spelling both sides compare: nvidia-smi's
// eight-digit upper-case domain and sysfs's form name the same device.
func TestNormalizePCI(t *testing.T) {
	for in, want := range map[string]string{
		"00000000:41:00.0": "0000:41:00.0",
		"0000:03:00.0":     "0000:03:00.0",
		" 0000:0A:00.0\n":  "0000:0a:00.0",
		"0:3:00.0":         "0000:3:00.0",
		"00000001:41:00.0": "0001:41:00.0",
		"10000000:41:00.0": "10000000:41:00.0", // a domain that does not fit four digits is kept, not cut
		"41:00.0":          "41:00.0",          // no domain: returned as written
		"":                 "",
		"not-an-address":   "not-an-address",
	} {
		if got := NormalizePCI(in); got != want {
			t.Errorf("NormalizePCI(%q) = %q, want %q", in, got, want)
		}
	}
}

// engine_ttft_p50_ms (T24.25) is absent when the engine does not say, and a
// stated value survives the round trip.
func TestEngineLoadTTFTIsAbsentWhenUnsaid(t *testing.T) {
	raw, err := json.Marshal(EngineLoad{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "engine_ttft_p50_ms") {
		t.Fatalf("an unsaid engine TTFT is on the wire: %s", raw)
	}
	ms := int64(37)
	raw, _ = json.Marshal(EngineLoad{EngineTTFTP50Ms: &ms})
	var back EngineLoad
	if err := json.Unmarshal(raw, &back); err != nil || back.EngineTTFTP50Ms == nil || *back.EngineTTFTP50Ms != 37 {
		t.Fatalf("round trip: %s → %+v (%v)", raw, back, err)
	}
}
