package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// A worker that holds a GPU never serves from the CPU.
//
// llama.cpp does not treat a missing GPU as fatal: when its backend cannot
// start (measured on a Radeon host whose cards had fallen into a runtime-PM
// error: "failed to initialize ROCm: no ROCm-capable device is detected") it
// prints "no usable GPU found, --gpu-layers option will be ignored" and loads
// the model into host RAM. The worker had already registered, the load was
// reported resident, and the launch line still carried a KV pool sized for the
// card's memory — so the pod was OOMKilled in host RAM, and with a smaller
// pool it would have served correct text from the CPU while every view said a
// GPU was serving it.
//
// So before a whole llama-server starts on a worker that holds an accelerator,
// the worker asks the same binary which devices it can reach
// (`llama-server --list-devices`, which initialises every backend it was built
// with and prints what it found). No device is a refused load with llama.cpp's
// own words, and a worker with no model is what the leader and the manager
// already see and act on (SelfLoad). An answer that cannot be read — an older
// build without the flag — refuses nothing: it is logged and the launch goes on.

// llamaDeviceProbe runs `llama-server --list-devices` and returns its combined
// output. Through a login shell for the same reason the launch is
// (launchLlamaServer: a direct exec segfaults the Intel CPU build). A var so
// tests can stand in for the binary.
var llamaDeviceProbe = func(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "/bin/sh", "-lc", "exec llama-server --list-devices").CombinedOutput()
	return string(out), err
}

// llamaDeviceProbeTimeout bounds the probe: backend initialisation is seconds,
// and a hung driver must not hold the load forever.
const llamaDeviceProbeTimeout = 60 * time.Second

// llamaDevices reads `--list-devices` output: the devices listed under
// "Available devices:", and whether that header was there at all (false = the
// output is not an answer this code can judge).
func llamaDevices(out string) (devices []string, answered bool) {
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "Available devices:" {
			continue
		}
		for _, d := range lines[i+1:] {
			d = strings.TrimSpace(d)
			if d == "" || d == "(none)" {
				continue
			}
			devices = append(devices, d)
		}
		return devices, true
	}
	return nil, false
}

// checkLlamaReachesDevice refuses a llama-server launch on a worker that holds
// an accelerator when llama.cpp can reach none. A plan that pinned ngl (0 is an
// explicit CPU run) is the operator's decision and is not second-guessed.
func (s *Server) checkLlamaReachesDevice(ctx context.Context) error {
	if _, pinned := s.EngineFlags.get("ngl"); pinned || !s.Accelerated {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, llamaDeviceProbeTimeout)
	defer cancel()
	out, err := llamaDeviceProbe(ctx)
	devices, answered := llamaDevices(out)
	if !answered {
		s.logger().Warn("llama-server --list-devices gave no device list; launching without the GPU check", "err", err, "output", outputTail(out, 400))
		return nil
	}
	if len(devices) == 0 {
		return fmt.Errorf("this worker holds a GPU but llama.cpp can reach no device, so it would serve from the CPU: %s", outputTail(out, 400))
	}
	return nil
}

// outputTail is the last n bytes of s, trimmed — enough of an engine's output to say
// why without carrying its whole banner.
func outputTail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	return s
}
