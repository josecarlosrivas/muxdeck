// Package mushrun manages mush engine runs: spawn `mush stdio` per run, stream
// its NDJSON protocol events to subscribers, and forward client commands
// (approvals, follow-up turns, interrupt) back to the engine. muxdeck is a
// protocol client, not a library consumer — the envelope shapes are the only
// contract (mush docs/PROTOCOL.md), so this works with any engine binary that
// speaks them.
package mushrun

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// maxFrameBytes matches the protocol's frame cap.
const maxFrameBytes = 16 << 20

// maxBufferBytes bounds the per-run replay buffer. Oldest frames drop first; a
// late subscriber sees a trimmed head rather than unbounded memory use.
const maxBufferBytes = 4 << 20

type envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Info is the API-facing view of a run.
type Info struct {
	ID        string    `json:"id"`
	Task      string    `json:"task"`
	Dir       string    `json:"dir"`
	State     string    `json:"state"` // running | awaiting_approval | done | failed | interrupted
	Steps     int       `json:"steps"`
	StartedAt time.Time `json:"started_at"`
}

type subscriber chan []byte

type Run struct {
	id        string
	task      string
	dir       string
	startedAt time.Time

	mu       sync.Mutex
	state    string
	steps    int
	buf      [][]byte
	bufBytes int
	trimmed  bool
	subs     map[subscriber]bool
	stdin    io.WriteCloser
	proc     *os.Process
	stopping bool
}

type Manager struct {
	bin string

	mu   sync.Mutex
	runs map[string]*Run
	seq  int
}

// New builds a manager around the given engine binary. An empty bin resolves
// "mush" from PATH; Available reports whether that worked.
func New(bin string) *Manager {
	if bin == "" {
		bin, _ = exec.LookPath("mush")
	}
	return &Manager{bin: bin, runs: map[string]*Run{}}
}

func (m *Manager) Available() bool { return m.bin != "" }

// Start spawns an engine in dir and submits task as the first turn. The run's
// approval policy is ask: every write pauses for an explicit decision from a
// viewer (design decision 2026-07-18 — remote-started runs never default open).
func (m *Manager) Start(task, dir string) (*Run, error) {
	if !m.Available() {
		return nil, errors.New("no mush binary found")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", dir)
	}

	cmd := exec.Command(m.bin, "stdio", "-approve", "ask")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), engineEnv()...)
	// The engine's stderr is its only diagnostic channel — a missing provider
	// key, a bad settings file — so keep the tail and surface it when the run
	// fails, instead of a bare "run failed".
	stderr := &tail{}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("r%d", m.seq)
	run := &Run{
		id: id, task: task, dir: dir, startedAt: time.Now(),
		state: "running", subs: map[subscriber]bool{},
		stdin: stdin, proc: cmd.Process,
	}
	m.runs[id] = run
	m.mu.Unlock()

	turn, _ := json.Marshal(envelope{Type: "user_turn", Data: mustRaw(map[string]string{"text": task})})
	run.writeFrame(turn)

	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), maxFrameBytes)
		for sc.Scan() {
			frame := append([]byte(nil), sc.Bytes()...)
			run.ingest(frame)
		}
		cmd.Wait()
		run.finish(stderr.String())
	}()
	return run, nil
}

// tail keeps the last chunk of a stream — enough of the engine's stderr to
// explain a failure without holding a whole session's diagnostics.
type tail struct {
	mu sync.Mutex
	b  []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 2048 {
		t.b = t.b[len(t.b)-2048:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

func mustRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// engineEnv reads KEY=VALUE lines from mush.env in the muxdeck config dir —
// provider keys for spawned engines. Daemons run under launchd/systemd with a
// bare environment and no shell rc files, so credentials need a deliberate,
// muxdeck-owned home. Re-read per start: edits apply without a daemon restart.
func engineEnv() []string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "muxdeck", "mush.env"))
	if err != nil {
		return nil
	}
	var env []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		env = append(env, line)
	}
	return env
}

