package mushrun

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func waitState(t *testing.T, r *Run, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r.Info().State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state = %q, want %q", r.Info().State, want)
}

func frames(t *testing.T, replay [][]byte) []string {
	t.Helper()
	var types []string
	for _, f := range replay {
		var env struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(f, &env); err != nil {
			t.Fatalf("bad frame %q: %v", f, err)
		}
		types = append(types, env.Type)
	}
	return types
}

func TestRunApprovalFlow(t *testing.T) {
	m := New(fakeBin)
	run, err := m.Start("do the thing", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, run, "awaiting_approval")

	replay, live, _ := run.Subscribe()
	if got := frames(t, replay); got[len(got)-1] != "approval_requested" {
		t.Fatalf("replay tail = %v, want approval_requested last", got)
	}

	if err := run.Command([]byte(`{"type":"approval_response","data":{"approved":true}}`)); err != nil {
		t.Fatal(err)
	}
	var liveTypes []string
	for f := range live {
		var env struct {
			Type string `json:"type"`
		}
		json.Unmarshal(f, &env)
		liveTypes = append(liveTypes, env.Type)
	}
	joined := strings.Join(liveTypes, ",")
	if !strings.Contains(joined, "tool_result") || !strings.Contains(joined, "done") || !strings.Contains(joined, "_exit") {
		t.Fatalf("live stream = %v, want tool_result, done, _exit", liveTypes)
	}
	waitState(t, run, "done")

	// Late subscriber to a finished run gets the full replay and no channel.
	replay2, live2, _ := run.Subscribe()
	if live2 != nil {
		t.Fatal("done run returned a live channel")
	}
	if got := frames(t, replay2); got[len(got)-1] != "_exit" {
		t.Fatalf("final replay tail = %v, want _exit last", got)
	}
}

func TestRunRejectsNonCommands(t *testing.T) {
	m := New(fakeBin)
	run, err := m.Start("task", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer run.Stop()
	if err := run.Command([]byte(`{"type":"done","data":{}}`)); err == nil {
		t.Fatal("event tag accepted as command")
	}
	if err := run.Command([]byte(`not json`)); err == nil {
		t.Fatal("garbage accepted as command")
	}
}

func TestStartRejectsBadDir(t *testing.T) {
	m := New(fakeBin)
	if _, err := m.Start("task", "/definitely/not/a/dir"); err == nil {
		t.Fatal("bad dir accepted")
	}
}

func TestManagerUnavailable(t *testing.T) {
	m := New("/no/such/binary/anywhere")
	// LookPath is skipped for explicit paths; Start should still fail cleanly.
	if _, err := m.Start("task", t.TempDir()); err == nil {
		t.Fatal("start succeeded with missing binary")
	}
}
