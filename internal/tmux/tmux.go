// Package tmux wraps the tmux CLI for session management.
package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Bin is the resolved tmux binary. GUI-launched processes (the desktop
// app's sidecar, launchd without a PATH override) inherit a PATH without
// Homebrew or MacPorts, so a bare "tmux" fails on most Macs — probe the
// standard install locations before giving up.
var Bin = findTmux()

func findTmux() string {
	if p, err := exec.LookPath("tmux"); err == nil {
		return p
	}
	for _, p := range []string{
		"/opt/homebrew/bin/tmux",
		"/usr/local/bin/tmux",
		"/opt/local/bin/tmux",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "tmux" // let exec surface the not-found error
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ErrBadName is returned when a session name fails validation.
var ErrBadName = errors.New("session name must match [A-Za-z0-9_-]{1,64}")

type Session struct {
	Name     string    `json:"name"`
	Windows  int       `json:"windows"`
	Created  time.Time `json:"created"`
	Attached int       `json:"attached"`
	Activity int64     `json:"activity"`
}

func ValidName(name string) bool { return nameRe.MatchString(name) }

func run(args ...string) error {
	out, err := exec.Command(Bin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux %s: %s", args[0], strings.TrimSpace(string(out)))
	}
	return nil
}

// List returns all sessions on the default tmux server. A missing server
// (tmux not started yet) is reported as an empty list, not an error.
//
// The format uses "|" separators, numeric fields first and the free-text name
// last (split with a count, so a "|" inside a session name stays in the name).
// TAB is not safe here: tmux ≥3.6 sanitizes control characters to "_" when the
// client runs outside tmux — exactly the daemon case.
func List() ([]Session, error) {
	out, err := exec.Command(Bin, "list-sessions", "-F",
		"#{session_windows}|#{session_created}|#{session_attached}|#{session_activity}|#{session_name}").CombinedOutput()
	if err != nil {
		msg := string(out)
		if strings.Contains(msg, "no server running") || strings.Contains(msg, "error connecting to") {
			return []Session{}, nil
		}
		return nil, fmt.Errorf("tmux list-sessions: %s", strings.TrimSpace(msg))
	}
	sessions := []Session{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.SplitN(line, "|", 5)
		if len(f) != 5 {
			continue
		}
		windows, _ := strconv.Atoi(f[0])
		created, _ := strconv.ParseInt(f[1], 10, 64)
		attached, _ := strconv.Atoi(f[2])
		activity, _ := strconv.ParseInt(f[3], 10, 64)
		sessions = append(sessions, Session{Name: f[4], Windows: windows, Created: time.Unix(created, 0), Attached: attached, Activity: activity})
	}
	return sessions, nil
}

// EnsureClipboard makes tmux forward copy-mode yanks to the attached client
// as OSC 52, which the web frontend turns into a browser clipboard write.
// set-clipboard only takes effect when the outer terminal advertises the Ms
// capability, which xterm-256color terminfo does not — hence the override.
func EnsureClipboard() error {
	if err := run("set-option", "-s", "set-clipboard", "on"); err != nil {
		return err
	}
	out, err := exec.Command(Bin, "show-options", "-s", "-v", "terminal-overrides").CombinedOutput()
	if err == nil && strings.Contains(string(out), "Ms=") {
		return nil
	}
	return run("set-option", "-s", "-a", "terminal-overrides", `,xterm*:Ms=\E]52;%p1%s;%p2%s\007`)
}

func New(name string) error {
	if !ValidName(name) {
		return ErrBadName
	}
	if err := run("new-session", "-d", "-s", name); err != nil {
		return err
	}
	// Web-first default: wheel/touch scrolling only reaches tmux (instead of
	// the browser page) when the application requests mouse tracking.
	// Option commands reject the "=" exact-match prefix; the session was just
	// created under this validated name, so a plain target is safe here.
	return run("set-option", "-t", name, "mouse", "on")
}

// Mouse reports the effective mouse option for a session (session value,
// falling back to the global value when unset).
func Mouse(name string) (bool, error) {
	if !Has(name) {
		return false, fmt.Errorf("no such session: %s", name)
	}
	out, err := exec.Command(Bin, "show-options", "-t", name, "-v", "mouse").CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("tmux show-options: %s", strings.TrimSpace(string(out)))
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		out, err = exec.Command(Bin, "show-options", "-g", "-v", "mouse").CombinedOutput()
		if err != nil {
			return false, fmt.Errorf("tmux show-options -g: %s", strings.TrimSpace(string(out)))
		}
		v = strings.TrimSpace(string(out))
	}
	return v == "on", nil
}

func SetMouse(name string, on bool) error {
	if !Has(name) {
		return fmt.Errorf("no such session: %s", name)
	}
	v := "off"
	if on {
		v = "on"
	}
	return run("set-option", "-t", name, "mouse", v)
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
	return exec.Command(Bin, "has-session", "-t", "="+name).Run() == nil
}
