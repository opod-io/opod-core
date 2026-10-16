//go:build perfgate

package perfgate

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// opodBinary is $OPOD_BIN when set (CI builds once and hands it over), else a
// fresh build of ./cmd/opod into the test's temp dir.
func opodBinary(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("OPOD_BIN"); bin != "" {
		abs, err := filepath.Abs(bin)
		if err != nil {
			t.Fatalf("OPOD_BIN: %v", err)
		}
		return abs
	}
	bin := filepath.Join(t.TempDir(), "opod")
	build := exec.Command("go", "build", "-trimpath", "-o", bin, "../../cmd/opod")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/opod: %v\n%s", err, out)
	}
	return bin
}

// freeAddr returns a loopback address nothing listens on right now.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().String()
}

// process is one opod process; its output goes to a log file that is printed
// when the test fails, because a gate that fails without the leader's own
// words sends the reader to re-run it.
type process struct {
	name string
	cmd  *exec.Cmd
	log  string
}

func start(t *testing.T, name, bin string, env []string, args ...string) *process {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), name+".log")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("%s log: %v", name, err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	p := &process{name: name, cmd: cmd, log: logPath}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		f.Close()
		if t.Failed() {
			p.dump(t)
		}
	})
	return p
}

func (p *process) dump(t *testing.T) {
	out, _ := os.ReadFile(p.log)
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	t.Logf("--- last lines of %s ---\n%s", p.name, strings.Join(lines, "\n"))
}

var tokenRE = regexp.MustCompile(`sk-orc-[A-Za-z0-9_-]+`)

// cluster is one leader and one worker in front of the stub engine, started
// the way an operator starts them: `opod up`, `opod token create --node`,
// `opod join`.
type cluster struct {
	leaderURL string
}

func startCluster(t *testing.T, bin, engineURL string) cluster {
	t.Helper()
	leaderAddr, workerAddr := freeAddr(t), freeAddr(t)
	leaderDir, workerDir := t.TempDir(), t.TempDir()
	quiet := []string{"OPOD_NO_UPDATE_CHECK=1"}

	// The leader is router-only. Its own engine points at a closed port so it
	// can never answer locally — arm B must cross the worker hop, as it does in
	// a cluster.
	leader := start(t, "leader", bin, append(quiet,
		"OPOD_DATA_DIR="+leaderDir,
		"OPOD_LISTEN="+leaderAddr,
		"OPOD_REQUIRE_KEYS=false",
		"OPOD_OLLAMA_ENDPOINT=http://127.0.0.1:1",
	), "up", "--no-wizard", "--auto-pull=false")
	leaderURL := "http://" + leaderAddr
	waitFor(t, leader, 30*time.Second, "leader /healthz", func() bool {
		resp, err := http.Get(leaderURL + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})

	mint := exec.Command(bin, "token", "create", "perfgate", "--node")
	mint.Env = append(os.Environ(), append(quiet, "OPOD_DATA_DIR="+leaderDir)...)
	out, err := mint.CombinedOutput()
	if err != nil {
		t.Fatalf("opod token create --node: %v\n%s", err, out)
	}
	token := tokenRE.FindString(string(out))
	if token == "" {
		t.Fatalf("no token in `opod token create` output:\n%s", out)
	}

	worker := start(t, "worker", bin, append(quiet,
		"OPOD_DATA_DIR="+workerDir,
		"OPOD_ENGINE=vllm",
		"OPOD_VLLM_ENDPOINT="+engineURL,
		"OPOD_ADVERTISE_ADDR="+workerAddr,
	), "join", leaderURL+"?token="+token)

	// Placed means a streamed request through the leader comes back with
	// tokens: the worker registered, heartbeat its model and the router dials it.
	probe := &http.Client{Timeout: 10 * time.Second}
	waitFor(t, worker, 60*time.Second, "a token through the leader", func() bool {
		s, err := stream(context.Background(), probe, leaderURL)
		return err == nil && s.tokens > 0
	})
	return cluster{leaderURL: leaderURL}
}

func waitFor(t *testing.T, p *process, limit time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if p.cmd.ProcessState != nil {
			t.Fatalf("%s exited while waiting for %s", p.name, what)
		}
		if ok() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no %s after %s", what, limit)
}

// sample is one streamed request as the caller saw it.
type sample struct {
	ttft   time.Duration // to the first chunk that carries content
	total  time.Duration
	tokens int
}

var chatBody = []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"count"}],"stream":true,"max_tokens":64}`, stubModel))

func stream(ctx context.Context, client *http.Client, baseURL string) (sample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(chatBody))
	if err != nil {
		return sample{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	began := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return sample{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return sample{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	var s sample
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) || !bytes.Contains(line, []byte(`"content":"`)) {
			continue
		}
		if bytes.Contains(line, []byte(`"content":""`)) {
			continue
		}
		if s.tokens == 0 {
			s.ttft = time.Since(began)
		}
		s.tokens++
	}
	if err := sc.Err(); err != nil {
		return sample{}, err
	}
	s.total = time.Since(began)
	if s.tokens == 0 {
		return sample{}, fmt.Errorf("no content chunks")
	}
	return s, nil
}
