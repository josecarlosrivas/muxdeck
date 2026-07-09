package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"

	"github.com/josecarlosrivas/muxdeck/internal/server"
)

//go:embed web
var webFS embed.FS

func main() {
	addr := flag.String("addr", envOr("MUXDECK_ADDR", "127.0.0.1:8300"), "listen address")
	token := flag.String("token", os.Getenv("MUXDECK_TOKEN"), "access token; empty disables auth")
	tlsCert := flag.String("tls-cert", os.Getenv("MUXDECK_TLS_CERT"), "TLS certificate file; serve HTTPS when set with -tls-key")
	tlsKey := flag.String("tls-key", os.Getenv("MUXDECK_TLS_KEY"), "TLS key file")
	flag.Parse()

	static, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}

	srv := server.New(static, *token)
	if *tlsCert != "" || *tlsKey != "" {
		if *tlsCert == "" || *tlsKey == "" {
			log.Fatal("-tls-cert and -tls-key must be set together")
		}
		log.Printf("muxdeck listening on https://%s (auth: %v)", *addr, *token != "")
		if err := http.ListenAndServeTLS(*addr, *tlsCert, *tlsKey, srv); err != nil {
			log.Fatal(err)
		}
		return
	}
	log.Printf("muxdeck listening on http://%s (auth: %v)", *addr, *token != "")
	if err := http.ListenAndServe(*addr, srv); err != nil {
		log.Fatal(err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
