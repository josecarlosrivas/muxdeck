// Package mushrun makes muxdeck a viewer and starter of mush runs rather than
// their host. mush owns the run: a ledger row (`mush runs --json`), a
// replayable journal (.mush/runs/<id>.jsonl) and, for a checkout a `mush
// serve` owns, the queue and the approval broker behind serve.sock. muxdeck
// reads what mush records and hands new work to mush's own front door, so
// runs survive daemon restarts and one worker per checkout is guaranteed.
//
// Two transports answer approvals. A served run is answered over the socket
// (`approve {run_id, approved}`). A run started where nothing serves is
// spawned here as `mush stdio -approve ask` — mush still writes the row and
// the journal — and muxdeck stays the stdin responder for its lifetime.
package mushrun

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// maxFrameBytes matches the protocol's frame cap.
const maxFrameBytes = 16 << 20

// startTimeout bounds how long a spawned engine may take to announce its run
// id (run_queued) before the start is reported as failed.
const startTimeout = 20 * time.Second

type envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Row is a ledger row as `mush runs --json` prints it, plus muxdeck's view of
// who answers its approvals.
type Row struct {
	ID         string    `json:"id"`
	Project    string    `json:"project"`
	Source     string    `json:"source"`
	SourceRef  string    `json:"source_ref"`
	State      string    `json:"state"`
	Model      string    `json:"model"`
	Provider   string    `json:"provider"`
	Approve    string    `json:"approve"`
	Autopilot  string    `json:"autopilot"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Steps      int       `json:"steps"`
	TokensIn   int       `json:"tokens_in"`
	TokensOut  int       `json:"tokens_out"`
	CostUSD    float64   `json:"cost_usd"`
	Verdict    string    `json:"verdict"`
	Status     string    `json:"status"`
	Branch     string    `json:"branch"`
	PRURL      string    `json:"pr_url"`
	Commit     string    `json:"commit"`
	Journal    string    `json:"journal"`
	Error      string    `json:"error"`
	ParentID   string    `json:"parent_id"`
	// Local: this daemon spawned the engine and answers its approvals over
	// stdin. Served runs are answered over serve.sock instead.
	Local bool `json:"local"`
}

// Terminal reports whether the row's state can no longer change on its own.
func (r Row) Terminal() bool {
	switch r.State {
	case "done", "failed", "interrupted", "blocked", "merged", "pr_open":
		return true
	}
	return false
}

// JournalPath resolves the row's journal; mush records it relative to the
// project when it can.
func (r Row) JournalPath() string {
	if r.Journal == "" || filepath.IsAbs(r.Journal) {
		return r.Journal
	}
	return filepath.Join(r.Project, r.Journal)
}

// ServeProject is one served checkout's live view (serve.sock `status`).
type ServeProject struct {
	Project  string   `json:"project"`
	Current  string   `json:"current,omitempty"`
	Queued   int      `json:"queued"`
	Queue    []string `json:"queue,omitempty"`
	Awaiting []string `json:"awaiting,omitempty"`
}

// ServeView is what the UI learns about `mush serve`: whether one answers the
// socket and which checkouts it owns.
type ServeView struct {
	Live     bool           `json:"live"`
	Socket   string         `json:"socket"`
	Projects []ServeProject `json:"projects"`
	Error    string         `json:"error,omitempty"`
}

type sockRequest struct {
	Op       string `json:"op"`
	Project  string `json:"project,omitempty"`
	Source   string `json:"source,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
	Model    string `json:"model,omitempty"`
	Approve  string `json:"approve,omitempty"`
	Parent   string `json:"parent,omitempty"`
	RunID    string `json:"run_id,omitempty"`
	Approved bool   `json:"approved,omitempty"`
}

type sockResponse struct {
	OK       bool           `json:"ok"`
	Error    string         `json:"error,omitempty"`
	RunID    string         `json:"run_id,omitempty"`
	Projects []ServeProject `json:"projects,omitempty"`
}