// ingest buffers a frame, updates the run's state machine, and fans out.
func (r *Run) ingest(frame []byte) {
	var env envelope
	if json.Unmarshal(frame, &env) != nil {
		return
	}
	r.mu.Lock()
	switch env.Type {
	case "approval_requested":
		r.state = "awaiting_approval"
	case "step_started":
		r.state = "running"
		var d struct {
			Step int `json:"step"`
		}
		if json.Unmarshal(env.Data, &d) == nil {
			r.steps = d.Step
		}
	case "done":
		r.state = "done"
	default:
		if r.state == "awaiting_approval" {
			r.state = "running"
		}
	}
	r.buf = append(r.buf, frame)
	r.bufBytes += len(frame)
	for r.bufBytes > maxBufferBytes && len(r.buf) > 1 {
		r.bufBytes -= len(r.buf[0])
		r.buf = r.buf[1:]
		r.trimmed = true
	}
	for sub := range r.subs {
		select {
		case sub <- frame:
		default: // a stalled viewer never blocks the engine stream
		}
	}
	r.mu.Unlock()
}

// finish marks the run's terminal state after the engine exits and closes all
// subscriber channels. A synthetic _exit frame tells viewers why the stream
// ended; it is muxdeck-local, not a protocol event. stderr rides along on
// failure so the viewer sees the engine's own explanation.
func (r *Run) finish(stderr string) {
	r.mu.Lock()
	switch {
	case r.state == "done":
	case r.stopping:
		r.state = "interrupted"
	default:
		r.state = "failed"
	}
	data := map[string]string{"state": r.state}
	if r.state == "failed" && stderr != "" {
		data["error"] = stderr
	}
	exit, _ := json.Marshal(envelope{Type: "_exit", Data: mustRaw(data)})
	r.buf = append(r.buf, exit)
	r.bufBytes += len(exit)
	for sub := range r.subs {
		select {
		case sub <- exit:
		default:
		}
		close(sub)
	}
	r.subs = map[subscriber]bool{}
	r.mu.Unlock()
}

func (r *Run) writeFrame(b []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stdin == nil {
		return errors.New("run is not accepting commands")
	}
	if _, err := r.stdin.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// Command validates and forwards a client command frame to the engine.
func (r *Run) Command(frame []byte) error {
	if len(frame) > maxFrameBytes {
		return errors.New("frame too large")
	}
	var env envelope
	if err := json.Unmarshal(frame, &env); err != nil {
		return err
	}
	switch env.Type {
	case "user_turn", "approval_response", "interrupt":
	default:
		return fmt.Errorf("not a protocol command: %q", env.Type)
	}
	return r.writeFrame(frame)
}

// Subscribe returns the replay of everything so far plus a live channel. The
// channel is closed when the run ends; a done run returns a nil channel.
func (r *Run) Subscribe() (replay [][]byte, live <-chan []byte, trimmed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	replay = append([][]byte(nil), r.buf...)
	if r.terminalLocked() {
		return replay, nil, r.trimmed
	}
	sub := make(subscriber, 256)
	r.subs[sub] = true
	return replay, sub, r.trimmed
}

func (r *Run) Unsubscribe(live <-chan []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for sub := range r.subs {
		if (<-chan []byte)(sub) == live {
			delete(r.subs, sub)
			return
		}
	}
}

func (r *Run) terminalLocked() bool {
	return r.state == "done" || r.state == "failed" || r.state == "interrupted"
}

func (r *Run) Info() Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Info{ID: r.id, Task: r.task, Dir: r.dir, State: r.state, Steps: r.steps, StartedAt: r.startedAt}
}

// Stop interrupts the run: protocol interrupt first, SIGTERM shortly after if
// the engine is still up, SIGKILL as the backstop.
func (r *Run) Stop() {
	r.mu.Lock()
	r.stopping = true
	proc := r.proc
	r.mu.Unlock()
	frame, _ := json.Marshal(envelope{Type: "interrupt"})
	r.Command(frame)
	if proc == nil {
		return
	}
	go func() {
		time.Sleep(3 * time.Second)
		proc.Signal(os.Interrupt)
		time.Sleep(7 * time.Second)
		proc.Kill()
	}()
}

func (m *Manager) Get(id string) (*Run, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	return r, ok
}

func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.runs))
	for _, r := range m.runs {
		out = append(out, r.Info())
	}
	return out
}

// Shutdown stops every live engine; call on daemon exit so no orphans remain.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	runs := make([]*Run, 0, len(m.runs))
	for _, r := range m.runs {
		runs = append(runs, r)
	}
	m.mu.Unlock()
	for _, r := range runs {
		r.mu.Lock()
		terminal := r.terminalLocked()
		proc := r.proc
		r.mu.Unlock()
		if !terminal && proc != nil {
			proc.Kill()
		}
	}
}
