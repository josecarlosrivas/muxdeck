package ports

import (
	"reflect"
	"testing"
)

// A dev server is rarely the pane process itself — it sits under a shell,
// often under a process manager under that — so attribution has to reach the
// whole subtree and stop at the boundary between two sessions.
func TestAttributeClaimsDescendants(t *testing.T) {
	parent := map[int]int{
		100: 1, 101: 100, 102: 101, // build: pane -> shell -> npm
		200: 1, 201: 200, // logs: pane -> tail
		300: 1, // another user's tmux, no session of ours
	}
	got := attribute(map[string][]int{"build": {100}, "logs": {200}}, parent)
	want := map[int]string{100: "build", 101: "build", 102: "build", 200: "logs", 201: "logs"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestAttributeMultiplePanes(t *testing.T) {
	parent := map[int]int{10: 1, 11: 10, 20: 1, 21: 20}
	got := attribute(map[string][]int{"work": {10, 20}}, parent)
	want := map[int]string{10: "work", 11: "work", 20: "work", 21: "work"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// The process table is read without a lock, so it can be caught mid-reparent
// or, in the pathological case, torn into a cycle. Neither may hang the walk,
// and a contested pid must resolve the same way on every poll.
func TestAttributeIsStableAndTerminates(t *testing.T) {
	parent := map[int]int{10: 11, 11: 10}
	panes := map[string][]int{"zebra": {10}, "alpha": {10}}
	for i := 0; i < 20; i++ {
		got := attribute(panes, parent)
		want := map[int]string{10: "alpha", 11: "alpha"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: got %v, want %v", i, got, want)
		}
	}
}

func TestAssemble(t *testing.T) {
	owner := map[int]string{10: "build", 11: "build", 20: "logs", 30: "orphan"}
	// 10 and 11 are workers of one pre-forking server sharing a listening
	// socket; 30 is a pid the platform reported after the session that owned
	// it went away.
	byPID := map[int][]int{10: {8080, 3000}, 11: {8080}, 20: {9000}, 40: {9999}}
	got := assemble(owner, byPID)
	want := map[string][]int{"build": {3000, 8080}, "logs": {9000}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := assemble(owner, nil); got != nil {
		t.Errorf("no listeners: got %v, want nil", got)
	}
}

func TestAssembleCapsPerSession(t *testing.T) {
	all := make([]int, 0, maxPerSession+5)
	for p := 9000; p < 9000+maxPerSession+5; p++ {
		all = append(all, p)
	}
	got := assemble(map[int]string{7: "busy"}, map[int][]int{7: all})
	if len(got["busy"]) != maxPerSession {
		t.Fatalf("got %d ports, want %d", len(got["busy"]), maxPerSession)
	}
	// Capped after sorting, so the lowest ports survive rather than whichever
	// the process happened to open first.
	if !reflect.DeepEqual(got["busy"], all[:maxPerSession]) {
		t.Errorf("got %v, want %v", got["busy"], all[:maxPerSession])
	}
}

func TestListeningWithoutPanes(t *testing.T) {
	if got := Listening(nil); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}
