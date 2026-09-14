package agent

// LoRA adapters as variants of one model identity (ADR-040 gap G2, ROADMAP
// R9.2; feature "lora"). An adapter never becomes a second model: it is
// served under "<base>:<name>" by the worker that holds the base, loaded into
// the engine's LoRA slots (vLLM `--enable-lora` + the runtime
// /v1/load_lora_adapter route), reported in the heartbeat under that
// suffixed id (so the leader's placements route it), and rewritten back to
// the engine's own adapter name on the way in. A leader whose plan pins one
// model accepts its "<model>:<adapter>" variants (controlplane.planAllowsModel).
//
// Adapters arrive two ways: OPOD_ADAPTERS in the worker's env (a JSON list of
// {name, source} — what a control plane renders from its plan) is loaded once
// the base model is resident; POST /v1/adapters/load|unload changes the set
// at runtime.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Adapter is one LoRA: the name it is served under (after the base id and
// a colon) and where its weights are (a Hugging Face repo or a local path —
// vLLM resolves either).
type Adapter struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

var adapterNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// AdapterID is the model id an adapter is served under.
func AdapterID(base, name string) string { return base + ":" + name }

// adaptersFromEnv reads OPOD_ADAPTERS; a malformed value is logged by the
// caller and ignored — never a crash-looping worker.
func adaptersFromEnv() ([]Adapter, error) {
	raw := strings.TrimSpace(os.Getenv("OPOD_ADAPTERS"))
	if raw == "" || raw == "null" || raw == "[]" {
		return nil, nil
	}
	var out []Adapter
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("OPOD_ADAPTERS: %w", err)
	}
	for _, a := range out {
		if !adapterNameRe.MatchString(a.Name) || a.Source == "" {
			return nil, fmt.Errorf("OPOD_ADAPTERS: adapter %q needs a lower-case name and a source", a.Name)
		}
	}
	return out, nil
}

// vllmLoRAArgs is what `vllm serve` needs so adapters can be loaded at
// runtime; empty when no adapter is configured (the engine is then exactly
// what it was).
func vllmLoRAArgs(n int) string {
	if n <= 0 {
		return ""
	}
	if n < 4 {
		n = 4
	}
	return fmt.Sprintf("--enable-lora --max-loras %d", n)
}

// adapterState is the worker's record of what it holds.
type adapterState struct {
	mu   sync.Mutex
	held map[string]Adapter // adapter id → adapter
}

func (s *Server) adapters() *adapterState {
	s.adaptersOnce.Do(func() { s.adapterSet = &adapterState{held: map[string]Adapter{}} })
	return s.adapterSet
}

// engineBase is the engine's own base URL (the vLLM driver knows it).
func (s *Server) engineBase() string {
	if ep, ok := s.Engine.(interface{ Endpoint() string }); ok {
		return strings.TrimRight(ep.Endpoint(), "/")
	}
	return "http://127.0.0.1:8000"
}

// loadAdapter asks the engine to load one adapter and records the id.
func (s *Server) loadAdapter(ctx context.Context, base string, a Adapter) error {
	if !strings.HasPrefix(s.Engine.Name(), "vllm") {
		return fmt.Errorf("adapters need an engine with LoRA slots: vLLM (this worker runs %s)", s.Engine.Name())
	}
	if !adapterNameRe.MatchString(a.Name) || a.Source == "" {
		return fmt.Errorf("adapter needs a lower-case name and a source")
	}
	body, _ := json.Marshal(map[string]string{"lora_name": a.Name, "lora_path": a.Source})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.engineBase()+"/v1/load_lora_adapter", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("vllm load_lora_adapter: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("vllm load_lora_adapter %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	id := AdapterID(base, a.Name)
	s.Aliases.Note(a.Name, id) // the heartbeat reports "<base>:<name>" for vLLM's "<name>"
	st := s.adapters()
	st.mu.Lock()
	st.held[id] = a
	st.mu.Unlock()
	return nil
}

// unloadAdapter removes one adapter from the engine and the record.
func (s *Server) unloadAdapter(ctx context.Context, base, name string) error {
	body, _ := json.Marshal(map[string]string{"lora_name": name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.engineBase()+"/v1/unload_lora_adapter", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("vllm unload_lora_adapter: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("vllm unload_lora_adapter %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	s.Aliases.Forget(name)
	st := s.adapters()
	st.mu.Lock()
	delete(st.held, AdapterID(base, name))
	st.mu.Unlock()
	return nil
}

// loadAdaptersWhenReady loads the env-configured adapters once the base model
// is resident (vLLM binds its port only after the weights load). Bounded by
// the same patience as the engine launch; a failure is logged per adapter and
// retried on the next attempt, never fatal to the worker.
func (s *Server) loadAdaptersWhenReady(base string, adapters []Adapter) {
	if len(adapters) == 0 {
		return
	}
	deadline := time.Now().Add(40 * time.Minute)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		models, err := s.Engine.List(ctx)
		cancel()
		resident := false
		for _, m := range models {
			if m == base || s.Aliases.ID(m) == base {
				resident = true
				break
			}
		}
		if err != nil || !resident {
			time.Sleep(10 * time.Second)
			continue
		}
		pending := adapters[:0:0]
		for _, a := range adapters {
			lctx, lcancel := context.WithTimeout(context.Background(), 10*time.Minute)
			err := s.loadAdapter(lctx, base, a)
			lcancel()
			if err != nil {
				s.logf("adapter %s: %v (will retry)", a.Name, err)
				pending = append(pending, a)
			}
		}
		if len(pending) == 0 {
			return
		}
		adapters = pending
		time.Sleep(30 * time.Second)
	}
}

func (s *Server) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[worker] "+format+"\n", args...)
}

// adaptersLoad — POST /v1/adapters/load {base, name, source}.
func (s *Server) adaptersLoad(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req struct {
		Base   string `json:"base"`
		Name   string `json:"name"`
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Base == "" {
		http.Error(w, "base, name and source required", http.StatusBadRequest)
		return
	}
	if err := s.loadAdapter(r.Context(), req.Base, Adapter{Name: req.Name, Source: req.Source}); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready", "model": AdapterID(req.Base, req.Name)})
}

// adaptersUnload — POST /v1/adapters/unload {base, name}.
func (s *Server) adaptersUnload(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req struct {
		Base string `json:"base"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Base == "" || req.Name == "" {
		http.Error(w, "base and name required", http.StatusBadRequest)
		return
	}
	if err := s.unloadAdapter(r.Context(), req.Base, req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "unloaded", "model": AdapterID(req.Base, req.Name)})
}

// adaptersList — GET /v1/adapters.
func (s *Server) adaptersList(w http.ResponseWriter, _ *http.Request) {
	st := s.adapters()
	st.mu.Lock()
	out := make([]map[string]string, 0, len(st.held))
	for id, a := range st.held {
		out = append(out, map[string]string{"id": id, "name": a.Name, "source": a.Source})
	}
	st.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
