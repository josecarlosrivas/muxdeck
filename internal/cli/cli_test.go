package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/josecarlosrivas/muxdeck/internal/agent"
)

func TestSelected(t *testing.T) {
	for _, args := range [][]string{{"ls"}, {"frobnicate"}, {"status", "get", "x"}} {
		if !Selected(args) {
			t.Errorf("%v: got false, want true", args)
		}
	}
	// The daemon takes only flags, so an unknown *command* must reach the CLI
	// and be refused — never fall through and start a second daemon.
	for _, args := range [][]string{{}, {"-addr", ":9000"}, {"-version"}} {
		if Selected(args) {
			t.Errorf("%v: got true, want false", args)
		}
	}
}

// Go's flag package stops at the first positional, so a command that reads
// naturally has to resume parsing after each one.
func TestParseMixed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	note := fs.String("note", "", "")
	verbose := fs.Bool("v", false, "")
	pos, err := parse(fs, mixed, []string{"set", "build", "-note", "hi", "working", "-v"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"set", "build", "working"}; !reflect.DeepEqual(pos, want) {
		t.Errorf("positional: got %v, want %v", pos, want)
	}
	if *note != "hi" || !*verbose {
		t.Errorf("flags: got note=%q v=%v, want hi true", *note, *verbose)
	}
}

// A message or a payload that starts with "-" is a message, not a mistake.
func TestParseLeading(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	noEnter := fs.Bool("no-enter", false, "")
	pos, err := parse(fs, leading, []string{"-no-enter", "build", "-n", "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"build", "-n", "hello"}; !reflect.DeepEqual(pos, want) {
		t.Errorf("positional: got %v, want %v", pos, want)
	}
	if !*noEnter {
		t.Error("flag before the positionals was not parsed")
	}
}

func TestParseChip(t *testing.T) {
	for in, want := range map[string]agent.Chip{
		"ctx=38%":                {Key: "ctx", Value: "38%"},
		"flask:tests=12/14":      {Key: "tests", Value: "12/14", Icon: "flask"},
		"warn:ctx=38%":           {Key: "ctx", Value: "38%", Color: "warn"},
		"flask:warn:tests=1":     {Key: "tests", Value: "1", Icon: "flask", Color: "warn"},
		"warn:flask:tests=1":     {Key: "tests", Value: "1", Icon: "flask", Color: "warn"},
		"eta=3:20":               {Key: "eta", Value: "3:20"},
		"clock:eta=3:20 to warn": {Key: "eta", Value: "3:20 to warn", Icon: "clock"},
		// A key that happens to be an icon name still parses as the key: the
		// segment carrying "=" can only be the key and value.
		"check=on": {Key: "check", Value: "on"},
	} {
		got, err := parseChip(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q: got %#v, want %#v", in, got, want)
		}
	}
	for _, in := range []string{
		"", "novalue", "=novalue", "key=", "nope:k=v", "flask:flask:k=v", "flask:k",
	} {
		if got, err := parseChip(in); err == nil {
			t.Errorf("%q: got %#v, want an error", in, got)
		}
	}
}

// --- command tests against a stub daemon ---

type stub struct {
	*httptest.Server
	sessions []listEntry
	posted   []map[string]any
	sent     []map[string]any
	doctor   any
}

func newStub(t *testing.T, sessions ...listEntry) *stub {
	t.Helper()
	s := &stub{sessions: sessions}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(s.sessions)
	})
	mux.HandleFunc("POST /api/agent/status", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		s.posted = append(s.posted, body)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/doctor", func(w http.ResponseWriter, r *http.Request) {
		if s.doctor == nil {
			s.doctor = map[string]any{"supported": false}
		}
		json.NewEncoder(w).Encode(s.doctor)
	})
	mux.HandleFunc("POST /api/sessions/{name}/send", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		body["session"] = r.PathValue("name")
		s.sent = append(s.sent, body)
		w.WriteHeader(http.StatusNoContent)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	// Via the environment rather than -url, so the argument lists under test
	// are the ones a person would actually type.
	t.Setenv("MUXDECK_URL", s.URL)
	return s
}

func (s *stub) run(t *testing.T, args ...string) (string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	e := &env{out: &out, err: &errOut}
	cmd, ok := commands[args[0]]
	if !ok {
		t.Fatalf("no such command %q", args[0])
	}
	args = args[1:]
	code := 0
	if err := cmd(e, args); err != nil {
		code = 1
	}
	return out.String(), code
}

