package main

import (
	"crypto/rand"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/josecarlosrivas/muxdeck/internal/cli"
	"github.com/josecarlosrivas/muxdeck/internal/mushrun"
	"github.com/josecarlosrivas/muxdeck/internal/remote"
	"github.com/josecarlosrivas/muxdeck/internal/server"
)

//go:embed web
var webFS embed.FS

// set via -ldflags "-X main.version=..." in releases
var version = "dev"

// One binary, two jobs. A bare subcommand selects the CLI; anything else —
// no arguments, or the daemon's flags, which all start with "-" — serves.
// Shipping the client inside the server is what makes it available wherever
// muxdeck already is, which on a box you reached to look at its sessions is
// the only place it is any use.
func main() {
	if cli.Selected(os.Args[1:]) {
		os.Exit(cli.Run(os.Args[1:]))
	}
	serve()
}

func serve() {
	addr := flag.String("addr", envOr("MUXDECK_ADDR", "127.0.0.1:8300"), "listen address")
	tokenFlag := flag.String("token", os.Getenv("MUXDECK_TOKEN"), `access token; "auto" generates a code; empty generates one on non-loopback binds`)
	noAuth := flag.Bool("no-auth", os.Getenv("MUXDECK_NO_AUTH") != "", "allow unauthenticated access on non-loopback binds")
	tlsCert := flag.String("tls-cert", os.Getenv("MUXDECK_TLS_CERT"), "TLS certificate file; serve HTTPS when set with -tls-key")
	tlsKey := flag.String("tls-key", os.Getenv("MUXDECK_TLS_KEY"), "TLS key file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("muxdeck", version)
		return
	}

	static, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}

	scheme := "http"
	if *tlsCert != "" || *tlsKey != "" {
		if *tlsCert == "" || *tlsKey == "" {
			log.Fatal("-tls-cert and -tls-key must be set together")
		}
		scheme = "https"
	}

	remotes, err := remote.Load(envOr("MUXDECK_REMOTES", remote.DefaultPath()))
	if err != nil {
		log.Fatal(err)
	}
	// ssh tunnels are child processes; kill them on the way out so they
	// don't outlive the daemon across launchd/sidecar restarts.
	mushruns := mushrun.New(os.Getenv("MUXDECK_MUSH_BIN"))
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		remotes.Shutdown()
		mushruns.Shutdown()
		os.Exit(0)
	}()

	token, generated := resolveToken(*tokenFlag, *addr, *noAuth)
	srv := server.New(static, token, generated, remotes, mushruns)

	log.Printf("muxdeck %s listening on %s", version, *addr)
	for _, u := range urls(scheme, *addr) {
		log.Printf("  → %s", u)
	}
	switch {
	case generated:
		log.Printf("access code: %s   (set your own with -token, disable auth with -no-auth)", token)
	case token != "":
		log.Printf("auth: token required")
	default:
		log.Printf("auth: disabled (loopback bind)")
	}

	if scheme == "https" {
		err = http.ListenAndServeTLS(*addr, *tlsCert, *tlsKey, srv)
	} else {
		err = http.ListenAndServe(*addr, srv)
	}
	log.Fatal(err)
}

// Crockford-style alphabet: no I, L, O, U, 0, 1 to avoid misreading.
const codeAlphabet = "ABCDEFGHJKMNPQRSTVWXYZ23456789"

func genCode(n int) string {
	b := make([]byte, n)
	for i := range b {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(codeAlphabet))))
		if err != nil {
			log.Fatal(err)
		}
		b[i] = codeAlphabet[idx.Int64()]
	}
	return string(b)
}

// resolveToken decides the auth material. Explicit tokens win; otherwise a
// short access code is generated whenever the bind is reachable beyond
// loopback, unless the operator opts out — secure by default.
func resolveToken(token, addr string, noAuth bool) (string, bool) {
	switch {
	case token == "auto":
		return genCode(6), true
	case token != "":
		return token, false
	case noAuth || isLoopback(addr):
		return "", false
	default:
		return genCode(6), true
	}
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// urls lists the addresses a client could plausibly reach this server at,
// expanding wildcard binds to per-interface URLs.
func urls(scheme, addr string) []string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return []string{fmt.Sprintf("%s://%s", scheme, addr)}
	}
	ip := net.ParseIP(host)
	if host != "" && (ip == nil || !ip.IsUnspecified()) {
		return []string{fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(host, port))}
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return []string{fmt.Sprintf("%s://localhost:%s", scheme, port)}
	}
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil || ipn.IP.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(ipn.IP.String(), port)))
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
