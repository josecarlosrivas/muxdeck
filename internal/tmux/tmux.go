// Package tmux wraps the tmux CLI for session management.
package tmux

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ErrBadName is returned when a session name fails validation.
var ErrBadName = errors.New("session name must match [A-Za-z0-9_-]{1,64}")

type Session struct {
	Name     string    `json:"name"`
	Windows  int       `json:"windows"`
	Created  time.Time `json:"created"`
	Attached int       `json:"attached"`
}

func ValidName(name string) bool { return nameRe.MatchString(name) }

func run(args ...string) error {
	out, err := exec.Command("tmux", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux %s: %s", args[0], strings.TrimSpace(string(out)))
	}
	return nil
}

// List returns all sessions on the default tmux server. A missing server
// (tmux not started yet) is reported as an empty list, not an error.
func List() ([]Session, error) {
	out, err := exec.Command("tmux", "list-sessions", "-F",
		"#{session_name}\t#{session_windows}\t#{session_created}\t#{session_attached}").CombinedOutput()
	if err != nil {
		msg := string(out)
		if strings.Contains(msg, "no server running") || strings.Contains(msg, "error connecting to") {
			return []Session{}, nil
		}
		return nil, fmt.Errorf("tmux list-sessions: %s", strings.TrimSpace(msg))
	}
	sessions := []Session{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			continue
		}
		windows, _ := strconv.Atoi(f[1])
		created, _ := strconv.ParseInt(f[2], 10, 64)
		attached, _ := strconv.Atoi(f[3])
		sessions = append(sessions, Session{Name: f[0], Windows: windows, Created: time.Unix(created, 0), Attached: attached})
	}
	return sessions, nil
}

func New(name string) error {
	if !ValidName(name) {
		return ErrBadName
	}
	return run("new-session", "-d", "-s", name)
}

func Kill(name string) error {
	if !ValidName(name) {
		return ErrBadName
	}
	return run("kill-session", "-t", "="+name)
}

func Rename(oldName, newName string) error {
	if !ValidName(oldName) || !ValidName(newName) {
		return ErrBadName
	}
	return run("rename-session", "-t", "="+oldName, newName)
}

func Has(name string) bool {
	if !ValidName(name) {
		return false
	}
	return exec.Command("tmux", "has-session", "-t", "="+name).Run() == nil
}
