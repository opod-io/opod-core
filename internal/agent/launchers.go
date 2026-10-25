package agent

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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
//	--tool_parser                          the catalog's parser for the model (T27.8)
//	--config                               an options file that always turns on
//	                                       guided decoding (trtllmConfig)
//
// trtllm-serve has no served-model-name: it lists the model under the base
// name of the directory it was given, or the repo id, and answers whatever
// `model` a request names. So the worker records THAT name as this model's
// native one, or its heartbeat would report a name the leader cannot place
// (read on the cell, 2026-10-19). No sleep mode, no runtime LoRA.
func (s *Server) launchTRTLLM(model, servedName string) error {
	host, port := s.engineAddr(8000)
	if servedName == "" {
		servedName = model
	}
	cmdline, servedAs, err := s.trtllmCmdline(model, servedName, host, port)
	if err != nil {
		return err
	}
	s.noteToolParser("trtllm", servedName)
	s.Aliases.Forget(model)
	s.Aliases.Note(servedAs, servedName)
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

// trtllmCmdline is the shell line launchTRTLLM runs, and the name the engine
// will list the model under: a directory's base name, else the repo id.
// --revision is spelled as vLLM's and SGLang's (pinnedSource); servedName is
// the catalog id the tool-call parser is looked up by.
func (s *Server) trtllmCmdline(model, servedName, host string, port int) (cmdline, servedAs string, err error) {
	source, revArgs, err := s.pinnedSource(model)
	if err != nil {
		return "", "", err
	}
	servedAs = source
	if filepath.IsAbs(source) {
		servedAs = filepath.Base(source)
	}
	if revArgs != "" {
		revArgs += " "
	}
	config, err := s.trtllmConfig()
	if err != nil {
		return "", "", err
	}
	flagOverrides, flagArgs := s.EngineFlags.trtllmShellOverrides(config)
	if ta := s.toolParserArgs("trtllm", servedName); ta != "" {
		flagArgs = strings.TrimSpace(flagArgs + " " + ta)
	}
	return gpuCountShell + fmt.Sprintf(
		"%s exec trtllm-serve '%s' %s--host %s --port %d --backend pytorch --tp_size \"$TP\" --trust_remote_code %s",
		flagOverrides, source, revArgs, host, port, flagArgs), servedAs, nil
}

// trtllmGuidedDecoding turns on structured output (`response_format`): the
// LLM-API option has no command-line flag on release 1.2.1, only the
// --config file. Without it trtllm-serve accepts a json_schema request and
// answers it in prose (PLAN T27.8).
const trtllmGuidedDecoding = "guided_decoding_backend: xgrammar"

// trtllmConfig writes the one options file trtllm-serve reads and returns its
// path: the file the plan's extra names (the control plane's KV tier, T26.10),
// with guided decoding added unless that file already chooses a backend.
// trtllm-serve takes one --config, so the two are merged here. The file lives
// in the container's temp directory: it belongs to this process, not to the
// shared model cache.
func (s *Server) trtllmConfig() (string, error) {
	extra, _ := s.EngineFlags.get("extra")
	var body []byte
	if p := trtllmConfigPath(extra); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("read trtllm-serve options %s: %w", p, err)
		}
		body = b
	}
	if !yamlHasTopKey(body, "guided_decoding_backend") {
		if len(body) > 0 && body[len(body)-1] != '\n' {
			body = append(body, '\n')
		}
		body = append(body, trtllmGuidedDecoding+"\n"...)
	}
	out := filepath.Join(os.TempDir(), "opod-trtllm-options.yaml")
	if err := os.WriteFile(out, body, 0o644); err != nil {
		return "", fmt.Errorf("write trtllm-serve options %s: %w", out, err)
	}
	return out, nil
}

// yamlHasTopKey reports whether a YAML document sets key at its top level.
func yamlHasTopKey(doc []byte, key string) bool {
	for _, line := range strings.Split(string(doc), "\n") {
		if strings.HasPrefix(line, key+":") {
			return true
		}
	}
	return false
}