// localRun is an engine this daemon spawned: the stdin it answers on and the
// process it must not orphan.
type localRun struct {
	id    string
	dir   string
	task  string
	model string
	ready chan struct{} // closed once id is known
	exit  chan struct{} // closed when the process is gone

	mu     sync.Mutex
	stdin  io.WriteCloser
	proc   *os.Process
	stderr *tail
	gone   bool
}

type Manager struct {
	bin string

	mu    sync.Mutex
	local map[string]*localRun
}

// New builds a manager around the given engine binary. An empty bin resolves
// "mush" from PATH; Available reports whether that worked.
func New(bin string) *Manager {
	if bin == "" {
		bin, _ = exec.LookPath("mush")
	}
	return &Manager{bin: bin, local: map[string]*localRun{}}
}

func (m *Manager) Available() bool { return m.bin != "" }

// Models reports the model ids offered for run configuration — the
// MUXDECK_MUSH_MODELS line (comma-separated) in mush.env. Purely advisory:
// any model string is accepted at start; this feeds UI completion.
func (m *Manager) Models() []string {
	for _, kv := range engineEnv() {
		if v, ok := strings.CutPrefix(kv, "MUXDECK_MUSH_MODELS="); ok {
			var out []string
			for _, id := range strings.Split(v, ",") {
				if id = strings.TrimSpace(id); id != "" {
					out = append(out, id)
				}
			}
			return out
		}
	}
	return nil
}

// engineEnv reads KEY=VALUE lines from mush.env in the muxdeck config dir —
// provider keys for spawned engines. Daemons run under launchd/systemd with a
// bare environment and no shell rc files, so credentials need a deliberate,
// muxdeck-owned home. Re-read per use: edits apply without a daemon restart.
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

func (m *Manager) command(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command(m.bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), engineEnv()...)
	return cmd
}

// --- ledger ---

// List is `mush runs --json`: newest first, optionally one project's rows.
func (m *Manager) List(project string, limit int) ([]Row, error) {
	if !m.Available() {
		return []Row{}, nil
	}
	if limit <= 0 {
		limit = 50
	}
	args := []string{"runs", "--json", "--limit", strconv.Itoa(limit)}
	if project != "" {
		args = append(args, "--project", project)
	}
	var stderr bytes.Buffer
	cmd := m.command("", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("mush runs: %s", strings.TrimSpace(firstNonEmpty(stderr.String(), err.Error())))
	}
	rows := []Row{}
	if trimmed := bytes.TrimSpace(out); len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &rows); err != nil {
			return nil, fmt.Errorf("mush runs: %v", err)
		}
	}
	m.mu.Lock()
	for i := range rows {
		_, rows[i].Local = m.local[rows[i].ID]
	}
	m.mu.Unlock()
	return rows, nil
}

// Get finds one row. The ledger has no by-id query, so this scans a generous
// window; a run older than that is still reachable through its project.
func (m *Manager) Get(id string) (Row, error) {
	rows, err := m.List("", 500)
	if err != nil {
		return Row{}, err
	}
	for _, r := range rows {
		if r.ID == id {
			return r, nil
		}
	}
	if lr := m.localRun(id); lr != nil {
		// The engine announced the id but the ledger has not caught up (or is
		// unavailable): synthesize enough for a viewer to open.
		return Row{ID: id, Project: lr.dir, Source: "prompt", SourceRef: truncate(lr.task, 80),
			State: "queued", Model: lr.model, Local: true}, nil
	}
	return Row{}, errNotFound
}

var errNotFound = errors.New("no such run")

func IsNotFound(err error) bool { return errors.Is(err, errNotFound) }

// --- journal ---

