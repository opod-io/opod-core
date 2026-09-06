// Process supervisor. Used on both leader (to launch the coordinator
// llama-server for sharded models) and workers (to launch rpc-server per
// shard). Wraps os/exec with start/stop/list/logs + a TCP-port readiness
// probe so callers can wait until a launched process is actually serving.
//
// Supports optional restart-on-crash: set ProcessSpec.Restart and the
// supervisor will re-launch the process (with exponential backoff, capped
// at MaxRestarts) when it exits abnormally. Used by the sharding
// orchestrator so a single rpc-server dying mid-stream doesn't take the
// model offline until an admin re-runs `opod shard create`.
package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// stableUptime is how long a generation must stay up (ready → exit) before
// the restart counter resets. Without it, crashes accumulated months apart
// would eventually trip the crashloop cap on a process that is, in practice,
// perfectly healthy.
const stableUptime = 10 * time.Minute

// ProcessSpec describes a child process to launch.
type ProcessSpec struct {
	ID         string // caller-assigned unique id
	Command    string // absolute path or PATH-resolvable binary
	Args       []string
	Env        map[string]string // appended to os.Environ()
	WorkDir    string            // optional; defaults to CWD
	HealthPort int               // optional; if >0, waitReady probes TCP this port
	HealthHost string            // default "127.0.0.1"
	LogLines   int               // ring-buffer capacity (default 200)

	// HealthPath, when set, upgrades readiness from "the TCP port is open" to
	// "the server actually answers 200 here" (e.g. "/health").
	//
	// A bare TCP probe is not readiness for a model server: llama-server binds
	// its socket immediately and only THEN loads the weights, so the dial
	// succeeds within milliseconds while the model is minutes away from serving
	// — or hangs forever. The shard was stamped "ready", `shard create` returned
	// success, and the failure surfaced much later as an unexplained 502 from the
	// gateway. Probe the real endpoint so a model that cannot serve fails loudly
	// here, at creation, with its logs attached.
	HealthPath string

	// ReadyTimeout bounds the readiness probe. Default 30s, which suits a
	// process that listens immediately — but NOT one that listens only after
	// loading a model. llama-server opens its port after the weights are in,
	// and over RPC on a slower accelerator that takes minutes: the 30s probe
	// expired, the process was marked failed, Restart relaunched it, and it
	// crash-looped forever without ever being given time to finish loading.
	// Callers that launch a model server should set this generously.
	ReadyTimeout time.Duration

	// Restart, when true, makes the supervisor re-launch the process on
	// abnormal exit (anything other than an explicit Stop). Sharding uses
	// this so a single rpc-server dying mid-stream doesn't take a whole
	// model offline.
	Restart bool
	// MaxRestarts caps how many times we'll retry before giving up and
	// marking the process "crashloop". 0 → unlimited (not recommended).
	// Default applied by Start: 5.
	MaxRestarts int
	// RestartBackoff is the initial backoff between restarts; doubles on
	// each consecutive failure up to a 30s cap. Default 1s.
	RestartBackoff time.Duration
}

