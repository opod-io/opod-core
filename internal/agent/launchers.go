package agent

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// An engine the worker launches for the model it is told to serve: vLLM,
// SGLang and TensorRT-LLM have no persistent server and their drivers never
// start one, so the model only becomes resident once the worker starts the
// server for it — under the supervisor, fire-and-forget, because the port
// binds only after a load of minutes. One table, so a new engine of this kind
// is a row and not another name-prefix branch in the load path (T19.15).
type engineLauncher struct {
	prefix   string // the driver name it serves (engines.Engine.Name prefix)
	process  string // the supervisor id; also what unload and sleep look for
	adapters bool   // LoRA adapters load through the engine's runtime route once it is up
	launch   func(s *Server, model, servedName string) error
}

var launchers = []engineLauncher{
	{prefix: "vllm", process: "vllm-serve", adapters: true, launch: (*Server).launchVLLM},
	{prefix: "sglang", process: "sglang-serve", launch: (*Server).launchSGLang},
	{prefix: "trtllm", process: "trtllm-serve", launch: (*Server).launchTRTLLM},
}

// launcherFor is the launcher of the engine a driver name belongs to.
func launcherFor(engine string) (engineLauncher, bool) {
	for _, l := range launchers {
		if strings.HasPrefix(engine, l.prefix) {
			return l, true
		}
	}
	return engineLauncher{}, false
}

// engineAddr is the host and port the driver probes, which the launched
// server must bind; def when the driver names none.
func (s *Server) engineAddr(defPort int) (string, int) {
	host, port := "127.0.0.1", defPort
	if ep, ok := s.Engine.(interface{ Endpoint() string }); ok {
		if u, err := url.Parse(ep.Endpoint()); err == nil {
			if h := u.Hostname(); h != "" {
				host = h
			}
			if p := u.Port(); p != "" {
				if n, err := strconv.Atoi(p); err == nil {
					port = n
				}
			}
		}
	}
	return host, port
}

// launchTRTLLM (re)starts `trtllm-serve` for one model on the PyTorch backend:
// safetensors from the Hub or a prefetched snapshot, no engine build. Flags:
//
//	--tp_size                              how many cards (the same power-of-two rule)
//	--max_batch_size                       the slot count the worker reports (ADR-091)
//	--max_seq_len                          the plan's max_model_len
//	--kv_cache_free_gpu_memory_fraction    a share of the memory LEFT AFTER the weights
//	                                       (T23.3 owns the translation from a budget)
//
// trtllm-serve takes no --revision, so a pinned revision is served only from
// its prefetched snapshot; without one the launch is refused rather than
// serving whatever the repo holds today. No sleep mode, no runtime LoRA.
func (s *Server) launchTRTLLM(model, servedName string) error {
	host, port := s.engineAddr(8000)
	cmdline, err := s.trtllmCmdline(model, host, port)
	if err != nil {
		return err
	}
	s.launchedSlots.Store(int64(s.EngineFlags.LaunchSlots("trtllm")))
	_ = s.Supervisor.Stop("trtllm-serve") // exclusive: one model per worker
	_, err = s.Supervisor.Start(context.Background(), ProcessSpec{
		ID:          "trtllm-serve",
		Command:     "/bin/sh",
		Args:        []string{"-lc", cmdline},
		Restart:     true,
		MaxRestarts: 3,
	})
	return err
}

// trtllmCmdline is the shell line launchTRTLLM runs. trtllm-serve has no
// --served-model-name: it serves under the path or repo it was given, which is
// why the driver resolves the catalog id to that native name (openaicompat).
func (s *Server) trtllmCmdline(model, host string, port int) (string, error) {
	source, revArgs, err := s.pinnedSource(model)
	if err != nil {
		return "", err
	}
	if revArgs != "" {
		return "", fmt.Errorf("model %s: TensorRT-LLM cannot pin revision %s by flag — prefetch the snapshot so the worker serves it from %s", model, s.ModelRevision, s.ModelsDir)
	}
	flagOverrides, flagArgs := s.EngineFlags.trtllmShellOverrides()
	return gpuCountShell + fmt.Sprintf(
		"%s exec trtllm-serve '%s' --host %s --port %d --backend pytorch --tp_size \"$TP\" --trust_remote_code %s",
		flagOverrides, source, host, port, flagArgs), nil
}
