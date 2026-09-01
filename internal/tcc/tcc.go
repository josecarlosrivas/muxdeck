// Package tcc detects macOS folder-privacy (TCC) denials, so the operator
// can be told which protected folders the daemon cannot read — and how to
// grant access — instead of watching an ls hang from an iPad.
//
// Detection is passive: the daemon never reads a protected folder on its own
// initiative, because reading one whose access is undecided makes macOS
// raise a consent prompt on the Mac's screen — which nobody is looking at
// when the whole point of muxdeck is being somewhere else — and parks the
// read behind it. Denials are recorded where the daemon organically trips
// over them; the probe behind `muxdeck doctor` is the one deliberate
// exception, run when the operator asks for it.
package tcc

import (
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// SettingsCmd opens System Settings at the Full Disk Access pane, the one
// grant that covers every protected folder at once. Run on the Mac itself.
const SettingsCmd = `open "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles"`

// protected names the per-folder TCC domains the probe checks, relative to
// the daemon's home.
var protected = []string{"Desktop", "Documents", "Downloads"}

// Denied reports whether err is a TCC denial. TCC refuses with EPERM
// ("operation not permitted"), which is not EACCES: ordinary unix permission
// problems must not be diagnosed as privacy settings. Errors that arrive as
// captured command output (git's, mainly) are matched by that same text.
func Denied(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "operation not permitted")
}

// hits is what the daemon has organically been denied on, capped so an
// adversarial run of paths cannot grow it without bound.
var hits struct {
	sync.Mutex
	paths map[string]bool
}

const maxHits = 16

// Note records a TCC denial at path, when err is one. Free to call on every
// error path that touches a session's directory; anything that is not a
// denial on macOS is ignored.
func Note(path string, err error) {
	if supported {
		note(path, err)
	}
}

// note is Note without the platform gate, split out so its behavior has
// tests that run on every builder rather than only on a Mac.
func note(path string, err error) {
	if path == "" || !Denied(err) {
		return
	}
	hits.Lock()
	defer hits.Unlock()
	if hits.paths == nil {
		hits.paths = map[string]bool{}
	}
	if len(hits.paths) < maxHits {
		hits.paths[filepath.Clean(path)] = true
	}
}

// Dir is one protected folder's probe result.
type Dir struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// ok, blocked, pending (a consent prompt is waiting on the Mac's
	// screen), absent, or error (unreadable, but not a privacy denial).
	Status string `json:"status"`
}

// Report is the /api/doctor response shape.
type Report struct {
	Supported   bool     `json:"supported"`
	Dirs        []Dir    `json:"dirs,omitempty"`
	Hits        []string `json:"hits"`
	SettingsCmd string   `json:"settingsCmd,omitempty"`
}

// Status returns what is known passively — and, when probe is set, what an
// active read of the protected folders finds. Only the passive form is safe
// to poll.
func Status(probe bool) Report {
	r := Report{Supported: supported, Hits: []string{}}
	if !supported {
		return r
	}
	r.SettingsCmd = SettingsCmd
	if probe {
		r.Dirs = probeDirs()
	}
	hits.Lock()
	for p := range hits.paths {
		r.Hits = append(r.Hits, p)
	}
	hits.Unlock()
	sort.Strings(r.Hits)
	return r
}
