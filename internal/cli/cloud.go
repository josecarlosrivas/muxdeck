package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"strings"
)

// runCloud drives the daemon's cloud account: status by default, signin
// with a device token from the account page, signout, and sync. The token
// is stored by the daemon — the config file belongs to it.
func runCloud(e *env, args []string) error {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "", "status":
		return cloudStatus(e, args)
	case "signin":
		return cloudSignIn(e, args)
	case "signout":
		return cloudSignOut(e, args)
	case "sync":
		return cloudSync(e, args)
	}
	return fmt.Errorf("%w: unknown cloud subcommand %q", errUsage, sub)
}

type cloudStatusView struct {
	SignedIn bool   `json:"signed_in"`
	URL      string `json:"url"`
	Account  string `json:"account"`
	State    string `json:"state"`
	Error    string `json:"error"`
	SyncedAt string `json:"synced_at"`
	Daemons  []struct {
		Name      string `json:"name"`
		RelayName string `json:"relay_name"`
		Remote    string `json:"remote"`
		Self      bool   `json:"self"`
		Skipped   string `json:"skipped"`
	} `json:"daemons"`
}

func printCloud(e *env, st cloudStatusView) {
	switch {
	case !st.SignedIn && st.State == "revoked":
		fmt.Fprintf(e.out, "cloud: signed out — %s\n", st.Error)
	case !st.SignedIn:
		fmt.Fprintln(e.out, "cloud: signed out — run \"muxdeck cloud signin <device-token>\" (mint one on the account page)")
	default:
		line := fmt.Sprintf("cloud: %s — %s as %s", st.State, st.URL, st.Account)
		if st.Error != "" {
			line += " (" + st.Error + ")"
		}
		fmt.Fprintln(e.out, line)
		for _, d := range st.Daemons {
			switch {
			case d.Self:
				fmt.Fprintf(e.out, "  %-24s %s  (this machine)\n", d.Name, d.RelayName)
			case d.Skipped != "":
				fmt.Fprintf(e.out, "  %-24s %s  skipped: %s\n", d.Name, d.RelayName, d.Skipped)
			default:
				fmt.Fprintf(e.out, "  %-24s %s  → remote %s\n", d.Name, d.RelayName, d.Remote)
			}
		}
	}
}

func cloudShow(e *env, raw []byte) error {
	var st cloudStatusView
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("unexpected response: %w", err)
	}
	printCloud(e, st)
	return nil
}

func cloudStatus(e *env, args []string) error {
	pos, err := newFlags(e, "cloud", mixed, args, nil)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("%w: cloud status takes no arguments", errUsage)
	}
	raw, err := e.api.do(http.MethodGet, "/api/cloud", nil)
	if err != nil {
		return err
	}
	return cloudShow(e, raw)
}

func cloudSignIn(e *env, args []string) error {
	var cloudURL, domain string
	pos, err := newFlags(e, "cloud signin", mixed, args, func(fs *flag.FlagSet) {
		fs.StringVar(&cloudURL, "cloud", "", "control plane (default: the configured one, else https://cloud.muxdeck.app)")
		fs.StringVar(&domain, "relay-domain", "", "suffix machines are served under, when it cannot be derived from -url")
	})
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("%w: cloud signin <device-token>", errUsage)
	}
	body := map[string]string{"token": pos[0]}
	if cloudURL != "" {
		body["url"] = cloudURL
	}
	if domain != "" {
		body["relay_domain"] = domain
	}
	raw, err := e.api.do(http.MethodPost, "/api/cloud", body)
	if err != nil {
		return err
	}
	return cloudShow(e, raw)
}

func cloudSignOut(e *env, args []string) error {
	pos, err := newFlags(e, "cloud signout", mixed, args, nil)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("%w: cloud signout takes no arguments", errUsage)
	}
	if _, err := e.api.do(http.MethodDelete, "/api/cloud", nil); err != nil {
		return err
	}
	fmt.Fprintln(e.out, "cloud: signed out")
	return nil
}

func cloudSync(e *env, args []string) error {
	pos, err := newFlags(e, "cloud sync", mixed, args, nil)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("%w: cloud sync takes no arguments", errUsage)
	}
	raw, err := e.api.do(http.MethodPost, "/api/cloud/sync", nil)
	if err != nil {
		return err
	}
	return cloudShow(e, raw)
}
