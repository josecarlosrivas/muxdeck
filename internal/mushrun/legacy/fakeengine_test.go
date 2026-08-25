package legacy

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fakeEngine is a stand-in mush binary: it speaks just enough of the protocol
// to exercise the manager — emits a scripted run with an approval pause, and
// honors the answer. Built once per test binary run.
const fakeEngineSrc = `package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

type env struct {
	Type string          ` + "`json:\"type\"`" + `
	Data json.RawMessage ` + "`json:\"data,omitempty\"`" + `
}

func emit(t, data string) {
	if data == "" {
		fmt.Printf("{\"type\":%q}\n", t)
		return
	}
	fmt.Printf("{\"type\":%q,\"data\":%s}\n", t, data)
}

func main() {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var e env
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Type {
		case "user_turn":
			emit("run_started", "{\"task\":\"t\"}")
			emit("step_started", "{\"step\":1,\"model\":\"fake\"}")
			emit("assistant_text", "{\"delta\":\"working\"}")
			emit("tool_called", "{\"name\":\"write_file\",\"args\":{\"path\":\"x\"}}")
			emit("approval_requested", "{\"name\":\"write_file\",\"args\":{\"path\":\"x\"},\"risk\":\"write\"}")
		case "approval_response":
			var d struct{ Approved bool ` + "`json:\"approved\"`" + ` }
			json.Unmarshal(e.Data, &d)
			if d.Approved {
				emit("tool_result", "{\"name\":\"write_file\",\"content\":\"ok\"}")
			} else {
				emit("tool_result", "{\"name\":\"write_file\",\"content\":\"denied\",\"is_error\":true}")
			}
			emit("done", "{\"steps\":1,\"text\":\"finished\"}")
			return
		case "interrupt":
			os.Exit(0)
		}
	}
}
`

var fakeBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakeengine")
	if err != nil {
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(fakeEngineSrc), 0o644); err != nil {
		os.Exit(1)
	}
	fakeBin = filepath.Join(dir, "fakeengine")
	if out, err := exec.Command("go", "build", "-o", fakeBin, src).CombinedOutput(); err != nil {
		os.Stderr.Write(out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
