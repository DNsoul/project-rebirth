// Command awgprobe is the measuring instrument for the blocking experiment.
//
// It answers on several UDP ports and one TCP port so the outside can tell
// which of them still carry traffic from Russia, and it keeps its own log of
// whether the server is alive and still receiving packets. When the server
// goes unreachable, that log is what distinguishes "the machine broke" from
// "something upstream stopped letting packets through".
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"awgrotator/internal/probe"
)

func main() {
	udpPorts := flag.String("udp", "53,443,34567", "UDP ports to answer DNS on")
	bindIP := flag.String("bind", "", "address to bind the UDP listeners to (default: all)")
	tcpPort := flag.Int("tcp", 443, "TCP port to serve HTTPS on")
	logPath := flag.String("log", "/var/log/awgprobe.jsonl", "where to append samples")
	every := flag.Duration("every", 30*time.Second, "how often to sample")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.LUTC)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var listeners []*probe.Responder
	for _, field := range strings.Split(*udpPorts, ",") {
		port, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil {
			log.Fatalf("bad udp port %q: %v", field, err)
		}
		r, err := probe.NewResponder(*bindIP, port)
		if err != nil {
			log.Printf("could not listen on udp/%d: %v", port, err)
			continue
		}
		go r.Serve()
		listeners = append(listeners, r)
		log.Printf("answering dns on udp/%d", port)
	}
	if len(listeners) == 0 {
		log.Fatal("no udp listener came up")
	}
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()

	if *tcpPort > 0 {
		srv, err := startHTTPS(*tcpPort)
		if err != nil {
			log.Printf("could not serve https on tcp/%d: %v", *tcpPort, err)
		} else {
			log.Printf("serving https on tcp/%d", *tcpPort)
			defer srv.Close()
		}
	}

	ticker := time.NewTicker(*every)
	defer ticker.Stop()

	sample := func() {
		s := probe.Collect(ctx, listeners)
		if err := probe.Append(*logPath, s); err != nil {
			log.Printf("could not append sample: %v", err)
		}
		log.Printf("outbound=%v seen_as=%s rx=%d listeners=%v",
			s.OutboundOK, s.SeenAs, s.RxPkts, summarise(s.Listeners))
	}
	sample()

	for {
		select {
		case <-ctx.Done():
			log.Print("stopping")
			return
		case <-ticker.C:
			sample()
		}
	}
}

func summarise(stats []probe.Stats) string {
	parts := make([]string, 0, len(stats))
	for _, s := range stats {
		parts = append(parts, fmt.Sprintf("%d:%d/%d", s.Port, s.Received, s.Answered))
	}
	return strings.Join(parts, " ")
}

// startHTTPS serves a trivial page over TLS with a self-signed certificate.
// The certificate is not meant to validate: the point is only that a TLS
// handshake on tcp/443 completes.
func startHTTPS(port int) (*http.Server, error) {
	cert, err := selfSigned()
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})
	srv := &http.Server{
		Addr:      ":" + strconv.Itoa(port),
		Handler:   mux,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return nil, err
	}
	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
			log.Printf("https stopped: %v", err)
		}
	}()
	return srv, nil
}

func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "probe"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
