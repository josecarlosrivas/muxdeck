package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Version is the running build's version, injected by main so the updater
// can tell a no-op from an upgrade.
var Version = "dev"

const updateRepo = "josecarlosrivas/muxdeck"

// runUpdate swaps the running binary for the latest release — the
// installer's update mode without the installer round-trip. The service
// keeps running the old binary until restarted, so it ends by offering the
// same restart the installer does.
func runUpdate(e *env, args []string) error {
	fs := flag.NewFlagSet("muxdeck update", flag.ContinueOnError)
	fs.SetOutput(e.err)
	force := fs.Bool("f", false, "reinstall even when already on the latest release")
	yes := fs.Bool("y", false, "restart the service without asking")
	if err := fs.Parse(args); err != nil {
		return flagError{err}
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: update takes no arguments", errUsage)
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return fmt.Errorf("unsupported OS %s (linux and macOS only)", runtime.GOOS)
	}

	latest, err := latestTag()
	if err != nil {
		return fmt.Errorf("resolve latest release: %w", err)
	}
	if latest == Version && !*force {
		fmt.Fprintf(e.out, "already current: muxdeck %s\n", Version)
		return nil
	}

	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return fmt.Errorf("locate running binary: %w", err)
	}

	fmt.Fprintf(e.out, "downloading muxdeck %s (%s-%s)\n", latest, runtime.GOOS, runtime.GOARCH)
	tmp, err := download(latest, exe)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%s is not writable — re-run with sudo", filepath.Dir(exe))
		}
		return err
	}
	defer os.Remove(tmp)

	// A truncated or wrong-arch download must fail here, not after it has
	// replaced the binary a restarting service will exec.
	if out, err := exec.Command(tmp, "-version").Output(); err != nil {
		return fmt.Errorf("downloaded binary failed self-check: %v", err)
	} else if !strings.HasPrefix(string(out), "muxdeck ") {
		return fmt.Errorf("downloaded binary failed self-check: unexpected output %q", strings.TrimSpace(string(out)))
	}
	if err := os.Rename(tmp, exe); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%s is not writable — re-run with sudo", exe)
		}
		return err
	}
	if Version == latest {
		fmt.Fprintf(e.out, "reinstalled muxdeck %s\n", latest)
	} else {
		fmt.Fprintf(e.out, "updated: muxdeck %s -> %s\n", Version, latest)
	}
	restartService(e, *yes)
	return nil
}

// latestTag resolves the newest release tag from the /releases/latest
// redirect — no API quota, no JSON.
func latestTag() (string, error) {
	c := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := c.Head("https://github.com/" + updateRepo + "/releases/latest")
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if resp.StatusCode/100 != 3 || i < 0 {
		return "", fmt.Errorf("unexpected response %s for latest release", resp.Status)
	}
	return loc[i+len("/tag/"):], nil
}

// download fetches the release asset for this platform into a temp file
// beside exe (same filesystem, so the rename that follows is atomic) and
// returns its path.
func download(tag, exe string) (string, error) {
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/muxdeck-%s-%s",
		updateRepo, tag, runtime.GOOS, runtime.GOARCH)
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", url, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".muxdeck-update-*")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(tmp, resp.Body); err == nil {
		err = tmp.Chmod(0o755)
	} else {
		err = fmt.Errorf("download: %w", err)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// restartService restarts whichever service shape this machine has — the
// same detection the installer uses.
func restartService(e *env, assumeYes bool) {
	var cmd []string
	if runtime.GOOS == "linux" {
		if exec.Command("systemctl", "is-enabled", "muxdeck").Run() == nil {
			cmd = []string{"systemctl", "restart", "muxdeck"}
			if os.Geteuid() != 0 {
				cmd = append([]string{"sudo"}, cmd...)
			}
		} else if exec.Command("systemctl", "--user", "is-enabled", "muxdeck").Run() == nil {
			cmd = []string{"systemctl", "--user", "restart", "muxdeck"}
		}
	} else {
		gui := fmt.Sprintf("gui/%d/com.muxdeck.agent", os.Getuid())
		if exec.Command("launchctl", "print", gui).Run() == nil {
			cmd = []string{"launchctl", "kickstart", "-k", gui}
		}
	}
	if cmd == nil {
		fmt.Fprintln(e.out, "no muxdeck service detected — restart the daemon to pick up the new binary")
		return
	}
	if !assumeYes && !confirm(e, "Restart the muxdeck service now? [Y/n]: ") {
		fmt.Fprintf(e.out, "skipped — run %q when ready\n", strings.Join(cmd, " "))
		return
	}
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, e.out, e.err
	if err := c.Run(); err != nil {
		fmt.Fprintf(e.err, "restart failed: %v — run %q manually\n", err, strings.Join(cmd, " "))
		return
	}
	fmt.Fprintln(e.out, "service restarted")
}

// confirm asks on the controlling terminal; no terminal means no (a script
// that wants the restart passes -y).
func confirm(e *env, prompt string) bool {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return false
	}
	defer tty.Close()
	fmt.Fprint(e.out, prompt)
	buf := make([]byte, 16)
	n, _ := tty.Read(buf)
	ans := strings.TrimSpace(strings.ToLower(string(buf[:n])))
	return ans == "" || ans == "y" || ans == "yes"
}
