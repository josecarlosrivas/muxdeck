package cli

import (
	"strings"
	"testing"
)

func TestLaunchdPlistShape(t *testing.T) {
	p := launchdPlist("/Applications/muxdeck.app/Contents/MacOS/muxdeck", "127.0.0.1:8300", "")
	for _, want := range []string{
		"<key>Label</key><string>com.muxdeck.agent</string>",
		"<array><string>/Applications/muxdeck.app/Contents/MacOS/muxdeck</string></array>",
		"<key>MUXDECK_ADDR</key><string>127.0.0.1:8300</string>",
		"<key>RunAtLoad</key><true/>",
		"<key>SuccessfulExit</key><false/>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "MUXDECK_TOKEN") {
		t.Error("tokenless install wrote a token key")
	}
	withToken := launchdPlist("/opt/bin & co/muxdeck", "0.0.0.0:8300", "s3<cret>")
	if !strings.Contains(withToken, "<key>MUXDECK_TOKEN</key><string>s3&lt;cret&gt;</string>") || !strings.Contains(withToken, "/opt/bin &amp; co/muxdeck") {
		t.Errorf("escaping:\n%s", withToken)
	}
}

func TestSystemdUnitShape(t *testing.T) {
	u := systemdUnit("/home/u/.local/bin/muxdeck", "127.0.0.1:8300", "")
	for _, want := range []string{"ExecStart=/home/u/.local/bin/muxdeck", "Environment=MUXDECK_ADDR=127.0.0.1:8300\n", "KillMode=process", "WantedBy=default.target"} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %q:\n%s", want, u)
		}
	}
	if !strings.Contains(systemdUnit("/b", "0.0.0.0:8300", "tok"), "Environment=MUXDECK_ADDR=0.0.0.0:8300 MUXDECK_TOKEN=tok") {
		t.Error("token missing from unit environment")
	}
}

func TestProbeAddr(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:8300": "127.0.0.1:8300",
		"0.0.0.0:8300":   "127.0.0.1:8300",
		":8301":          "127.0.0.1:8301",
		"[::]:8300":      "127.0.0.1:8300",
		"10.0.0.5:8300":  "10.0.0.5:8300",
	} {
		if got := probeAddr(in); got != want {
			t.Errorf("probeAddr(%q) = %q, want %q", in, got, want)
		}
	}
}
