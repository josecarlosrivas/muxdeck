package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/josecarlosrivas/muxdeck/internal/agent"
)

// --- ls ---

func runLS(e *env, args []string) error {
	var asJSON bool
	pos, err := newFlags(e, "ls", mixed, args, func(fs *flag.FlagSet) {
		fs.BoolVar(&asJSON, "json", false, "print the API response verbatim")
	})
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("%w: ls takes no arguments", errUsage)
	}
	if asJSON {
		// Handed through unchanged: a script should see the API's shape, and
		// a field added to the API should reach it without a release here.
		raw, err := e.api.sessionsRaw()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(e.out, "%s", raw)
		return err
	}
	sessions, err := e.api.sessions()
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Fprintln(e.out, "no sessions")
		return nil
	}
	w := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tWINDOWS\tDIR\tBRANCH\tPORTS\tAGENT")
	for _, s := range sessions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Name, windows(s), dash(s.Path), branch(s), portList(s.Ports), agentCell(s.Agent))
	}
	return w.Flush()
}

func windows(s listEntry) string {
	if s.Attached > 0 {
		return fmt.Sprintf("%d*", s.Windows)
	}
	return strconv.Itoa(s.Windows)
}

func branch(s listEntry) string {
	if s.Branch == "" {
		return "-"
	}
	if s.Dirty {
		return s.Branch + "*"
	}
	return s.Branch
}

func portList(ports []int) string {
	if len(ports) == 0 {
		return "-"
	}
	out := make([]string, len(ports))
	for i, p := range ports {
		out[i] = strconv.Itoa(p)
	}
	return strings.Join(out, ",")
}

func agentCell(a *agent.Status) string {
	if a == nil {
		return "-"
	}
	cell := a.State
	if a.Model != "" {
		cell += " " + a.Model
	}
	if a.Progress != nil && (a.Progress.Label != "" || a.Progress.Value > 0) {
		cell += fmt.Sprintf(" %d%%", int(a.Progress.Value*100+0.5))
	}
	for _, c := range a.Chips {
		cell += " " + c.Value
	}
	return cell
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// --- status ---

func runStatus(e *env, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: status needs a verb (get or set)", errUsage)
	}
	switch args[0] {
	case "get":
		return statusGet(e, args[1:])
	case "set":
		return statusSet(e, args[1:])
	default:
		return fmt.Errorf("%w: unknown status verb %q", errUsage, args[0])
	}
}

func statusGet(e *env, args []string) error {
	pos, err := newFlags(e, "status get", mixed, args, nil)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("%w: status get <session>", errUsage)
	}
	name, err := session(pos[0])
	if err != nil {
		return err
	}
	sessions, err := e.api.sessions()
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if s.Name != name {
			continue
		}
		if s.Agent == nil {
			return fmt.Errorf("no agent status reported for %q", name)
		}
		out, err := json.MarshalIndent(s.Agent, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(e.out, "%s\n", out)
		return err
	}
	return fmt.Errorf("no such session: %s", name)
}

func statusSet(e *env, args []string) error {
	var (
		who      string
		model    string
		note     string
		cost     float64
		progress string
		label    string
		chips    chipList
		clear    bool
	)
	pos, err := newFlags(e, "status set", mixed, args, func(fs *flag.FlagSet) {
		fs.StringVar(&who, "agent", "cli", "reporting agent name")
		fs.StringVar(&model, "model", "", "model name")
		fs.StringVar(&note, "note", "", "note, shown in the badge tooltip")
		fs.Float64Var(&cost, "cost", 0, "cumulative spend in USD")
		fs.StringVar(&progress, "progress", "", "completion as a fraction of 1")
		fs.StringVar(&label, "label", "", "what the progress is measuring")
		fs.Var(&chips, "chip", "a chip, [icon:][color:]key=value; repeatable")
		fs.BoolVar(&clear, "clear", false, "clear the progress and chips (overrides -progress and -chip)")
	})
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return fmt.Errorf("%w: status set <session> <working|waiting|idle>", errUsage)
	}
	name, err := session(pos[0])
	if err != nil {
		return err
	}
	state := pos[1]
	if !agent.ValidState(state) {
		return fmt.Errorf("%w: %v", errUsage, agent.ErrBadState)
	}

	post := statusPost{Session: name, Status: agent.Status{
		Agent: who, State: state, Model: model, CostUSD: cost, Note: note,
	}}
	// The daemon reads an omitted field as "unchanged", so the flags have to
	// distinguish "not given" from "given as zero" — hence a string for
	// progress, and a -clear flag rather than "-progress 0", which is a real
	// thing to report at the start of a long job.
	if clear {
		post.Progress = &agent.Progress{}
		post.Chips = []agent.Chip{}
		return e.api.postStatus(post)
	}
	if progress != "" || label != "" {
		value, err := parseProgress(progress)
		if err != nil {
			return err
		}
		post.Progress = &agent.Progress{Value: value, Label: label}
	}
	if len(chips) > 0 {
		post.Chips = chips
	}
	return e.api.postStatus(post)
}

