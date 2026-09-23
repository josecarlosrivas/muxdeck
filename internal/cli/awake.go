package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// runAwake drives the daemon's keep-awake preference: status by default,
// on or off to set it. The preference lives on the daemon being viewed.
func runAwake(e *env, args []string) error {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}
	pos, err := newFlags(e, "awake", mixed, args, nil)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("%w: awake [status|on|off]", errUsage)
	}
	var raw []byte
	switch sub {
	case "", "status":
		raw, err = e.api.do(http.MethodGet, "/api/power", nil)
	case "on", "off":
		raw, err = e.api.do(http.MethodPost, "/api/power", map[string]bool{"keep_awake_while_viewing": sub == "on"})
	default:
		return fmt.Errorf("%w: unknown awake subcommand %q", errUsage, sub)
	}
	if err != nil {
		return err
	}
	var st struct {
		Supported bool   `json:"supported"`
		Enabled   bool   `json:"enabled"`
		State     string `json:"state"`
		Viewers   int    `json:"viewers"`
		Backend   string `json:"backend"`
		Power     string `json:"power"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("unexpected response: %w", err)
	}
	fmt.Fprintln(e.out, "keep awake while viewing: "+awakeLine(st.State, st.Viewers, st.Backend, st.Power, st.Error))
	return nil
}

func awakeLine(state string, viewers int, backend, power, errText string) string {
	viewersText := fmt.Sprintf("%d viewer", viewers)
	if viewers != 1 {
		viewersText += "s"
	}
	switch state {
	case "off":
		return "off — \"muxdeck awake on\" to enable"
	case "waiting":
		return "on — waiting for a viewer"
	case "keeping_awake":
		return fmt.Sprintf("keeping awake — %s, %s on %s", viewersText, backend, power)
	case "on_battery":
		return fmt.Sprintf("on battery — not preventing sleep (%s)", viewersText)
	case "unsupported":
		return "unsupported on this machine (macOS only)"
	case "unavailable":
		return "unavailable — " + errText
	}
	return state
}
