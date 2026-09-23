package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// runService installs this binary as the machine's always-on daemon — a
// launchd agent on macOS, a systemd user unit on Linux — so terminals stay
// reachable from phones and the cloud when no app is open, and Keep Awake
// works without one. The default shape is the installer's cloud mode:
// loopback, tokenless, claimed with the hosted relay whose gate does the
// authenticating. The desktop app calls this from its first-launch offer,
// pointing the service at the daemon inside its own bundle, so the service
// follows app updates.
func runService(e *env, args []string) error {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "", "status":
		return serviceStatus(e, args)
	case "install":
		return serviceInstall(e, args)
	case "uninstall":
		return serviceUninstall(e, args)
	}
	return fmt.Errorf("%w: unknown service subcommand %q", errUsage, sub)
}

const (
	launchdLabelDefault = "com.muxdeck.agent"
	systemdUnitName     = "muxdeck"
)

var errNoServiceOS = errors.New("services are supported on macOS (launchd) and Linux (systemd --user)")

func serviceInstall(e *env, args []string) error {
	var addr, bin, token, cloudURL, name string
	var noClaim, asJSON bool
	pos, err := newFlags(e, "service install", mixed, args, func(fs *flag.FlagSet) {
		fs.StringVar(&addr, "addr", "127.0.0.1:8300", "listen address for the service")
		fs.StringVar(&bin, "bin", "", "daemon binary the service runs (default: this one)")
		fs.StringVar(&token, "service-token", "", "fixed access token for the service; none on loopback behind a gated relay")
		fs.StringVar(&cloudURL, "cloud", "https://cloud.muxdeck.app", "account server to claim with")
		fs.StringVar(&name, "name", "", "daemon name on the account page (default: hostname)")
		fs.BoolVar(&noClaim, "no-claim", false, "install only; no relay claim")
		fs.BoolVar(&asJSON, "json", false, "print the result as JSON (for the desktop app)")
	})
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("%w: service install takes no arguments", errUsage)
	}
	if bin == "" {
		if bin, err = os.Executable(); err != nil {
			return err
		}
		if resolved, err := filepath.EvalSymlinks(bin); err == nil {
			bin = resolved
		}
	}
	unit, err := installUnit(bin, addr, token)
	if err != nil {
		return err
	}
	probe := "http://" + probeAddr(addr)
	if err := waitForDaemon(probe, 30*time.Second); err != nil {
		return fmt.Errorf("%s installed but the daemon did not answer on %s: %w", unit, probe, err)
	}
	result := map[string]any{"installed": true, "unit": unit, "bin": bin, "addr": addr}
	if !noClaim && cloudURL != "" {
		base := strings.TrimRight(cloudURL, "/")
		if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			base = "https://" + base
		}
		if name == "" {
			name, _ = os.Hostname()
		}
		c, err := startClaim(newClient(probe, token), base, name, "")
		if err != nil {
			return fmt.Errorf("service installed, but the relay claim failed: %w", err)
		}
		result["claim"] = map[string]any{"code": c.Code, "expires_in": c.ExpiresIn, "url": base}
	}
	if asJSON {
		return json.NewEncoder(e.out).Encode(result)
	}
	fmt.Fprintf(e.out, "service: %s running %s on %s\n", unit, bin, addr)
	if cl, ok := result["claim"].(map[string]any); ok {
		fmt.Fprintf(e.out, `claim code: %s   (expires in %d minutes)

Enter it on your account page at %s under Daemons, then run
"muxdeck relay on" to re-arm the tunnel.
`, cl["code"], cl["expires_in"].(int)/60, cl["url"])
	}
	if runtime.GOOS == "linux" {
		fmt.Fprintln(e.out, "note: a user unit starts at login; \"loginctl enable-linger\" makes it start at boot")
	}
	return nil
}

func serviceUninstall(e *env, args []string) error {
	pos, err := newFlags(e, "service uninstall", mixed, args, nil)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("%w: service uninstall takes no arguments", errUsage)
	}
	unit, err := removeUnit()
	if err != nil {
		return err
	}
	fmt.Fprintf(e.out, "service: %s removed\n", unit)
	return nil
}

func serviceStatus(e *env, args []string) error {
	pos, err := newFlags(e, "service", mixed, args, nil)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fmt.Errorf("%w: service status takes no arguments", errUsage)
	}
	unit, running, err := unitStatus()
	if err != nil {
		return err
	}
	switch {
	case unit == "":
		fmt.Fprintln(e.out, "service: not installed — \"muxdeck service install\" runs the daemon in the background")
	case running:
		fmt.Fprintf(e.out, "service: %s running\n", unit)
	default:
		fmt.Fprintf(e.out, "service: %s installed but not running\n", unit)
	}
	return nil
}

