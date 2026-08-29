// Package cli is muxdeck's command line: the same binary, driving the same
// HTTP API the browser drives.
//
// HTTP rather than a unix socket, which would be the usual answer for a
// local daemon. A socket is reachable only from the machine the daemon runs
// on; muxdeck's whole point is that the machine you are looking at is often
// not the machine you are on, and a session already reachable through a
// tunnel or a remote entry is one the CLI can drive with no extra plumbing.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

var commands = map[string]func(*env, []string) error{
	"ls":     runLS,
	"status": runStatus,
	"notify": runNotify,
	"send":   runSend,
}

// Selected reports whether the process arguments are meant for the CLI.
//
// Any first argument that is not a flag is. The daemon takes no positional
// arguments, so dispatching only on names this build happens to know would
// make a mistyped command start a second daemon on the configured port —
// which is a much worse outcome than an error message.
func Selected(args []string) bool {
	return len(args) > 0 && !strings.HasPrefix(args[0], "-")
}

type env struct {
	out io.Writer
	err io.Writer
	api *client
}

// Run executes one command and returns the process exit status: 0 on
// success, 1 on failure, 2 on misuse.
func Run(args []string) int {
	e := &env{out: os.Stdout, err: os.Stderr}
	if len(args) == 0 || args[0] == "help" {
		usage(e.out)
		return 0
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(e.err, "muxdeck: unknown command %q\n\n", args[0])
		usage(e.err)
		return 2
	}
	err := cmd(e, args[1:])
	switch {
	case err == nil:
		return 0
	case errors.As(err, new(flagError)):
		// ContinueOnError has already printed the message and the usage;
		// saying it a second time is the only thing left to get wrong.
		if errors.Is(err, flag.ErrHelp) {
			return 0 // -h printed what was asked for
		}
		return 2
	}
	fmt.Fprintf(e.err, "muxdeck %s: %v\n", args[0], err)
	if errors.Is(err, errUsage) {
		return 2
	}
	return 1
}

var errUsage = errors.New("bad usage")

// flagError marks a failure the flag package has already reported.
type flagError struct{ err error }

func (e flagError) Error() string { return e.err.Error() }
func (e flagError) Unwrap() error { return e.err }

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: muxdeck <command> [flags]

  ls [-json]                       list sessions
  status get <session>             print a session's agent status
  status set <session> <state>     report agent status (working|waiting|idle)
  notify [flags] <session> <msg>   raise the operator's attention on a session
  send [flags] <session> [text]    type text into a session

Run "muxdeck <command> -h" for a command's flags. notify and send take
theirs first, so a message that starts with "-" stays a message.
With no command, muxdeck serves; run "muxdeck -h" for the daemon's flags.

The daemon to talk to comes from -url (default $MUXDECK_URL, else
http://127.0.0.1:8300) and -token (default $MUXDECK_TOKEN).
`)
}

// parseMode says how a command lets flags and positional arguments mix.
//
// Go's flag package stops at the first non-flag argument, so
// "status set main working -note x" would take every flag as a positional.
// mixed resumes parsing after each positional until the flags run out, which
// is how the command reads naturally. leading does not, because a command
// whose payload is arbitrary text has to treat a message that starts with
// "-" as a message and not a mistake.
type parseMode int

const (
	mixed parseMode = iota
	leading
)

// newFlags builds a command's flag set with the connection flags every
// command shares, resolves those into e.api, and returns the positional
// arguments.
func newFlags(e *env, name string, mode parseMode, args []string, bind func(*flag.FlagSet)) ([]string, error) {
	fs := flag.NewFlagSet("muxdeck "+name, flag.ContinueOnError)
	fs.SetOutput(e.err)
	url := fs.String("url", envOr("MUXDECK_URL", "http://127.0.0.1:8300"), "daemon base URL")
	token := fs.String("token", os.Getenv("MUXDECK_TOKEN"), "access token")
	if bind != nil {
		bind(fs)
	}
	positional, err := parse(fs, mode, args)
	if err != nil {
		return nil, err
	}
	e.api = newClient(*url, *token)
	return positional, nil
}

func parse(fs *flag.FlagSet, mode parseMode, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, flagError{err}
		}
		rest := fs.Args()
		if mode == leading || len(rest) == 0 {
			return append(positional, rest...), nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// session validates a session name argument here rather than letting the
// daemon reject it, so a typo costs a message and not a round trip.
func session(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, " \t/") {
		return "", fmt.Errorf("%w: %q is not a session name", errUsage, name)
	}
	return name, nil
}