// Journal reads raw journal frames from byte offset from; next is where a
// tail continues. Checkpoints (engine state blobs, often megabytes) are
// reduced to their step number — viewers never need the payload.
func (m *Manager) Journal(row Row, from int64) (frames [][]byte, next int64, err error) {
	path := row.JournalPath()
	if path == "" {
		return nil, from, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, from, nil
	}
	if err != nil {
		return nil, from, err
	}
	defer f.Close()
	if from > 0 {
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			return nil, from, err
		}
	}
	next = from
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			// A partial last line is a frame mid-write; leave it for the next tail.
			if errors.Is(err, io.EOF) {
				return frames, next, nil
			}
			return frames, next, err
		}
		next += int64(len(line))
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 {
			continue
		}
		var env envelope
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		if env.Type == "checkpoint" {
			var cp struct {
				Step int `json:"step"`
			}
			_ = json.Unmarshal(env.Data, &cp)
			line, _ = json.Marshal(envelope{Type: "checkpoint", Data: mustRaw(cp)})
		}
		frames = append(frames, append([]byte(nil), line...))
	}
}

// Task recovers the run's full prompt from its journal (run_started carries
// it verbatim; the ledger's source_ref is truncated), falling back to the row.
func (m *Manager) Task(row Row) string {
	frames, _, err := m.Journal(row, 0)
	if err == nil {
		for _, f := range frames {
			var env envelope
			if json.Unmarshal(f, &env) != nil {
				continue
			}
			if env.Type == "run_started" {
				var d struct {
					Task string `json:"task"`
				}
				if json.Unmarshal(env.Data, &d) == nil && d.Task != "" {
					return d.Task
				}
			}
		}
	}
	if lr := m.localRun(row.ID); lr != nil && lr.task != "" {
		return lr.task
	}
	return row.SourceRef
}

// --- serve socket ---

// mushHome is $MUSH_HOME, else ~/.mush — where serve.sock and runs.db live.
func mushHome() (string, error) {
	for _, kv := range engineEnv() {
		if v, ok := strings.CutPrefix(kv, "MUSH_HOME="); ok && v != "" {
			return v, nil
		}
	}
	if home := os.Getenv("MUSH_HOME"); home != "" {
		return home, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".mush"), nil
}

// socketPath mirrors mush's own rule: $MUSH_HOME/serve.sock, or — when that
// exceeds the kernel's sun_path limit — a short path under the temp dir keyed
// by a hash of the home.
func socketPath() (string, error) {
	home, err := mushHome()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, "serve.sock")
	if len(path) <= 100 {
		return path, nil
	}
	sum := sha256.Sum256([]byte(home))
	return filepath.Join(os.TempDir(), fmt.Sprintf("mush-%d", os.Getuid()), hex.EncodeToString(sum[:6])+".sock"), nil
}

// roundTrip sends one request to a live serve; live is false when nothing
// answers the socket (no serve, or a socket a dead one left behind).
func roundTrip(req sockRequest) (resp sockResponse, live bool, err error) {
	path, err := socketPath()
	if err != nil {
		return resp, false, err
	}
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) ||
			errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOTSOCK) {
			return resp, false, nil
		}
		return resp, false, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return resp, true, err
	}
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return resp, true, err
	}
	return resp, true, nil
}

// Serve asks serve.sock for its status. A missing serve is a state, not an error.
func (m *Manager) Serve() ServeView {
	v := ServeView{Projects: []ServeProject{}}
	v.Socket, _ = socketPath()
	resp, live, err := roundTrip(sockRequest{Op: "status"})
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.Live = live
	if live && resp.Projects != nil {
		v.Projects = resp.Projects
	}
	return v
}

