package mushrun

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mushHomeDir points MUSH_HOME at a short scratch path (serve.sock must fit
// sun_path) and cleans it up.
func mushHomeDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("MUSH_HOME", dir)
	return dir
}

func types(t *testing.T, frames [][]byte) []string {
	t.Helper()
	var out []string
	for _, f := range frames {
		var env struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(f, &env); err != nil {
			t.Fatalf("bad frame %q: %v", f, err)
		}
		out = append(out, env.Type)
	}
	return out
}

func waitJournal(t *testing.T, m *Manager, row Row, want string) [][]byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		frames, _, err := m.Journal(row, 0)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.Join(types(t, frames), ","), want) {
			return frames
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("journal never showed %q", want)
	return nil
}

func TestLocalRunFlow(t *testing.T) {
	mushHomeDir(t)
	m := New(fakeBin)
	dir := t.TempDir()
	task := "do the thing " + strings.Repeat("carefully ", 12)
	row, err := m.Start(task, dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !row.Local || row.ID == "" || row.Project != dir {
		t.Fatalf("row = %+v, want a local run in %s", row, dir)
	}

	frames := waitJournal(t, m, row, "approval_requested")
	got := types(t, frames)
	if got[0] != "run_queued" || got[len(got)-1] != "approval_requested" {
		t.Fatalf("journal = %v", got)
	}
	// Checkpoints are reduced to their step: the state blob never reaches a viewer.
	for _, f := range frames {
		if strings.HasPrefix(string(f), `{"type":"checkpoint"`) && len(f) > 100 {
			t.Fatalf("checkpoint payload leaked: %d bytes", len(f))
		}
	}
	if m.Task(row) != task {
		t.Fatalf("Task = %q, want the full prompt", m.Task(row))
	}

	if err := m.Approve(row.ID, true); err != nil {
		t.Fatal(err)
	}
	frames = waitJournal(t, m, row, "done")
	joined := strings.Join(types(t, frames), ",")
	if !strings.Contains(joined, "approval_resolved") || !strings.Contains(joined, "tool_result") {
		t.Fatalf("journal after approval = %s", joined)
	}

	// The engine exits after its one turn and leaves the ledger behind.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.Wait(ctx, row.ID)
	if m.localRun(row.ID) != nil {
		t.Fatal("engine still registered after exit")
	}
	fresh, err := m.Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.State != "done" || fresh.Local || !fresh.Terminal() {
		t.Fatalf("ledger row after run = %+v", fresh)
	}
	rows, err := m.List(dir, 10)
	if err != nil || len(rows) != 1 || rows[0].ID != row.ID {
		t.Fatalf("List(project) = %+v, %v", rows, err)
	}
	if _, err := m.Get("nope"); !IsNotFound(err) {
		t.Fatalf("Get(unknown) = %v, want not found", err)
	}
}

func TestInterruptLocalRun(t *testing.T) {
	mushHomeDir(t)
	m := New(fakeBin)
	row, err := m.Start("task", t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Interrupt(row.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.Wait(ctx, row.ID)
	if fresh, _ := m.Get(row.ID); fresh.State != "interrupted" {
		t.Fatalf("state after interrupt = %q", fresh.State)
	}
	if err := m.Approve(row.ID, true); err == nil {
		t.Fatal("approve succeeded with no responder anywhere")
	}
}

func TestJournalTailOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "j.jsonl")
	os.WriteFile(path, []byte("{\"type\":\"a\"}\n{\"type\":\"b\"}\n{\"type\":\"c\""), 0o644)
	m := New(fakeBin)
	row := Row{Project: dir, Journal: "j.jsonl"}
	frames, next, err := m.Journal(row, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := types(t, frames); strings.Join(got, ",") != "a,b" {
		t.Fatalf("frames = %v, want the two complete lines", got)
	}
	os.WriteFile(path, []byte("{\"type\":\"a\"}\n{\"type\":\"b\"}\n{\"type\":\"c\"}\n"), 0o644)
	frames, _, err = m.Journal(row, next)
	if err != nil {
		t.Fatal(err)
	}
	if got := types(t, frames); strings.Join(got, ",") != "c" {
		t.Fatalf("tail = %v, want c", got)
	}
}

// fakeServe answers serve.sock like `mush serve`: status owns project, enqueue
// records a queued row, approve resolves once.
func fakeServe(t *testing.T, home, project string) (approved chan bool) {
	t.Helper()
	approved = make(chan bool, 1)
	ln, err := net.Listen("unix", filepath.Join(home, "serve.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var req sockRequest
				if json.NewDecoder(c).Decode(&req) != nil {
					return
				}
				var resp sockResponse
				switch req.Op {
				case "status":
					resp = sockResponse{OK: true, Projects: []ServeProject{{Project: project, Queued: 0}}}
				case "enqueue":
					if req.Approve != "ask" || req.Project != project {
						resp = sockResponse{Error: "unexpected enqueue: " + req.Approve + " " + req.Project}
						break
					}
					id := "20260102T000000Z-served"
					row := map[string]any{"id": id, "project": project, "source": "prompt", "source_ref": req.Prompt,
						"state": "queued", "model": req.Model, "parent_id": req.Parent}
					b, _ := json.Marshal(row)
					os.MkdirAll(filepath.Join(home, "rows"), 0o755)
					os.WriteFile(filepath.Join(home, "rows", id+".json"), b, 0o644)
					resp = sockResponse{OK: true, RunID: id}
				case "approve":
					approved <- req.Approved
					resp = sockResponse{OK: true, RunID: req.RunID}
				}
				json.NewEncoder(c).Encode(resp)
			}()
		}
	}()
	return approved
}

func TestServedRun(t *testing.T) {
	home := mushHomeDir(t)
	project := t.TempDir()
	approved := fakeServe(t, home, project)
	m := New(fakeBin)

	if v := m.Serve(); !v.Live || len(v.Projects) != 1 {
		t.Fatalf("Serve = %+v", v)
	}
	row, err := m.Start("served task", project, "m1", "parent-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.Local || row.ID != "20260102T000000Z-served" || row.ParentID != "parent-1" || row.Model != "m1" {
		t.Fatalf("row = %+v, want the serve's queued row", row)
	}
	if err := m.Approve(row.ID, false); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-approved:
		if v {
			t.Fatal("approve carried the wrong decision")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("approve never reached the socket")
	}
	if err := m.Interrupt(row.ID); err == nil {
		t.Fatal("interrupt of a served run succeeded")
	}
	// A checkout the serve does not own still runs locally.
	other := t.TempDir()
	local, err := m.Start("local task", other, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !local.Local {
		t.Fatalf("row = %+v, want a local run", local)
	}
	m.Shutdown()
}

func TestResumeAndRetry(t *testing.T) {
	mushHomeDir(t)
	m := New(fakeBin)
	dir := t.TempDir()
	row, err := m.Start("first", dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	m.Approve(row.ID, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.Wait(ctx, row.ID)

	if err := m.Resume(row); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if r, err := m.Get(row.ID + "-resumed"); err == nil && r.ParentID == row.ID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("resumed row never appeared")
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Setenv("FAKE_RUN_ID", "20260103T000000Z-retry")
	child, err := m.Retry(row)
	if err != nil {
		t.Fatal(err)
	}
	if child.ID == row.ID || m.Task(child) != "first" {
		t.Fatalf("retry = %+v (task %q)", child, m.Task(child))
	}
	m.Shutdown()
}

func TestStartRejectsBadDir(t *testing.T) {
	mushHomeDir(t)
	m := New(fakeBin)
	if _, err := m.Start("task", "/definitely/not/a/dir", "", ""); err == nil {
		t.Fatal("bad dir accepted")
	}
}

func TestManagerUnavailable(t *testing.T) {
	mushHomeDir(t)
	m := New("/no/such/binary/anywhere")
	if _, err := m.Start("task", t.TempDir(), "", ""); err == nil {
		t.Fatal("start succeeded with missing binary")
	}
	m2 := New("")
	if m2.Available() {
		t.Skip("a real mush is on PATH")
	}
	if rows, err := m2.List("", 5); err != nil || len(rows) != 0 {
		t.Fatalf("List without a binary = %v, %v", rows, err)
	}
}