// ProcessInfo is the observable state of a managed process.
type ProcessInfo struct {
	ID        string    `json:"id"`
	Command   string    `json:"command"`
	Args      []string  `json:"args"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	// Status: starting | running | stopped | failed | crashloop.
	// "crashloop" means Restart was enabled but MaxRestarts was exceeded.
	Status   string `json:"status"`
	ExitErr  string `json:"exit_err,omitempty"`
	Address  string `json:"address,omitempty"`
	Restarts int    `json:"restarts,omitempty"` // count of automatic restarts so far
}

type Process struct {
	Info   ProcessInfo
	spec   ProcessSpec
	cmd    *exec.Cmd
	cancel context.CancelFunc
	logBuf *ringBuffer
	mu     sync.RWMutex

	// stopping is set by Stop() under mu. Every relaunch path checks it,
	// so an explicit Stop always wins over restart-on-crash — including a
	// Stop that lands during the restart backoff window.
	stopping bool
	// generation increments under mu at every (re)launch. Signaling and
	// exited-channel waits always operate on a snapshot taken under mu, so
	// a stale generation (an old, possibly OS-reused PID) is never touched.
	generation int
	// exited is the current generation's exit notification: closed by that
	// generation's reaper goroutine — the ONLY caller of cmd.Wait(), ever —
	// once the child has been collected. exitErr is the Wait error the
	// reaper recorded for the current generation.
	exited  chan struct{}
	exitErr error
	// readyAt is when the current generation passed its readiness probe;
	// used for the stableUptime restart-counter reset.
	readyAt time.Time
}

// Supervisor manages a set of child processes by id.
type Supervisor struct {
	mu    sync.RWMutex
	procs map[string]*Process
	log   *slog.Logger
}

func NewSupervisor(log *slog.Logger) *Supervisor {
	if log == nil {
		log = slog.Default()
	}
	return &Supervisor{
		procs: make(map[string]*Process),
		log:   log,
	}
}

// StartAsync spawns a managed process and returns as soon as it is REGISTERED —
// it does not wait for the readiness probe.
//
// Start blocks until the process is healthy, which is right for an in-process
// caller but wrong across HTTP: a shard coordinator's ReadyTimeout is 15 minutes
// (a real cross-RPC model load), while the leader's client gives up in 60s. The
// leader would then report a failed create for a process that was loading
// perfectly well — and worse, the cancelled request context would tear the
// process down. Callers over HTTP use this and then poll Get() until the status
// leaves "starting".
//
// Errors returned here are registration errors only (bad spec, duplicate ID).
// A spawn or readiness failure lands asynchronously on the process record as
// status "failed" with ExitErr — which carries the engine's own output — and the
// record is KEPT so a poller can read it.
func (s *Supervisor) StartAsync(spec ProcessSpec) (*ProcessInfo, error) {
	p, err := s.register(spec)
	if err != nil {
		return nil, err
	}
	go func() {
		// context.Background(), never a request context: the caller's HTTP
		// deadline must not kill a process that is still legitimately loading.
		if err := s.launchProc(context.Background(), p); err != nil {
			// Keep the record (unlike Start, which deletes it) so the poller can
			// see WHY it failed instead of "process not found".
			s.markFailed(p, err)
		}
	}()
	return s.snapshot(p), nil
}

// register validates a spec, applies defaults, and inserts the process record.
// Shared by Start and StartAsync so both agree on defaults and on the
// duplicate-ID boundary.
func (s *Supervisor) register(spec ProcessSpec) (*Process, error) {
	if spec.ID == "" {
		return nil, fmt.Errorf("ProcessSpec.ID required")
	}
	if spec.Command == "" {
		return nil, fmt.Errorf("ProcessSpec.Command required")
	}
	if spec.LogLines <= 0 {
		spec.LogLines = 200
	}
	if spec.HealthHost == "" {
		spec.HealthHost = "127.0.0.1"
	}
	if spec.Restart && spec.MaxRestarts == 0 {
		spec.MaxRestarts = 5
	}
	if spec.Restart && spec.RestartBackoff == 0 {
		spec.RestartBackoff = time.Second
	}

	addr := ""
	if spec.HealthPort > 0 {
		addr = net.JoinHostPort(spec.HealthHost, strconv.Itoa(spec.HealthPort))
	}

	p := &Process{
		Info: ProcessInfo{
			ID:        spec.ID,
			Command:   spec.Command,
			Args:      spec.Args,
			StartedAt: time.Now(),
			Status:    "starting",
			Address:   addr,
		},
		spec:   spec,
		logBuf: newRingBuffer(spec.LogLines),
	}

	// Exists-check and insert under ONE lock acquisition: two concurrent
	// Starts with the same ID must not both pass the check and silently
	// overwrite each other's *Process (the loser's child would leak,
	// untracked).
	s.mu.Lock()
	if _, ok := s.procs[spec.ID]; ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("process %q already exists", spec.ID)
	}
	s.procs[spec.ID] = p
	s.mu.Unlock()
	return p, nil
}

// Start launches a new managed process. Returns once the process has either
// reached "running" (PID + optional health probe pass) or "failed".
func (s *Supervisor) Start(ctx context.Context, spec ProcessSpec) (*ProcessInfo, error) {
	p, err := s.register(spec)
	if err != nil {
		return nil, err
	}
	if err := s.launchProc(ctx, p); err != nil {
		s.mu.Lock()
		delete(s.procs, p.spec.ID)
		s.mu.Unlock()
		return &p.Info, err
	}
	return s.snapshot(p), nil
}

// launchProc starts the process once; called by Start and (when Restart is
// enabled) by the restart logic in superviseExit. Assumes the *Process is
// already registered in s.procs and holds the ring buffer.
func (s *Supervisor) launchProc(ctx context.Context, p *Process) error {
	spec := p.spec
	procCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, spec.Command, spec.Args...)
	// Put the child in its own process group so Stop() can signal the
	// whole group, killing any grandchildren the process forked.
	applyProcessGroup(cmd)
	// On Linux: also ask the kernel to SIGTERM the child if we (the
	// supervisor) die abnormally. No-op on macOS.
	applyParentDeathSignal(cmd)
	if spec.WorkDir != "" {
		cmd.Dir = spec.WorkDir
	}
	env := os.Environ()
	for k, v := range spec.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("stderr pipe: %w", err)
	}

	go readLines(stdout, p.logBuf, spec.ID)
	go readLines(stderr, p.logBuf, spec.ID)

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start %s: %w", filepath.Base(spec.Command), err)
	}

	p.mu.Lock()
	if p.stopping {
		// Stop() won a race while we were starting this generation: it set
		// stopping before we could publish the new cmd, so it had nothing
		// to signal. Kill the fresh child ourselves and reap it so it
		// can't zombie.
		p.mu.Unlock()
		if err := signalGroup(cmd.Process.Pid, syscall.SIGKILL); err != nil {
			_ = cmd.Process.Kill()
		}
		go func() { _ = cmd.Wait() }()
		cancel()
		return fmt.Errorf("process %q stopped during launch", spec.ID)
	}
	p.generation++
	gen := p.generation
	exited := make(chan struct{})
	p.cmd = cmd
	p.cancel = cancel
	p.exited = exited
	p.exitErr = nil
	p.readyAt = time.Time{}
	p.Info.PID = cmd.Process.Pid
	p.Info.StartedAt = time.Now()
	p.Info.ExitErr = ""
	p.Info.Status = "starting"
	p.mu.Unlock()

	// Reaper: starts immediately and unconditionally, and is the ONLY
	// caller of cmd.Wait() — exec.Cmd.Wait is not concurrency-safe, and a
	// child that dies before (or during) the readiness probe still gets
	// collected here, so no zombies pile up on failed launches.
	go func() {
		waitErr := cmd.Wait()
		p.mu.Lock()
		if p.generation == gen {
			p.exitErr = waitErr
		}
		p.mu.Unlock()
		close(exited)
	}()

	if spec.HealthPort > 0 {
		readyTimeout := spec.ReadyTimeout
		if readyTimeout <= 0 {
			readyTimeout = 30 * time.Second
		}
		if err := waitReadyPath(ctx, spec.HealthHost, spec.HealthPort, spec.HealthPath, readyTimeout, exited); err != nil {
			// Attach what the process actually SAID. Without this the caller gets a
			// bare "process did not become ready" and the real cause — llama.cpp's
			// own error on stderr — dies with the container, so a failure surfaces
			// as an unactionable 502 upstream. The ring buffer already has it.
			s.markFailed(p, fmt.Errorf("health probe: %w%s", err, logTail(s.Logs(spec.ID, 20))))
			return fmt.Errorf("process did not become ready: %w%s", err, logTail(s.Logs(spec.ID, 20)))
		}
	}

	p.mu.Lock()
	p.Info.Status = "running"
	p.readyAt = time.Now()
	p.mu.Unlock()

	go s.superviseExit(p, gen, exited)

	s.log.Info("process started", "id", spec.ID, "pid", p.Info.PID, "command", spec.Command, "addr", p.Info.Address, "restarts", p.Info.Restarts)
	return nil
}

// Stop sends SIGTERM, waits up to 10s, then SIGKILLs. It never calls
// cmd.Wait itself — the per-generation reaper owns that — and never signals
// a generation that has already been reaped (the PID may have been reused
// by the OS).
func (s *Supervisor) Stop(id string) error {
	s.mu.RLock()
	p, ok := s.procs[id]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("process %q not found", id)
	}

	// Mark the intent first, then snapshot the CURRENT generation under
	// p.mu. Anything launched after this point sees stopping=true and
	// kills itself (see launchProc); the restart logic re-checks stopping
	// before every relaunch, so after this block no new generation can
	// outlive us.
	p.mu.Lock()
	p.stopping = true
	cmd := p.cmd
	exited := p.exited
	cancel := p.cancel
	pid := 0
	alive := false
	if cmd != nil && cmd.Process != nil {
		pid = cmd.Process.Pid
		select {
		case <-exited:
			// Already reaped (e.g. crashed and sitting in restart backoff,
			// or in "crashloop"). Nothing to signal — and signaling would
			// risk hitting a recycled PID.
		default:
			alive = true
		}
	}
	p.mu.Unlock()

	if alive {
		// Signal the process group rather than just the leader so any
		// grandchildren the engine forked (download helpers, worker threads
		// the engine wraps in subprocesses) terminate too. Fall back to a
		// per-pid signal if the group signal fails (e.g. group already gone).
		if err := signalGroup(pid, syscall.SIGTERM); err != nil {
			_ = cmd.Process.Signal(os.Interrupt)
		}
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			if err := signalGroup(pid, syscall.SIGKILL); err != nil {
				_ = cmd.Process.Kill()
			}
		}
	}

	p.mu.Lock()
	p.Info.Status = "stopped"
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	s.mu.Lock()
	delete(s.procs, id)
	s.mu.Unlock()

	s.log.Info("process stopped", "id", id, "pid", pid)
	return nil
}

// StopAll terminates every managed process; used on agent shutdown.
func (s *Supervisor) StopAll() {
	s.mu.RLock()
	ids := make([]string, 0, len(s.procs))
	for id := range s.procs {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	for _, id := range ids {
		_ = s.Stop(id)
	}
}

// Get returns a snapshot of one process's info.
func (s *Supervisor) Get(id string) (*ProcessInfo, bool) {
	s.mu.RLock()
	p, ok := s.procs[id]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return s.snapshot(p), true
}

// List returns snapshots of all managed processes.
func (s *Supervisor) List() []*ProcessInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*ProcessInfo, 0, len(s.procs))
	for _, p := range s.procs {
		out = append(out, s.snapshot(p))
	}
	return out
}

// logTail renders captured process output for embedding in an error, so a
// startup failure carries its own explanation instead of forcing the operator to
// go dig for a log that the container teardown has already destroyed.
func logTail(lines []string) string {
	var kept []string
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			kept = append(kept, l)
		}
	}
	if len(kept) == 0 {
		return " (process produced no output)"
	}
	return "\n--- process output (last " + strconv.Itoa(len(kept)) + " lines) ---\n" +
		strings.Join(kept, "\n")
}

// Logs returns the most recent log lines from the given process (stderr +
// stdout interleaved).
func (s *Supervisor) Logs(id string, lines int) []string {
	s.mu.RLock()
	p, ok := s.procs[id]
	s.mu.RUnlock()
	if !ok {
		return nil
	}
	return p.logBuf.tail(lines)
}

func (s *Supervisor) snapshot(p *Process) *ProcessInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	info := p.Info
	return &info
}

func (s *Supervisor) markFailed(p *Process, err error) {
	p.mu.Lock()
	p.Info.Status = "failed"
	if err != nil {
		p.Info.ExitErr = err.Error()
	}
	// p.cancel is only assigned after cmd.Start() succeeds (launchProc). A
	// launch that fails earlier (pipe error, or the engine binary missing so
	// cmd.Start() itself errors) reaches here with a nil cancel — calling it
	// would nil-panic and crash the whole agent, defeating the "keep the record
	// so a poller can read ExitErr" contract. Guard it.
	cancel := p.cancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// superviseExit waits for one generation to be reaped, then decides what
// happens next: an explicit Stop wins (no restart, Stop owns logging and
// map removal); otherwise crash accounting + backoff + relaunch, up to the
// crashloop cap. Restart decisions live here — never in Stop — so there is
// exactly one writer for the restart state.
func (s *Supervisor) superviseExit(p *Process, gen int, exited <-chan struct{}) {
	<-exited

	p.mu.Lock()
	if p.generation != gen {
		// A newer generation took over. Shouldn't happen (relaunch only
		// occurs after this function returns), but guard against acting on
		// stale state anyway.
		p.mu.Unlock()
		return
	}
	if p.stopping {
		p.Info.Status = "stopped"
		p.mu.Unlock()
		return // we initiated this — no restart
	}
	// A generation that stayed up long enough proves the process can run:
	// don't let crashes from months ago count toward today's crashloop cap.
	if !p.readyAt.IsZero() && time.Since(p.readyAt) >= stableUptime {
		p.Info.Restarts = 0
	}
	exitErr := p.exitErr
	if exitErr != nil {
		p.Info.Status = "failed"
		// Carry the engine's own output on the ExitErr. A process that started
		// cleanly and died LATER (llama-server aborting mid-load across its rpc
		// backends: "signal: aborted (core dumped)") tells you only that it died
		// — the assertion that killed it is in the ring buffer, and the container
		// teardown is about to destroy it. Attach it while it still exists.
		p.Info.ExitErr = exitErr.Error() + logTail(p.logBuf.tail(20))
	} else {
		p.Info.Status = "stopped"
	}
	status := p.Info.Status
	prevPID := p.Info.PID
	restartEnabled := p.spec.Restart
	restarts := p.Info.Restarts
	maxRestarts := p.spec.MaxRestarts
	backoff := p.spec.RestartBackoff
	p.mu.Unlock()

	s.log.Info("process exited", "id", p.spec.ID, "pid", prevPID, "status", status, "err", exitErr, "restarts", restarts)

	if !restartEnabled {
		return
	}
	if maxRestarts > 0 && restarts >= maxRestarts {
		p.mu.Lock()
		p.Info.Status = "crashloop"
		p.mu.Unlock()
		// Log the engine's output here too: a crashloop is the one state an
		// operator MUST be able to act on, and "exceeded MaxRestarts" alone says
		// nothing about why.
		s.log.Error("process exceeded MaxRestarts — entering crashloop",
			"id", p.spec.ID, "restarts", restarts, "max", maxRestarts,
			"last_error", exitErr, "output", logTail(p.logBuf.tail(20)))
		return
	}

	// Exponential backoff up to 30s. Each consecutive failure doubles.
	wait := backoff
	for i := 0; i < restarts; i++ {
		wait *= 2
		if wait > 30*time.Second {
			wait = 30 * time.Second
			break
		}
	}
	s.log.Warn("process exited abnormally — restarting", "id", p.spec.ID, "wait", wait, "attempt", restarts+1)
	time.Sleep(wait)

	// Was the process Stopped during the backoff sleep? The child is
	// already reaped, so Stop signaled nothing — this check (plus the one
	// inside launchProc) is what makes Stop win without ever touching a
	// possibly-reused PID.
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		return
	}
	p.Info.Restarts = restarts + 1
	p.mu.Unlock()

	// Re-launch. launchProc spawns a new reaper + superviseExit on success.
	if err := s.launchProc(context.Background(), p); err != nil {
		p.mu.Lock()
		stopped := p.stopping
		p.mu.Unlock()
		if stopped {
			return // Stop raced the relaunch and won; nothing to report
		}
		s.log.Error("restart failed", "id", p.spec.ID, "err", err)
		// superviseExit recurses via launchProc's goroutine, so a re-launch
		// failure here means we don't get another chance until external action.
		p.mu.Lock()
		p.Info.Status = "failed"
		p.Info.ExitErr = "restart failed: " + err.Error()
		p.mu.Unlock()
	}
}

// waitReadyPath probes TCP, and — when path is non-empty — keeps going until an
// HTTP GET of that path returns 2xx. See ProcessSpec.HealthPath: an open socket
// is not a servable model.
func waitReadyPath(ctx context.Context, host string, port int, path string, timeout time.Duration, exited <-chan struct{}) error {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			// The dial may have hit a listener that isn't our child (e.g.
			// something stale on the port). If our child is already dead,
			// this was a false positive.
			select {
			case <-exited:
				return fmt.Errorf("process exited during startup")
			default:
			}
			if path == "" {
				return nil
			}
			// Socket is up; now insist the server can actually answer.
			resp, herr := client.Get("http://" + addr + path)
			if herr == nil {
				code := resp.StatusCode
				_ = resp.Body.Close()
				if code >= 200 && code < 300 {
					return nil
				}
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s%s did not become healthy after %s (socket open, but the server never answered 200 — still loading or wedged)", addr, path, timeout)
			}
			select {
			case <-exited:
				return fmt.Errorf("process exited during startup")
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not reachable after %s", addr, timeout)
		}
		select {
		case <-exited:
			return fmt.Errorf("process exited during startup")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ---- ring buffer for log lines ----

type ringBuffer struct {
	mu   sync.Mutex
	buf  []string
	pos  int
	full bool
	cap  int
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{cap: capacity, buf: make([]string, capacity)}
}

func (r *ringBuffer) append(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.pos] = line
	r.pos = (r.pos + 1) % r.cap
	if r.pos == 0 {
		r.full = true
	}
}

func (r *ringBuffer) tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full && r.pos == 0 {
		return nil
	}
	size := r.pos
	if r.full {
		size = r.cap
	}
	if n <= 0 || n > size {
		n = size
	}
	out := make([]string, 0, n)
	for i := size - n; i < size; i++ {
		idx := (r.pos - size + i + r.cap) % r.cap
		out = append(out, r.buf[idx])
	}
	return out
}

// readLines captures a child's output into its ring buffer AND echoes it to the
// supervisor's own stdout, tagged with the process id.
//
// The echo matters more than it looks. Without it the engine's output existed only
// inside this process — invisible to `docker logs`, invisible to anyone operating
// the cluster — so a worker spending twenty minutes compiling GPU kernels was
// indistinguishable from one that had hung. The operator could see THAT we were
// waiting, never WHAT for. An engine that talks to nobody may as well be silent.
func readLines(rc io.ReadCloser, dst *ringBuffer, id string) {
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		dst.append(line)
		fmt.Printf("[%s] %s\n", id, line)
	}
}