// parseProgress reads the -progress flag, which is a string so that "not
// given" and "given as zero" stay different answers.
func parseProgress(v string) (float64, error) {
	if v == "" {
		return 0, nil
	}
	value, err := strconv.ParseFloat(v, 64)
	if err != nil || value < 0 || value > 1 {
		return 0, fmt.Errorf("%w: -progress %q is not a fraction between 0 and 1", errUsage, v)
	}
	return value, nil
}

// --- notify ---

func runNotify(e *env, args []string) error {
	var who string
	pos, err := newFlags(e, "notify", leading, args, func(fs *flag.FlagSet) {
		fs.StringVar(&who, "agent", "", "reporting agent name (default: whoever is already reporting)")
	})
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return fmt.Errorf("%w: notify [flags] <session> <message>", errUsage)
	}
	name, err := session(pos[0])
	if err != nil {
		return err
	}
	// "waiting" is the state the UI notifies on, so raising attention is a
	// status post and not a channel of its own.
	//
	// Reusing the name already reporting for the session matters: the daemon
	// carries a post's omitted fields forward only within one reporter, so
	// notifying under a fresh name would blank the model and spend the
	// statusline had been keeping up to date.
	if who == "" {
		who = e.api.reporterFor(name)
	}
	return e.api.postStatus(statusPost{Session: name, Status: agent.Status{
		Agent: who, State: agent.StateWaiting, Note: strings.Join(pos[1:], " "),
	}})
}

// --- doctor ---

// runDoctor asks the daemon — not this process — to check protected-folder
// access, because TCC grants attach to the process doing the reading: a read
// that works from this shell, which inherits the terminal's or sshd's
// grants, proves nothing about what the service can see.
func runDoctor(e *env, args []string) error {
	pos, err := newFlags(e, "doctor", mixed, args, nil)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("%w: doctor takes no arguments", errUsage)
	}
	raw, err := e.api.do(http.MethodGet, "/api/doctor?probe", nil)
	if err != nil {
		return err
	}
	var rep struct {
		Supported bool `json:"supported"`
		Dirs      []struct {
			Name   string `json:"name"`
			Path   string `json:"path"`
			Status string `json:"status"`
		} `json:"dirs"`
		Hits        []string `json:"hits"`
		SettingsCmd string   `json:"settingsCmd"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return fmt.Errorf("unexpected response: %w", err)
	}
	if !rep.Supported {
		fmt.Fprintln(e.out, "nothing to check: folder privacy (TCC) is a macOS concern, and the daemon is not on macOS")
		return nil
	}
	blocked := false
	w := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
	for _, d := range rep.Dirs {
		note := ""
		switch d.Status {
		case "blocked":
			blocked = true
			note = "the daemon cannot read this folder"
		case "pending":
			blocked = true
			note = "a consent prompt is waiting on the Mac's screen"
		case "error":
			note = "unreadable, but not a privacy denial"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", d.Name, d.Status, note)
	}
	w.Flush()
	if len(rep.Hits) > 0 {
		blocked = true
		fmt.Fprintf(e.out, "\ndenied paths the daemon has hit: %s\n", strings.Join(rep.Hits, ", "))
	}
	if !blocked {
		fmt.Fprintln(e.out, "\nall protected folders readable")
		return nil
	}
	fmt.Fprintf(e.out, `
Sessions run under tmux, so grants matter for both binaries. Two ways in:

  per folder (least privilege): while at the Mac, touch the folder from a
  muxdeck session (e.g. ls ~/Downloads) and approve the prompt

  Full Disk Access (recommended for remote/headless use): System Settings >
  Privacy & Security > Full Disk Access, enable muxdeck and tmux; on the Mac:

    %s
`, rep.SettingsCmd)
	return nil
}

// --- send ---

func runSend(e *env, args []string) error {
	var noEnter bool
	pos, err := newFlags(e, "send", leading, args, func(fs *flag.FlagSet) {
		fs.BoolVar(&noEnter, "no-enter", false, "type the text without submitting it")
	})
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return fmt.Errorf("%w: send [flags] <session> [text]", errUsage)
	}
	name, err := session(pos[0])
	if err != nil {
		return err
	}
	// No text at all is a bare Enter, which is how you answer a prompt that
	// is waiting on one.
	body := map[string]any{"text": strings.Join(pos[1:], " "), "enter": !noEnter}
	_, err = e.api.do(http.MethodPost, "/api/sessions/"+name+"/send", body)
	return err
}
