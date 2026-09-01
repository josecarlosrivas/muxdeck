package tcc

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"
)

func TestDenied(t *testing.T) {
	for _, err := range []error{
		syscall.EPERM,
		&os.PathError{Op: "open", Path: "/x", Err: syscall.EPERM},
		// git's failure arrives as captured output, text only
		errors.New("fatal: cannot change to '/Users/x/Downloads': Operation not permitted"),
	} {
		if !Denied(err) {
			t.Errorf("%v: got false, want true", err)
		}
	}
	// EACCES is ordinary unix permissions, not privacy settings.
	for _, err := range []error{
		nil,
		syscall.EACCES,
		&os.PathError{Op: "open", Path: "/x", Err: syscall.EACCES},
		errors.New("fatal: not a git repository"),
	} {
		if Denied(err) {
			t.Errorf("%v: got true, want false", err)
		}
	}
}

func TestNoteRecordsOnlyDenials(t *testing.T) {
	hits.Lock()
	hits.paths = nil
	hits.Unlock()

	note("/Users/x/Downloads", syscall.EPERM)
	note("/Users/x/Downloads/", syscall.EPERM) // cleans to the same path
	note("/Users/x/repo", errors.New("not a git repository"))
	note("", syscall.EPERM)

	got := Status(false)
	if supported {
		if len(got.Hits) != 1 || got.Hits[0] != "/Users/x/Downloads" {
			t.Errorf("hits: got %v, want [/Users/x/Downloads]", got.Hits)
		}
	} else if len(got.Hits) != 0 {
		t.Errorf("hits on unsupported platform: got %v", got.Hits)
	}

	hits.Lock()
	if len(hits.paths) != 1 {
		t.Errorf("recorded %d paths, want 1", len(hits.paths))
	}
	hits.paths = nil
	hits.Unlock()
}

func TestNoteCap(t *testing.T) {
	hits.Lock()
	hits.paths = nil
	hits.Unlock()
	for i := 0; i < maxHits*2; i++ {
		note(fmt.Sprintf("/p/%d", i), syscall.EPERM)
	}
	hits.Lock()
	defer hits.Unlock()
	if len(hits.paths) != maxHits {
		t.Errorf("recorded %d paths, want %d", len(hits.paths), maxHits)
	}
	hits.paths = nil
}
