//go:build darwin

package ports

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// macOS has no procfs, and reading the process table through libproc would
// mean cgo in a binary that is otherwise pure Go and cross-compiled for four
// platforms — so both halves shell out.

// parents reads the pid -> ppid edge of every live process from ps.
func parents() (map[int]int, error) {
	out, err := run("ps", "-axo", "pid=,ppid=")
	if err != nil {
		return nil, err
	}
	return parsePS(out), nil
}

// listening asks lsof which of pids hold listening TCP sockets. One call for
// the whole set: lsof is the expensive part of this package on macOS, and the
// pid list is a session's descendants, not the machine's process table.
func listening(pids []int) (map[int][]int, error) {
	if len(pids) == 0 {
		return nil, nil
	}
	list := make([]string, len(pids))
	for i, pid := range pids {
		list[i] = strconv.Itoa(pid)
	}
	// lsof exits non-zero when nothing matches, which is the ordinary case
	// for a session that serves nothing, so the output is parsed regardless
	// of the exit status.
	out, _ := run("lsof", "-nP", "-a", "-p", strings.Join(list, ","),
		"-iTCP", "-sTCP:LISTEN", "-Fpn")
	return parseLsof(out), nil
}

// procTimeout bounds the shell-outs. This runs on a background refresh whose
// completion clears the cache's refreshing flag, so a command that never
// returns — lsof blocked on a hung network mount is the classic — would
// freeze every port chip until the daemon restarts.
const procTimeout = 3 * time.Second

func run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), procTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// Killing the command on a cancelled context does not by itself unblock
	// the read: anything it spawned inherits the output pipe and holds it
	// open. WaitDelay forces the pipe shut shortly after.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	return string(out), err
}
