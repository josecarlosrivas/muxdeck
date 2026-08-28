package tmux

import "testing"

// The length-prefixed payload exists so that separators and multi-byte
// characters inside a path or a session name cannot shift the fields, and so
// that a tmux which does not understand #{n:...} degrades to no context
// rather than to wrong context.
func TestParseContexts(t *testing.T) {
	got := parseContexts(`6|21|claude/home/agent/workspacemain
4|23|bash/tmp/pipe|in|path/a diredge case
4|15|bash/tmp/ünïcødeutf8
`)
	want := map[string]paneContext{
		"main":      {Command: "claude", Path: "/home/agent/workspace"},
		"edge case": {Command: "bash", Path: "/tmp/pipe|in|path/a dir"},
		"utf8":      {Command: "bash", Path: "/tmp/ünïcøde"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d sessions, want %d: %#v", len(got), len(want), got)
	}
	for name, w := range want {
		if g := got[name]; g != w {
			t.Errorf("%q: got %#v, want %#v", name, g, w)
		}
	}
}

func TestParseContextsRejectsMalformed(t *testing.T) {
	for name, out := range map[string]string{
		"no lengths":      "claude|/tmp|main",
		"length overruns": "99|99|bash/tmpmain",
		"negative length": "-1|4|bash/tmpmain",
		"missing name":    "4|4|bash/tmp",
		"too few fields":  "4|bash/tmpmain",
		"empty":           "",
	} {
		if got := parseContexts(out); len(got) != 0 {
			t.Errorf("%s: got %#v, want no sessions", name, got)
		}
	}
}
