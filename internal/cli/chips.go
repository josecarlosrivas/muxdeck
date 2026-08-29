package cli

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/josecarlosrivas/muxdeck/internal/agent"
)

type statusPost struct {
	Session string `json:"session"`
	agent.Status
}

func (c *client) postStatus(p statusPost) error {
	_, err := c.do(http.MethodPost, "/api/agent/status", p)
	return err
}

// reporterFor is the agent name already attached to a session, so a command
// that only means to change the state does not take the session over from
// whoever has been reporting it. Falling back to "cli" is right when nobody
// is: there is no previous status to preserve.
func (c *client) reporterFor(name string) string {
	sessions, err := c.sessions()
	if err != nil {
		return "cli"
	}
	for _, s := range sessions {
		if s.Name == name && s.Agent != nil && s.Agent.Agent != "" {
			return s.Agent.Agent
		}
	}
	return "cli"
}

// chipList collects repeated -chip flags.
type chipList []agent.Chip

func (l *chipList) String() string { return "" }

func (l *chipList) Set(v string) error {
	chip, err := parseChip(v)
	if err != nil {
		return err
	}
	*l = append(*l, chip)
	return nil
}

// parseChip reads "[icon:][color:]key=value".
//
// Icon and color lead rather than trail because they come from closed sets
// while the value does not: a leading segment with no "=" in it can only be
// one of them, and everything from the first "=" onward is the value
// verbatim. Trailing them would make a value that happens to end in ":warn"
// ambiguous, and a chip is often a fragment of somebody's output.
func parseChip(v string) (agent.Chip, error) {
	chip := agent.Chip{}
	rest := v
	for {
		head, tail, found := strings.Cut(rest, ":")
		if !found || strings.Contains(head, "=") {
			break
		}
		switch {
		case agent.ValidChipIcon(head) && chip.Icon == "":
			chip.Icon = head
		case agent.ValidChipColor(head) && chip.Color == "":
			chip.Color = head
		default:
			return chip, fmt.Errorf("%w: %q is not a chip icon or color in %q", errUsage, head, v)
		}
		rest = tail
	}
	key, value, found := strings.Cut(rest, "=")
	if !found || key == "" || value == "" {
		return chip, fmt.Errorf("%w: chip %q is not [icon:][color:]key=value", errUsage, v)
	}
	chip.Key, chip.Value = key, value
	return chip, nil
}