// --- units: the same shapes install.sh writes, rendered here so the app
// and the CLI produce one thing.

func launchdPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabelDefault+".plist")
}

func systemdUnitPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", systemdUnitName+".service")
}

func launchdPlist(bin, addr, token string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>` + launchdLabelDefault + `</string>
  <key>ProgramArguments</key><array><string>` + xmlEscape(bin) + `</string></array>
  <key>EnvironmentVariables</key><dict>
    <key>MUXDECK_ADDR</key><string>` + xmlEscape(addr) + `</string>
`)
	if token != "" {
		b.WriteString("    <key>MUXDECK_TOKEN</key><string>" + xmlEscape(token) + "</string>\n")
	}
	b.WriteString(`  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
</dict></plist>
`)
	return b.String()
}

func systemdUnit(bin, addr, token string) string {
	env := "MUXDECK_ADDR=" + addr
	if token != "" {
		env += " MUXDECK_TOKEN=" + token
	}
	return `[Unit]
Description=muxdeck web terminal for tmux
After=network-online.target

[Service]
Environment=` + env + `
ExecStart=` + bin + `
Restart=on-failure
RestartSec=3
# keep the tmux server (and your sessions) alive across muxdeck restarts
KillMode=process

[Install]
WantedBy=default.target
`
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

func installUnit(bin, addr, token string) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		path := launchdPlistPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, []byte(launchdPlist(bin, addr, token)), 0o600); err != nil {
			return "", err
		}
		gui := fmt.Sprintf("gui/%d", os.Getuid())
		target := gui + "/" + launchdLabelDefault
		exec.Command("launchctl", "bootout", target).Run()
		// bootout returns before the agent is gone, and bootstrapping a
		// label still unloading fails with an I/O error: wait it out,
		// then give bootstrap a few tries.
		for i := 0; i < 20 && exec.Command("launchctl", "print", target).Run() == nil; i++ {
			time.Sleep(250 * time.Millisecond)
		}
		var out []byte
		var err error
		for i := 0; i < 5; i++ {
			if out, err = exec.Command("launchctl", "bootstrap", gui, path).CombinedOutput(); err == nil {
				return launchdLabelDefault, nil
			}
			time.Sleep(500 * time.Millisecond)
		}
		return "", fmt.Errorf("launchctl bootstrap: %s", strings.TrimSpace(firstLine(string(out), err.Error())))
	case "linux":
		path := systemdUnitPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, []byte(systemdUnit(bin, addr, token)), 0o600); err != nil {
			return "", err
		}
		for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", systemdUnitName}, {"restart", systemdUnitName}} {
			if out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput(); err != nil {
				return "", fmt.Errorf("systemctl --user %s: %s", strings.Join(args, " "), strings.TrimSpace(firstLine(string(out), err.Error())))
			}
		}
		return systemdUnitName + ".service", nil
	}
	return "", errNoServiceOS
}

func removeUnit() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabelDefault)).Run()
		if err := os.Remove(launchdPlistPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return launchdLabelDefault, nil
	case "linux":
		exec.Command("systemctl", "--user", "disable", "--now", systemdUnitName).Run()
		if err := os.Remove(systemdUnitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		exec.Command("systemctl", "--user", "daemon-reload").Run()
		return systemdUnitName + ".service", nil
	}
	return "", errNoServiceOS
}

func unitStatus() (unit string, running bool, err error) {
	switch runtime.GOOS {
	case "darwin":
		label := launchdLabel()
		if label == "" {
			if _, err := os.Stat(launchdPlistPath()); err == nil {
				return launchdLabelDefault, false, nil
			}
			return "", false, nil
		}
		return label, exec.Command("launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), label)).Run() == nil, nil
	case "linux":
		if _, err := os.Stat(systemdUnitPath()); err != nil {
			if exec.Command("systemctl", "is-enabled", systemdUnitName).Run() == nil {
				return systemdUnitName + ".service (system)", exec.Command("systemctl", "is-active", systemdUnitName).Run() == nil, nil
			}
			return "", false, nil
		}
		return systemdUnitName + ".service", exec.Command("systemctl", "--user", "is-active", systemdUnitName).Run() == nil, nil
	}
	return "", false, errNoServiceOS
}

// probeAddr is where to reach a daemon bound to addr from this machine: a
// wildcard bind answers on loopback.
func probeAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func waitForDaemon(base string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	hc := &http.Client{Timeout: 2 * time.Second}
	for {
		res, err := hc.Get(base + "/api/meta")
		if err == nil {
			res.Body.Close()
			if res.StatusCode < 500 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return fmt.Errorf("answered %d", res.StatusCode)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func firstLine(s, fallback string) string {
	if line, _, _ := strings.Cut(strings.TrimSpace(s), "\n"); line != "" {
		return line
	}
	return fallback
}
