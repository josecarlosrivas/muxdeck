package power

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"sync"
)

// Caffeinate is the macOS backend: an owned `caffeinate -s -w <daemon
// pid>` child. -s prevents idle system sleep only while on AC power (the
// display may still sleep); -w binds the assertion to this process, so a
// crashed daemon cannot leave one behind. Not -i: that would also hold
// the machine up on battery.
type Caffeinate struct{}

func (Caffeinate) Name() string { return "caffeinate" }

func (Caffeinate) Start() (Assertion, error) {
	cmd := exec.Command("/usr/bin/caffeinate", "-s", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	a := &child{cmd: cmd, done: make(chan struct{})}
	go func() {
		cmd.Wait()
		close(a.done)
	}()
	return a, nil
}

// PowerSource reads `pmset -g ps`: "Now drawing from 'AC Power'".
func (Caffeinate) PowerSource() string {
	out, err := exec.Command("/usr/bin/pmset", "-g", "ps").Output()
	if err != nil {
		return ""
	}
	line, _, _ := bytes.Cut(out, []byte("\n"))
	switch {
	case bytes.Contains(line, []byte("AC Power")):
		return "ac"
	case bytes.Contains(line, []byte("Battery Power")):
		return "battery"
	}
	return ""
}

type child struct {
	cmd  *exec.Cmd
	done chan struct{}
	once sync.Once
}

func (c *child) Done() <-chan struct{} { return c.done }

func (c *child) Stop() {
	c.once.Do(func() {
		c.cmd.Process.Kill()
		<-c.done
	})
}

// Default is the platform backend, nil where there is none.
func Default() Backend { return Caffeinate{} }