func TestLS(t *testing.T) {
	s := newStub(t,
		listEntry{Name: "build", Windows: 2, Path: "/w/app", Branch: "main", Dirty: true,
			Ports: []int{3000, 8080},
			Agent: &agent.Status{Agent: "demo", State: "working", Model: "M",
				Progress: &agent.Progress{Value: 0.62}, Chips: []agent.Chip{{Key: "t", Value: "9/9"}}}},
		listEntry{Name: "logs", Windows: 1},
	)
	out, code := s.run(t, "ls")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, want := range []string{"build", "main*", "3000,8080", "working M 62% 9/9", "logs", "-"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestLSJSONIsVerbatim(t *testing.T) {
	s := newStub(t, listEntry{Name: "build", Windows: 1})
	out, code := s.run(t, "ls", "-json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var decoded []map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(decoded) != 1 || decoded[0]["name"] != "build" {
		t.Errorf("got %v", decoded)
	}
}

func TestStatusSetSendsChipsAndProgress(t *testing.T) {
	s := newStub(t)
	if _, code := s.run(t, "status", "set", "build", "working",
		"-agent", "demo", "-progress", "0.5", "-label", "tests",
		"-chip", "flask:tests=6/12"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := s.posted[0]
	if got["session"] != "build" || got["agent"] != "demo" || got["state"] != "working" {
		t.Errorf("got %v", got)
	}
	if p, _ := got["progress"].(map[string]any); p["value"] != 0.5 || p["label"] != "tests" {
		t.Errorf("progress: got %v", got["progress"])
	}
	chips, _ := got["chips"].([]any)
	if len(chips) != 1 {
		t.Fatalf("chips: got %v", got["chips"])
	}
	if c, _ := chips[0].(map[string]any); c["key"] != "tests" || c["icon"] != "flask" {
		t.Errorf("chip: got %v", chips[0])
	}
}

// Absent means unchanged to the daemon, so a plain state report must not
// carry a progress or chip field at all.
func TestStatusSetOmitsWhatWasNotAsked(t *testing.T) {
	s := newStub(t)
	s.run(t, "status", "set", "build", "idle")
	got := s.posted[0]
	for _, key := range []string{"progress", "chips", "model", "note"} {
		if _, present := got[key]; present {
			t.Errorf("got %q in %v, want it omitted", key, got)
		}
	}
}

func TestStatusSetClear(t *testing.T) {
	s := newStub(t)
	s.run(t, "status", "set", "build", "idle", "-clear", "-progress", "0.9", "-chip", "k=v")
	got := s.posted[0]
	if p, _ := got["progress"].(map[string]any); len(p) != 1 || p["value"] != float64(0) {
		t.Errorf("progress: got %v, want a zero object", got["progress"])
	}
	if chips, _ := got["chips"].([]any); len(chips) != 0 {
		t.Errorf("chips: got %v, want empty", got["chips"])
	}
}

// Notifying under a fresh name would blank the model and spend the daemon
// carries forward per reporter, so notify adopts whoever is already there.
func TestNotifyAdoptsTheExistingReporter(t *testing.T) {
	s := newStub(t, listEntry{Name: "build", Agent: &agent.Status{Agent: "claude-code", State: "working"}})
	if _, code := s.run(t, "notify", "build", "needs", "your", "permission"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := s.posted[0]
	if got["agent"] != "claude-code" {
		t.Errorf("agent: got %v, want claude-code", got["agent"])
	}
	if got["state"] != agent.StateWaiting {
		t.Errorf("state: got %v, want waiting", got["state"])
	}
	if got["note"] != "needs your permission" {
		t.Errorf("note: got %v", got["note"])
	}
}

func TestNotifyFallsBackWhenNobodyIsReporting(t *testing.T) {
	s := newStub(t, listEntry{Name: "build"})
	s.run(t, "notify", "build", "hello")
	if s.posted[0]["agent"] != "cli" {
		t.Errorf("agent: got %v, want cli", s.posted[0]["agent"])
	}
}

func TestSend(t *testing.T) {
	s := newStub(t)
	if _, code := s.run(t, "send", "build", "echo", "hi"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := s.sent[0]
	if got["session"] != "build" || got["text"] != "echo hi" || got["enter"] != true {
		t.Errorf("got %v", got)
	}

	s.run(t, "send", "-no-enter", "build", "partial")
	if got := s.sent[1]; got["enter"] != false || got["text"] != "partial" {
		t.Errorf("got %v", got)
	}

	// No text at all is a bare Enter, for a prompt waiting on one.
	s.run(t, "send", "build")
	if got := s.sent[2]; got["text"] != "" || got["enter"] != true {
		t.Errorf("got %v", got)
	}
}

func TestUsageErrors(t *testing.T) {
	s := newStub(t)
	for name, args := range map[string][]string{
		"ls with args":      {"ls", "extra"},
		"status no verb":    {"status"},
		"status bad verb":   {"status", "reset", "build"},
		"set missing state": {"status", "set", "build"},
		"set bad state":     {"status", "set", "build", "pondering"},
		"set bad name":      {"status", "set", "has space", "idle"},
		"set bad progress":  {"status", "set", "build", "idle", "-progress", "5"},
		"notify no message": {"notify", "build"},
		"send no session":   {"send"},
	} {
		if _, code := s.run(t, args...); code == 0 {
			t.Errorf("%s: got exit 0, want failure", name)
		}
	}
}

func TestDoctorBlocked(t *testing.T) {
	s := newStub(t)
	s.doctor = map[string]any{
		"supported": true,
		"dirs": []map[string]string{
			{"name": "Desktop", "path": "/Users/x/Desktop", "status": "ok"},
			{"name": "Documents", "path": "/Users/x/Documents", "status": "pending"},
			{"name": "Downloads", "path": "/Users/x/Downloads", "status": "blocked"},
		},
		"hits":        []string{"/Users/x/Downloads"},
		"settingsCmd": `open "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles"`,
	}
	out, code := s.run(t, "doctor")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, want := range []string{
		"Desktop", "ok", "blocked", "consent prompt",
		"/Users/x/Downloads", "Full Disk Access", "Privacy_AllFiles",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorUnsupported(t *testing.T) {
	s := newStub(t)
	out, code := s.run(t, "doctor")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "not on macOS") {
		t.Errorf("output missing platform note:\n%s", out)
	}
}
