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
// standard install locations before giving up. Found=false lets the API
// report "tmux missing" as a state instead of a cryptic exec error.
// MUXDECK_TMUX overrides the search entirely.
var Bin, Found = findTmux()

func findTmux() (string, bool) {
	if p := os.Getenv("MUXDECK_TMUX"); p != "" {
		_, err := os.Stat(p)
		return p, err == nil
	}
	if p, err := exec.LookPath("tmux"); err == nil {
		return p, true
	}
	for _, p := range []string{
		"/opt/homebrew/bin/tmux",
		"/usr/local/bin/tmux",
		"/opt/local/bin/tmux",
	} {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "tmux", false
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
	Command  string    `json:"command,omitempty"`
	Path     string    `json:"path,omitempty"`
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
	if ctx := contexts(); ctx != nil {
		for i, sess := range sessions {
			if c, ok := ctx[sess.Name]; ok {
				sessions[i].Command, sessions[i].Path = c.Command, c.Path
			}
		}
	}
	return sessions, nil
}

// paneContext is the active pane's command and working directory, the two facts
// that say what a session is actually doing.
type paneContext struct{ Command, Path string }

// contexts reads that pair for every session, keyed by session name.
//
// Pane formats resolve against the session's own active pane here, so unlike
// display-message this works from the daemon with no attached client.
//
// Three free-text fields cannot share a "|"-separated line — a path may
// legitimately contain one — so the two leading fields are byte lengths and
// the payload is sliced by count instead of split. #{n:...} needs a tmux new
// enough to have it, which is why this is a second call rather than extra
// fields on the List format: the session list is load-bearing and must not
// break to decorate a sidebar. A nil map simply means no decoration.
func contexts() map[string]paneContext {
	out, err := exec.Command(Bin, "list-sessions", "-F",
		"#{n:pane_current_command}|#{n:pane_current_path}|#{pane_current_command}#{pane_current_path}#{session_name}").Output()
	if err != nil {
		return nil
	}
	return parseContexts(string(out))
}

func parseContexts(out string) map[string]paneContext {
	ctx := map[string]paneContext{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.SplitN(line, "|", 3)
		if len(f) != 3 {
			continue
		}
		nCmd, errCmd := strconv.Atoi(f[0])
		nPath, errPath := strconv.Atoi(f[1])
		if errCmd != nil || errPath != nil || nCmd < 0 || nPath < 0 || nCmd+nPath > len(f[2]) {
			continue
		}
		if name := f[2][nCmd+nPath:]; name != "" {
			ctx[name] = paneContext{Command: f[2][:nCmd], Path: f[2][nCmd : nCmd+nPath]}
		}
	}
	return ctx
}

// PanePIDs lists the process id of every pane, grouped by session — the roots
// of the process trees each session owns.
//
// Numeric field first and the free-text session name last, split once, for the
// same reason as the session list: a name may legitimately contain the
// separator, and a name that shifts fields would attribute one session's
// processes to another.
func PanePIDs() map[string][]int {
	out, err := exec.Command(Bin, "list-panes", "-a", "-F", "#{pane_pid}|#{session_name}").Output()
	if err != nil {
		return nil
	}
	return parsePanePIDs(string(out))
}

func parsePanePIDs(out string) map[string][]int {
	panes := map[string][]int{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		pidStr, name, ok := strings.Cut(line, "|")
		if !ok || name == "" {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil || pid <= 0 {
			continue
		}
		panes[name] = append(panes[name], pid)
	}
	if len(panes) == 0 {
		return nil
	}
	return panes
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
	args := []string{"new-session", "-d", "-s", name}
	// Without -c the session inherits the tmux server's cwd, which is "/"
	// when the daemon was started by the desktop app or launchd.
	if home, err := os.UserHomeDir(); err == nil {
		args = append(args, "-c", home)
	}
	if err := run(args...); err != nil {
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

// SendKeys types text into a session's active pane, then optionally Enter.
//
// -l sends the text literally, so a payload that happens to name a tmux key
// ("Enter", "C-c") arrives as those characters instead of as that key —
// which is also why submitting is a separate send and a parameter, rather
// than something a caller can smuggle into the text. "--" is required, not
// decorative: without it a text starting with "-" is parsed as a flag and
// send-keys fails.
//
// The pane target is "=name:" rather than "=name": the "=" keeps tmux from
// prefix-matching a different session, and the trailing ":" is what makes it
// resolve to that session's current pane at all.
func SendKeys(name, text string, enter bool) error {
	if !ValidName(name) {
		return ErrBadName
	}
	target := "=" + name + ":"
	if text != "" {
		if err := run("send-keys", "-t", target, "-l", "--", text); err != nil {
			return err
		}
	}
	if enter {
		return run("send-keys", "-t", target, "Enter")
	}
	return nil
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

// Cwd reports the current working directory of the session's active pane.
// list-panes rather than display-message: the latter expands pane formats
// against the attached client's context and comes back empty when the
// caller isn't a tmux client — exactly the daemon case (seen on tmux 3.6a).
func Cwd(name string) (string, error) {
	if !ValidName(name) {
		return "", ErrBadName
	}
	out, err := exec.Command(Bin, "list-panes", "-t", "="+name,
		"-F", "#{pane_active}|#{pane_current_path}").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux list-panes: %s", strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if path, ok := strings.CutPrefix(line, "1|"); ok && path != "" {
			return path, nil
		}
	}
	return "", errors.New("could not resolve session cwd")
}
