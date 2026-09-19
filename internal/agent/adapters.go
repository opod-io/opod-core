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
// {name, source, rank} — what a control plane renders from its plan) is loaded
// once the base model is resident; POST /v1/adapters/load|unload changes the
// set at runtime, within what the engine was started for (loraFits).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// Rank is the adapter's own r, when whoever wrote OPOD_ADAPTERS knows it.
	// vLLM sizes its LoRA slots at start (--max-lora-rank, default 16) and
	// refuses a bigger adapter at load time, so the engine must be started for
	// the largest rank in the set. Zero means "not stated": the default stands.
	Rank int `json:"rank,omitempty"`
}

var adapterNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// AdapterID is the model id an adapter is served under.
func AdapterID(base, name string) string { return base + ":" + name }

// ParseAdapters parses the OPOD_ADAPTERS value (config.Env.Adapters); a
// malformed value is logged by the caller and ignored — never a
// crash-looping worker.
func ParseAdapters(raw string) ([]Adapter, error) {
	raw = strings.TrimSpace(raw)
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
// what it was). maxRank is the largest rank in the set: vLLM's own default is
// 16 and a rank above it fails at load time, so the engine is started for the
// next size it accepts.
func vllmLoRAArgs(n, maxRank int) string {
	if n <= 0 {
		return ""
	}
	if n < 4 {
		n = 4
	}
	args := fmt.Sprintf("--enable-lora --max-loras %d", n)
	if r := loRARankFor(maxRank); r > 0 {
		args += fmt.Sprintf(" --max-lora-rank %d", r)
	}
	return args
}

// vllmDefaultLoRARank is vLLM's own --max-lora-rank when the flag is absent.
const vllmDefaultLoRARank = 16

// loRARankFor rounds a rank up to a value vLLM accepts for --max-lora-rank
// (1, 8, 16, 32, 64, 128, 256); 0 when the default (16) already covers it, so
// the flag is passed only when it changes something.
func loRARankFor(maxRank int) int {
	if maxRank <= vllmDefaultLoRARank {
		return 0
	}
	for _, r := range []int{32, 64, 128, 256} {
		if maxRank <= r {
			return r
		}
	}
	return 256 // vLLM's largest; a bigger adapter is refused by the engine, and says so
}

// MaxAdapterRank is the largest rank in a set (0 = none stated).
func MaxAdapterRank(as []Adapter) int {
	largest := 0
	for _, a := range as {
		if a.Rank > largest {
			largest = a.Rank
		}
	}
	return largest
}

// loraLaunch is what this worker's own `vllm serve` launch fixed for LoRA.
// vLLM sizes its slots once, at start: an engine started without
// --enable-lora takes no adapter at all, and one started for rank r refuses a
// larger adapter when it is loaded — with an error that names neither number.
// Recorded by launchVLLM so a live add (POST /v1/adapters/load) that cannot
// work is refused HERE, saying what the engine has, what the adapter needs
// and that only a restart changes it.
type loraLaunch struct {
	slots   bool // --enable-lora was passed (an adapter was configured at start)
	maxRank int  // the effective --max-lora-rank (vLLM's default when the flag was not passed)
}

// noteLoRALaunch records the LoRA sizing of the launch just made. Nothing is
// recorded — and so nothing is ever refused on the engine's behalf — when the
// operator's own extra flags mention LoRA: then only the engine knows its size.
func (s *Server) noteLoRALaunch() {
	if extra, _ := s.EngineFlags.get("extra"); strings.Contains(strings.ToLower(extra), "lora") {
		s.loraLaunched.Store(nil)
		return
	}
	var adapters []Adapter
	if s.AdaptersErr == nil {
		adapters = s.Adapters
	}
	l := &loraLaunch{slots: len(adapters) > 0, maxRank: loRARankFor(MaxAdapterRank(adapters))}
	if l.maxRank == 0 {
		l.maxRank = vllmDefaultLoRARank
	}
	s.loraLaunched.Store(l)
}

// errAdapterNeedsRestart: the running engine cannot take this adapter and no
// retry will change that.
type errAdapterNeedsRestart struct{ msg string }

func (e errAdapterNeedsRestart) Error() string { return e.msg }

// loraFits checks an adapter against the launch this worker made. A worker
// that did not launch its engine (an external vLLM), an adapter with no
// stated rank, and an operator-sized engine all pass: the engine answers.
func (s *Server) loraFits(a Adapter) error {
	l := s.loraLaunched.Load()
	if l == nil {
		return nil
	}
	const remedy = "restart the worker with the adapter in OPOD_ADAPTERS (name, source and rank) so the engine is started for it"
	if !l.slots {
		return errAdapterNeedsRestart{fmt.Sprintf("adapter %q: this worker's engine was started without LoRA slots — no adapter was configured at start, so vLLM runs without --enable-lora and takes none at runtime; %s", a.Name, remedy)}
	}
	if a.Rank > l.maxRank {
		return errAdapterNeedsRestart{fmt.Sprintf("adapter %q has rank %d, but this worker's engine was started with --max-lora-rank %d; vLLM fixes that at launch and refuses a larger adapter at load time; %s", a.Name, a.Rank, l.maxRank, remedy)}
	}
	return nil
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
	if err := s.loraFits(a); err != nil {
		return err
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
// retried on the next attempt, never fatal to the worker. gen is the launch it
// waits on (Server.launchGen): a later launch, or an unload of the base, ends
// it — the engine it was loading into is gone.
func (s *Server) loadAdaptersWhenReady(base string, adapters []Adapter, gen int64) {
	if len(adapters) == 0 {
		return
	}
	deadline := time.Now().Add(40 * time.Minute)
	for time.Now().Before(deadline) && s.launchGen.Load() == gen {
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
			if s.launchGen.Load() != gen {
				return
			}
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

// adaptersLoad — POST /v1/adapters/load {base, name, source, rank?}. rank is
// the adapter's own r when the caller knows it (0 or absent = not stated):
// the same field OPOD_ADAPTERS carries at start, so the live path can say
// "this engine was started for rank 16" instead of relaying the engine's
// refusal. 409 = the running engine cannot take it; a restart is needed.
func (s *Server) adaptersLoad(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req struct {
		Base   string `json:"base"`
		Name   string `json:"name"`
		Source string `json:"source"`
		Rank   int    `json:"rank"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Base == "" {
		http.Error(w, "base, name and source required", http.StatusBadRequest)
		return
	}
	if req.Rank < 0 {
		http.Error(w, "rank must not be negative", http.StatusBadRequest)
		return
	}
	if err := s.loadAdapter(r.Context(), req.Base, Adapter{Name: req.Name, Source: req.Source, Rank: req.Rank}); err != nil {
		code := http.StatusBadGateway
		var restart errAdapterNeedsRestart
		if errors.As(err, &restart) {
			code = http.StatusConflict
		}
		http.Error(w, err.Error(), code)
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
