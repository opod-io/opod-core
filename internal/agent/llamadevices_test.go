package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The output measured on the Radeon host whose cards could not start ROCm, and
// one from a healthy card.
const (
	llamaNoDevice = `0.05.007.411 E ggml_cuda_init: failed to initialize ROCm: no ROCm-capable device is detected
Available devices:
  (none)
`
	llamaOneDevice = `ggml_cuda_init: found 1 ROCm devices:
  Device 0: Radeon RX 7900 XTX, gfx1100 (0x1100), VMM: no, Wave Size: 32
Available devices:
  ROCm0: Radeon RX 7900 XTX (24560 MiB, 24472 MiB free)
`
)

func TestAWorkerHoldingAGPUThatLlamaCppCannotReachRefusesTheLoad(t *testing.T) {
	for _, tc := range []struct {
		name        string
		accelerated bool
		flags       EngineFlags
		out         string
		probeErr    error
		refused     bool
	}{
		{"no device on a GPU worker", true, nil, llamaNoDevice, nil, true},
		{"a device on a GPU worker", true, nil, llamaOneDevice, nil, false},
		{"a CPU worker is not asked", false, nil, llamaNoDevice, nil, false},
		{"a pinned ngl is the operator's call", true, EngineFlags{"ngl": "0"}, llamaNoDevice, nil, false},
		{"an older build without the flag refuses nothing", true, nil, "error: invalid argument: --list-devices\n", errors.New("exit status 1"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := false
			old := llamaDeviceProbe
			llamaDeviceProbe = func(context.Context) (string, error) { asked = true; return tc.out, tc.probeErr }
			t.Cleanup(func() { llamaDeviceProbe = old })

			s := &Server{Accelerated: tc.accelerated, EngineFlags: tc.flags}
			err := s.checkLlamaReachesDevice(context.Background())
			if (err != nil) != tc.refused {
				t.Fatalf("refused=%v, want %v (err %v)", err != nil, tc.refused, err)
			}
			if tc.refused && !strings.Contains(err.Error(), "no ROCm-capable device") {
				t.Errorf("the refusal should carry llama.cpp's own words: %v", err)
			}
			if asked != (tc.accelerated && tc.flags == nil) {
				t.Errorf("probe asked=%v", asked)
			}
		})
	}
}

func TestLlamaDevicesReadsTheListUnderItsHeader(t *testing.T) {
	if d, ok := llamaDevices(llamaOneDevice); !ok || len(d) != 1 || !strings.HasPrefix(d[0], "ROCm0:") {
		t.Errorf("one device: %q %v", d, ok)
	}
	if d, ok := llamaDevices(llamaNoDevice); !ok || len(d) != 0 {
		t.Errorf("no device: %q %v", d, ok)
	}
	if _, ok := llamaDevices("segfault"); ok {
		t.Error("output with no header is not an answer")
	}
}
