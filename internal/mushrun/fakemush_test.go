package mushrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fakeMush stands in for the mush binary: enough of `stdio`, `runs --json`
// and `resume` to exercise the manager. Rows live as JSON files under
// $MUSH_HOME/rows; journals go to .mush/runs/<id>.jsonl in the cwd, as mush
// writes them. Built once per test binary run.
const fakeMushSrc = `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type env struct {
	Type string          ` + "`json:\"type\"`" + `
	Data json.RawMessage ` + "`json:\"data,omitempty\"`" + `
}

var journal *os.File

func emit(t, data string) {
	line := fmt.Sprintf("{\"type\":%q,\"data\":%s}", t, data)
	if data == "" {
		line = fmt.Sprintf("{\"type\":%q}", t)
	}
	fmt.Println(line)
	if journal != nil {
		fmt.Fprintln(journal, line)
		journal.Sync()
	}
}

func rowsDir() string { return filepath.Join(os.Getenv("MUSH_HOME"), "rows") }

func writeRow(row map[string]any) {
	os.MkdirAll(rowsDir(), 0o755)
	b, _ := json.Marshal(row)
	os.WriteFile(filepath.Join(rowsDir(), row["id"].(string)+".json"), b, 0o644)
}

func runs(args []string) {
	project := ""
	for i, a := range args {
		if a == "--project" && i+1 < len(args) {
			project = args[i+1]
		}
	}
	entries, _ := os.ReadDir(rowsDir())
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	out := []map[string]any{}
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(rowsDir(), n))
		if err != nil {
			continue
		}
		var row map[string]any
		if json.Unmarshal(b, &row) != nil {
			continue
		}
		if project != "" && row["project"] != project {
			continue
		}
		out = append(out, row)
	}
	json.NewEncoder(os.Stdout).Encode(out)
}

func stdio(args []string) {
	model := "fake"
	for i, a := range args {
		if a == "-model" && i+1 < len(args) {
			model = args[i+1]
		}
	}
	cwd, _ := os.Getwd()
	id := os.Getenv("FAKE_RUN_ID")
	if id == "" {
		id = "20260101T000000Z-abc123"
	}
	os.MkdirAll(filepath.Join(cwd, ".mush", "runs"), 0o755)
	journal, _ = os.Create(filepath.Join(cwd, ".mush", "runs", id+".jsonl"))
	fmt.Fprintln(os.Stderr, "mush stdio: recording run", id)
	row := map[string]any{"id": id, "project": cwd, "source": "prompt", "state": "queued",
		"model": model, "journal": filepath.Join(".mush", "runs", id+".jsonl")}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var e env
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Type {
		case "user_turn":
			var d struct{ Text string ` + "`json:\"text\"`" + ` }
			json.Unmarshal(e.Data, &d)
			row["source_ref"] = d.Text
			if len(d.Text) > 80 {
				row["source_ref"] = d.Text[:79] + "…"
			}
			writeRow(row)
			emit("run_queued", fmt.Sprintf("{\"run_id\":%q,\"project\":%q,\"source\":\"prompt\",\"model\":%q}", id, cwd, model))
			emit("run_started", fmt.Sprintf("{\"task\":%q,\"model\":%q}", d.Text, model))
			emit("checkpoint", "{\"step\":1,\"state\":\""+strings.Repeat("x", 4096)+"\"}")
			row["state"] = "implementing"
			writeRow(row)
			emit("run_state_changed", "{\"state\":\"implementing\"}")
			emit("step_started", "{\"step\":1,\"model\":\""+model+"\"}")
			emit("assistant_text", "{\"delta\":\"working\"}")
			emit("tool_called", "{\"name\":\"write_file\",\"args\":{\"path\":\"x\"}}")
			emit("approval_requested", "{\"name\":\"write_file\",\"args\":{\"path\":\"x\"},\"risk\":\"write\"}")
			row["state"] = "awaiting_approval"
			writeRow(row)
		case "approval_response":
			var d struct{ Approved bool ` + "`json:\"approved\"`" + ` }
			json.Unmarshal(e.Data, &d)
			emit("approval_resolved", fmt.Sprintf("{\"approved\":%t,\"reason\":\"\"}", d.Approved))
			if d.Approved {
				emit("tool_result", "{\"name\":\"write_file\",\"content\":\"ok\"}")
			} else {
				emit("tool_result", "{\"name\":\"write_file\",\"content\":\"denied\",\"is_error\":true}")
			}
			emit("done", "{\"steps\":1,\"text\":\"finished\"}")
			row["state"] = "done"
			writeRow(row)
		case "interrupt":
			row["state"] = "interrupted"
			writeRow(row)
			os.Exit(0)
		}
	}
}

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "runs":
		runs(os.Args[2:])
	case "stdio":
		stdio(os.Args[2:])
	case "resume":
		cwd, _ := os.Getwd()
		writeRow(map[string]any{"id": os.Args[2] + "-resumed", "project": cwd, "source": "prompt",
			"state": "done", "parent_id": os.Args[2]})
	default:
		fmt.Fprintln(os.Stderr, "fake mush: unknown command", os.Args[1])
		os.Exit(2)
	}
}
`

var fakeBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakemush")
	if err != nil {
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(fakeMushSrc), 0o644); err != nil {
		os.Exit(1)
	}
	fakeBin = filepath.Join(dir, "fakemush")
	if out, err := exec.Command("go", "build", "-o", fakeBin, src).CombinedOutput(); err != nil {
		os.Stderr.Write(out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
