// Command mockswitch serves a local EDM webroot like an ERS 5600 switch, for
// testing ERS-Proxy without hardware:
//
//	go run ./tools/mockswitch -root /path/to/edm/webroot -listen 127.0.0.1:18765
//
// The webroot (the switch's own files) is not part of this repository.
package main

import (
	"crypto/tls"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/mockswitch"
)

func main() {
	root := flag.String("root", "", "directory with the EDM files (login.html, index.html, …)")
	listen := flag.String("listen", "127.0.0.1:18765", "listen address")
	gz := flag.Bool("gzip", false, "send HTML, JS and CSS gzip-compressed")
	changePW := flag.Bool("change-password", false, `answer the login with "205 Change Password"`)
	logFile := flag.String("log", "", "write one JSON line per request to this file")
	certFile := flag.String("tls-cert", "", "serve HTTPS with this certificate (PEM)")
	keyFile := flag.String("tls-key", "", "private key for -tls-cert")
	snmprec := flag.String("snmprec", "", "answer the /jsp/ SNMP bridge from this .snmprec file")
	legacy := flag.Bool("tls-legacy", false, "with -tls-cert: speak only TLS 1.0 with RSA key exchange, like old switches")
	flag.Parse()
	if *root == "" {
		log.Fatal("-root is required")
	}
	opts := mockswitch.Options{Gzip: *gz, ChangePassword: *changePW}
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		opts.Log = f
	}
	if *snmprec != "" {
		f, err := os.Open(*snmprec)
		if err != nil {
			log.Fatal(err)
		}
		d, err := mockswitch.LoadSNMPRec(f)
		f.Close()
		if err != nil {
			log.Fatal(err)
		}
		opts.Data = d
	}
	srv := &http.Server{Addr: *listen, Handler: mockswitch.New(os.DirFS(*root), opts), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("mock switch on %s serving %s", *listen, *root)
	if *certFile != "" {
		if *legacy {
			srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS10,
				CipherSuites: []uint16{tls.TLS_RSA_WITH_AES_128_CBC_SHA}}
			srv.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){} // no HTTP/2, like the switch
		}
		log.Fatal(srv.ListenAndServeTLS(*certFile, *keyFile))
	}
	log.Fatal(srv.ListenAndServe())
}