func (v ServeView) owns(dir string) bool {
	for _, p := range v.Projects {
		if filepath.Clean(p.Project) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

// --- starting runs ---

// Start hands a task to mush in dir. When a serve owns the checkout the run
// is enqueued over the socket (one worker per checkout — it never races a
// schedule or an issue run); otherwise an engine is spawned here. Either way
// the approval policy is ask: remote-started runs never default open (design
// decision 2026-07-18). parent records the run this one retries.
func (m *Manager) Start(task, dir, model, parent string) (Row, error) {
	if !m.Available() {
		return Row{}, errors.New("no mush binary found")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return Row{}, fmt.Errorf("not a directory: %s", dir)
	}
	if strings.TrimSpace(task) == "" {
		return Row{}, errors.New("empty task")
	}
	if serve := m.Serve(); serve.Live && serve.owns(dir) {
		resp, _, err := roundTrip(sockRequest{Op: "enqueue", Project: dir, Source: "prompt",
			Prompt: task, Model: model, Approve: "ask", Parent: parent})
		if err != nil {
			return Row{}, fmt.Errorf("serve: %v", err)
		}
		if !resp.OK {
			return Row{}, fmt.Errorf("serve: %s", resp.Error)
		}
		return m.settle(resp.RunID, Row{ID: resp.RunID, Project: dir, Source: "prompt",
			SourceRef: truncate(task, 80), State: "queued", Model: model, ParentID: parent})
	}
	return m.spawn(task, dir, model)
}

// settle returns the ledger row for a just-created run, tolerating the short
// window before the writer's row is visible.
func (m *Manager) settle(id string, fallback Row) (Row, error) {
	for i := 0; i < 10; i++ {
		if row, err := m.Get(id); err == nil {
			return row, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fallback, nil
}

// spawn runs `mush stdio -approve ask` in dir with task as its only turn.
// mush records the row and journal itself; this daemon keeps stdin so it can
// answer the engine's approval requests, and closes it once the run is done
// so the served session exits instead of idling.
func (m *Manager) spawn(task, dir, model string) (Row, error) {
	args := []string{"stdio", "-approve", "ask"}
	if model != "" {
		args = append(args, "-model", model)
	}
	cmd := m.command(dir, args...)
	stderr := &tail{}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Row{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Row{}, err
	}
	if err := cmd.Start(); err != nil {
		return Row{}, err
	}
	lr := &localRun{dir: dir, task: task, model: model, ready: make(chan struct{}), exit: make(chan struct{}),
		stdin: stdin, proc: cmd.Process, stderr: stderr}

	turn, _ := json.Marshal(envelope{Type: "user_turn", Data: mustRaw(map[string]string{"text": task})})
	if err := lr.write(turn); err != nil {
		cmd.Process.Kill()
		return Row{}, err
	}

	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), maxFrameBytes)
		announced := false
		for sc.Scan() {
			var env envelope
			if json.Unmarshal(sc.Bytes(), &env) != nil {
				continue
			}
			switch env.Type {
			case "run_queued":
				if announced {
					break
				}
				var d struct {
					RunID string `json:"run_id"`
					Model string `json:"model"`
				}
				if json.Unmarshal(env.Data, &d) == nil && d.RunID != "" {
					announced = true
					lr.id = d.RunID
					if d.Model != "" {
						lr.model = d.Model
					}
					m.mu.Lock()
					m.local[d.RunID] = lr
					m.mu.Unlock()
					close(lr.ready)
				}
			case "done":
				// One turn per spawn: EOF tells the served session to exit.
				lr.closeStdin()
			}
		}
		cmd.Wait()
		lr.mu.Lock()
		lr.gone = true
		lr.mu.Unlock()
		close(lr.exit)
		if lr.id != "" {
			m.mu.Lock()
			delete(m.local, lr.id)
			m.mu.Unlock()
		}
	}()

	select {
	case <-lr.ready:
	case <-lr.exit:
		return Row{}, fmt.Errorf("engine exited before starting a run: %s", strings.TrimSpace(stderr.String()))
	case <-time.After(startTimeout):
		cmd.Process.Kill()
		return Row{}, fmt.Errorf("engine did not announce a run within %s: %s", startTimeout, strings.TrimSpace(stderr.String()))
	}
	return m.settle(lr.id, Row{ID: lr.id, Project: dir, Source: "prompt", SourceRef: truncate(task, 80),
		State: "queued", Model: lr.model, Local: true})
}

func (m *Manager) localRun(id string) *localRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.local[id]
}

