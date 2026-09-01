//go:build darwin

package tcc

import (
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const supported = true

// probeTimeout bounds a read that TCC may park behind a consent prompt.
// Timing out is itself an answer: the prompt is up on the Mac's screen.
const probeTimeout = 2 * time.Second

func probeDirs() []Dir {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	out := make([]Dir, len(protected))
	var wg sync.WaitGroup
	for i, name := range protected {
		out[i] = Dir{Name: name, Path: filepath.Join(home, name), Status: "pending"}
		wg.Add(1)
		go func(d *Dir) {
			defer wg.Done()
			status := make(chan string, 1)
			// The inner goroutine may outlive the timeout, parked behind
			// the prompt; the buffered channel lets it end whenever the
			// prompt is answered.
			go func() { status <- readDir(d.Path) }()
			select {
			case s := <-status:
				d.Status = s
			case <-time.After(probeTimeout):
			}
		}(&out[i])
	}
	wg.Wait()
	return out
}

func readDir(path string) string {
	f, err := os.Open(path)
	if err != nil {
		switch {
		case Denied(err):
			return "blocked"
		case os.IsNotExist(err):
			return "absent"
		default:
			return "error"
		}
	}
	defer f.Close()
	// Open can succeed where listing is what TCC actually guards.
	if _, err := f.Readdirnames(1); err != nil && err != io.EOF {
		if Denied(err) {
			return "blocked"
		}
		return "error"
	}
	return "ok"
}
