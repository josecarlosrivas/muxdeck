package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// --- relay ---

// runRelay drives the daemon's tunnel: status by default, set/off/on for
// configuration, and setup for the hosted claim flow. Configuration goes
// through the daemon API — the config file belongs to the daemon.
func runRelay(e *env, args []string) error {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "", "status":
		return relayStatus(e, args)
	case "set":
		return relaySet(e, args)
	case "off", "on":
		return relayToggle(e, sub == "off", args)
	case "setup":
		return relaySetup(e, args)
	}
	return fmt.Errorf("%w: unknown relay subcommand %q", errUsage, sub)
}

type relayStatusView struct {
	Configured bool   `json:"configured"`
	URL        string `json:"url"`
	Off        bool   `json:"off"`
	State      string `json:"state"`
	Error      string `json:"error"`
}

func printRelay(e *env, st relayStatusView) {
	switch {
	case !st.Configured:
		fmt.Fprintln(e.out, "relay: not configured — run \"muxdeck relay set <wss-url> [key]\" or \"muxdeck relay setup <account-url>\"")
	case st.State == "rejected":
		fmt.Fprintf(e.out, "relay: rejected — %s\n  the relay refused the credential; re-claim, then \"muxdeck relay on\"\n", st.URL)
	default:
		line := fmt.Sprintf("relay: %s — %s", st.State, st.URL)
		if st.Error != "" {
			line += " (" + st.Error + ")"
		}
		fmt.Fprintln(e.out, line)
	}
}

func relayStatus(e *env, args []string) error {
	pos, err := newFlags(e, "relay", mixed, args, nil)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("%w: relay status takes no arguments", errUsage)
	}
	raw, err := e.api.do(http.MethodGet, "/api/relay", nil)
	if err != nil {
		return err
	}
	var st relayStatusView
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("unexpected response: %w", err)
	}
	printRelay(e, st)
	return nil
}

func relaySet(e *env, args []string) error {
	pos, err := newFlags(e, "relay set", mixed, args, nil)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return fmt.Errorf("%w: relay set <wss-url> [key]", errUsage)
	}
	body := map[string]string{"url": pos[0]}
	if len(pos) == 2 {
		body["key"] = pos[1]
	}
	raw, err := e.api.do(http.MethodPost, "/api/relay", body)
	if err != nil {
		return err
	}
	var st relayStatusView
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("unexpected response: %w", err)
	}
	printRelay(e, st)
	return nil
}

func relayToggle(e *env, off bool, args []string) error {
	pos, err := newFlags(e, "relay", mixed, args, nil)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("%w: relay off|on takes no arguments", errUsage)
	}
	raw, err := e.api.do(http.MethodPost, "/api/relay", map[string]bool{"off": off})
	if err != nil {
		return err
	}
	var st relayStatusView
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("unexpected response: %w", err)
	}
	printRelay(e, st)
	return nil
}

// relaySetup claims this daemon with a hosted relay's control plane: it
// requests a claim code, configures the daemon with the dial credential,
// and tells the human where to type the code. The dial shows rejected
// until the claim lands — that is the expected order, not an error.
func relaySetup(e *env, args []string) error {
	var name, relayURL string
	host, _ := os.Hostname()
	pos, err := newFlags(e, "relay setup", mixed, args, func(fs *flag.FlagSet) {
		fs.StringVar(&name, "name", host, "daemon name shown on the account page")
		fs.StringVar(&relayURL, "relay-url", "", "tunnel URL when the control plane doesn't advertise one")
	})
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("%w: relay setup <account-url>", errUsage)
	}
	base := strings.TrimRight(pos[0], "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}

	payload, _ := json.Marshal(map[string]string{"name": name})
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Post(base+"/api/claim/start", "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		return fmt.Errorf("claim start: %s answered %s", base, res.Status)
	}
	var claim struct {
		Code       string `json:"code"`
		Credential string `json:"credential"`
		ExpiresIn  int    `json:"expiresIn"`
		RelayURL   string `json:"relayUrl"`
	}
	if err := json.NewDecoder(res.Body).Decode(&claim); err != nil {
		return fmt.Errorf("unexpected response: %w", err)
	}
	if relayURL == "" {
		relayURL = claim.RelayURL
	}
	if relayURL == "" {
		return fmt.Errorf("%s did not advertise a relay URL; pass -relay-url", base)
	}

	if _, err := e.api.do(http.MethodPost, "/api/relay", map[string]string{"url": relayURL, "key": claim.Credential}); err != nil {
		return err
	}
	fmt.Fprintf(e.out, `claim code: %s   (expires in %d minutes)

Enter it on your account page at %s.
The tunnel dials now and shows "rejected" until the claim lands — after
claiming, run "muxdeck relay on", then "muxdeck relay" to check state.
`, claim.Code, claim.ExpiresIn/60, base)
	return nil
}