// Approve answers a run's pending approval: over stdin for an engine this
// daemon spawned, over serve.sock for a served run.
func (m *Manager) Approve(id string, approved bool) error {
	if lr := m.localRun(id); lr != nil {
		frame, _ := json.Marshal(envelope{Type: "approval_response", Data: mustRaw(map[string]bool{"approved": approved})})
		return lr.write(frame)
	}
	resp, live, err := roundTrip(sockRequest{Op: "approve", RunID: id, Approved: approved})
	if err != nil {
		return fmt.Errorf("serve: %v", err)
	}
	if !live {
		return errors.New("nothing is waiting on this run here: no engine of ours and no mush serve listening")
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	return nil
}

// Interrupt stops an engine this daemon spawned: protocol interrupt first,
// SIGTERM shortly after if it is still up, SIGKILL as the backstop. Served
// runs belong to their serve; there is no socket verb for stopping one.
func (m *Manager) Interrupt(id string) error {
	lr := m.localRun(id)
	if lr == nil {
		return errors.New("not an engine of ours: interrupt served runs from the serve host")
	}
	frame, _ := json.Marshal(envelope{Type: "interrupt"})
	_ = lr.write(frame)
	go func() {
		select {
		case <-lr.exit:
			return
		case <-time.After(3 * time.Second):
			lr.proc.Signal(os.Interrupt)
		}
		select {
		case <-lr.exit:
		case <-time.After(7 * time.Second):
			lr.proc.Kill()
		}
	}()
	return nil
}

// Retry starts a fresh run with the same task in the same checkout, recorded
// as a child of the original.
func (m *Manager) Retry(row Row) (Row, error) {
	return m.Start(m.Task(row), row.Project, row.Model, row.ID)
}

// Resume is `mush resume <id>` in the run's checkout: mush restores the last
// checkpoint into a new run with its own journal. Detached — the row appears
// in the ledger on the next list.
func (m *Manager) Resume(row Row) error {
	if !m.Available() {
		return errors.New("no mush binary found")
	}
	if st, err := os.Stat(row.Project); err != nil || !st.IsDir() {
		return fmt.Errorf("project directory is gone: %s", row.Project)
	}
	cmd := m.command(row.Project, "resume", row.ID)
	stderr := &tail{}
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	// A resume that dies at once (no provider key, unreadable journal) is
	// reported here; one that outlives the grace period is a running run.
	select {
	case err := <-exited:
		if err != nil {
			return fmt.Errorf("mush resume: %s", strings.TrimSpace(firstNonEmpty(stderr.String(), err.Error())))
		}
		return nil
	case <-time.After(1500 * time.Millisecond):
		return nil
	}
}

// Shutdown stops every engine this daemon spawned; call on daemon exit so no
// orphans remain. Served runs are unaffected — they were never ours.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	runs := make([]*localRun, 0, len(m.local))
	for _, lr := range m.local {
		runs = append(runs, lr)
	}
	m.mu.Unlock()
	for _, lr := range runs {
		lr.mu.Lock()
		if !lr.gone && lr.proc != nil {
			lr.proc.Kill()
		}
		lr.mu.Unlock()
	}
}

func (lr *localRun) write(frame []byte) error {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if lr.stdin == nil || lr.gone {
		return errors.New("run is not accepting commands")
	}
	_, err := lr.stdin.Write(append(frame, '\n'))
	return err
}

func (lr *localRun) closeStdin() {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if lr.stdin != nil {
		lr.stdin.Close()
		lr.stdin = nil
	}
}

// Wait blocks until the run's engine is gone or ctx ends; it returns
// immediately for runs this daemon did not spawn.
func (m *Manager) Wait(ctx context.Context, id string) {
	lr := m.localRun(id)
	if lr == nil {
		return
	}
	select {
	case <-lr.exit:
	case <-ctx.Done():
	}
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

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
