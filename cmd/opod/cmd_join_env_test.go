package main

import (
	"os"
	"strings"
	"testing"
)

// `opod join --gpu / --vram-budget` reach the engines through the
// supervisor's base environment, never through os.Setenv on the worker
// process; an operator's own device pin is left alone.
func TestWorkerBaseEnv(t *testing.T) {
	t.Setenv("CUDA_VISIBLE_DEVICES", "")
	t.Setenv("HIP_VISIBLE_DEVICES", "")
	t.Setenv("ONEAPI_DEVICE_SELECTOR", "")
	env := workerBaseEnv("1", "12")
	for k, want := range map[string]string{"OPOD_GPU_INDEX": "1", "CUDA_VISIBLE_DEVICES": "1", "HIP_VISIBLE_DEVICES": "1", "ONEAPI_DEVICE_SELECTOR": "level_zero:1", "OPOD_VRAM_BUDGET_GB": "12"} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	if got := workerBaseEnv("", ""); len(got) != 0 {
		t.Errorf("no flags, no env: %v", got)
	}
	t.Setenv("CUDA_VISIBLE_DEVICES", "0,1")
	if got := workerBaseEnv("1", ""); got["CUDA_VISIBLE_DEVICES"] != "" || got["OPOD_GPU_INDEX"] != "1" {
		t.Errorf("an operator's own CUDA_VISIBLE_DEVICES must win: %v", got)
	}
}

// A join URL without `?token=` parses (the certificate is the credential under
// node mTLS, R9.6 / PLAN T7.3); the token is still returned when present, and
// the leader URL never keeps the query.
func TestParseJoinTargetTokenOptional(t *testing.T) {
	leader, token, err := parseJoinTarget("https://ep.opod.svc:8080?token=sk-orc-abc")
	if err != nil || leader != "https://ep.opod.svc:8080" || token != "sk-orc-abc" {
		t.Fatalf("with token: %q %q %v", leader, token, err)
	}
	leader, token, err = parseJoinTarget("https://ep.opod.svc:8080")
	if err != nil || leader != "https://ep.opod.svc:8080" || token != "" {
		t.Fatalf("without token: %q %q %v", leader, token, err)
	}
	// The minted callback secret is per process and never the same twice.
	if a, b := generateCallbackSecret(), generateCallbackSecret(); a == b || len(a) < 40 {
		t.Fatalf("callback secret: %q %q", a, b)
	}
}

// A worker image passes its join token through the environment and a bare URL,
// because a token on the command line is readable by anyone who can list
// processes (PLAN T8.20). The URL's token still wins when a person typed one;
// neither = "", which only a certificate join accepts.
func TestJoinCredential(t *testing.T) {
	for _, c := range []struct{ url, configured, want string }{
		{"sk-orc-url", "sk-orc-env", "sk-orc-url"},
		{"", "sk-orc-env", "sk-orc-env"},
		{"sk-orc-url", "", "sk-orc-url"},
		{"", "", ""},
	} {
		if got := joinCredential(c.url, c.configured); got != c.want {
			t.Errorf("joinCredential(%q, %q) = %q, want %q", c.url, c.configured, got, c.want)
		}
	}
}

// The image's own command line carries no token: the entrypoint must hand
// `opod join` the bare leader URL and leave OPOD_JOIN_TOKEN in the environment.
func TestEntrypointKeepsTheJoinTokenOffTheCommandLine(t *testing.T) {
	b, err := os.ReadFile("../../images/entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "#") || !strings.Contains(l, "opod join") {
			continue
		}
		if strings.Contains(l, "token=") || strings.Contains(l, "OPOD_JOIN_TOKEN") {
			t.Errorf("entrypoint puts the join token on the command line: %s", l)
		}
	}
}
