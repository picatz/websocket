// Command testee exercises the public websocket API against a loopback-only
// Autobahn testsuite. It is test infrastructure, not a production server.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/picatz/websocket"
)

type settings struct {
	compression bool
	maxBytes    int
	caseTimeout time.Duration
}

func main() {
	mode := flag.String("mode", "server", "server, client, or wait")
	address := flag.String("address", "127.0.0.1:9001", "literal loopback IP and port")
	agent := flag.String("agent", "picatz-websocket", "Autobahn report agent")
	var cfg settings
	flag.BoolVar(&cfg.compression, "compression", false, "opt into experimental permessage-deflate")
	flag.IntVar(&cfg.maxBytes, "max-bytes", 64<<20, "test-only incoming message cap")
	flag.DurationVar(&cfg.caseTimeout, "case-timeout", time.Minute, "hard connection lifetime per case")
	flag.Parse()
	if err := validateAddress(*address); err != nil {
		log.Fatal(err)
	}
	if cfg.maxBytes <= 0 || cfg.caseTimeout <= 0 {
		log.Fatal("max-bytes and case-timeout must be positive")
	}
	var err error
	switch *mode {
	case "server":
		err = serve(*address, cfg)
	case "client":
		err = runClient(*address, *agent, cfg)
	case "wait":
		err = waitReady(*address, 15*time.Second)
	default:
		err = fmt.Errorf("unknown mode %q", *mode)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func validateAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	n, err := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("address must contain a literal loopback IP and a valid port: %q", address)
	}
	return nil
}

func waitReady(address string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			return conn.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("listener %s did not become ready within %s", address, timeout)
}

func serve(address string, cfg settings) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		opts := []websocket.UpgradeOption{websocket.WithUpgradeMaxMessageSize(cfg.maxBytes)}
		if cfg.compression {
			// Extensions are stateful. Never share one across connections.
			opts = append(opts, websocket.WithUpgradeExtensions(websocket.NewPerMessageDeflateExtension()))
		}
		conn, err := websocket.Upgrade(w, r, opts...)
		if err != nil {
			log.Printf("upgrade: %v", err)
			return
		}
		echo(conn, cfg.caseTimeout)
	})
	server := &http.Server{
		Addr: address, Handler: mux,
		ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	return server.ListenAndServe()
}

func dial(address, path string, query url.Values, cfg settings) (*websocket.Conn, error) {
	u := url.URL{Scheme: "ws", Host: address, Path: path, RawQuery: query.Encode()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opts := []websocket.DialOption{websocket.WithMaxMessageSize(cfg.maxBytes)}
	if cfg.compression {
		opts = append(opts, websocket.WithExtensions(websocket.NewPerMessageDeflateExtension()))
	}
	conn, _, err := websocket.Dial(ctx, u.String(), opts...)
	return conn, err
}

// Follow the library's documented caller responsibility: close after any read
// or write error. Do not repair protocol behavior or manufacture close codes.
func echo(conn *websocket.Conn, timeout time.Duration) {
	defer conn.Close()
	timer := time.AfterFunc(timeout, func() {
		log.Printf("case watchdog expired after %s", timeout)
		_ = conn.Close()
	})
	defer timer.Stop()
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("read: %v", err)
			}
			return
		}
		if err := conn.WriteMessage(kind, data); err != nil {
			log.Printf("write: %v", err)
			return
		}
	}
}

func command(address, path, agent string, caseNumber int) ([]byte, error) {
	// Control endpoints are uncompressed and independently bounded.
	query := url.Values{"agent": {agent}}
	if caseNumber > 0 {
		query.Set("case", strconv.Itoa(caseNumber))
	}
	conn, err := dial(address, path, query, settings{maxBytes: 4096})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	timer := time.AfterFunc(30*time.Second, func() { _ = conn.Close() })
	defer timer.Stop()
	_, payload, err := conn.ReadMessage()
	if path == "/updateReports" && err == io.EOF {
		return nil, nil // Autobahn closes after synchronously writing reports.
	}
	if path == "/updateReports" && err == nil {
		return nil, fmt.Errorf("unexpected report-update payload %q", payload)
	}
	return payload, err
}

func runClient(address, agent string, cfg settings) error {
	if agent == "" {
		return errors.New("agent must not be empty")
	}
	if err := waitReady(address, 15*time.Second); err != nil {
		return err
	}
	payload, err := command(address, "/getCaseCount", agent, 0)
	if err != nil {
		return fmt.Errorf("case count: %w", err)
	}
	var count int
	if err := json.Unmarshal(payload, &count); err != nil || count < 1 || count > 10000 {
		return fmt.Errorf("invalid case count %q", payload)
	}
	log.Printf("running %d selected cases for %s", count, agent)
	for n := 1; n <= count; n++ {
		log.Printf("case %d/%d", n, count)
		query := url.Values{"agent": {agent}, "case": {strconv.Itoa(n)}}
		conn, err := dial(address, "/runCase", query, cfg)
		if err != nil {
			// Keep going so the report includes the whole selected inventory.
			// A missing result is detected by the independent report summarizer.
			log.Printf("case %d handshake: %v", n, err)
			continue
		}
		echo(conn, cfg.caseTimeout)
	}
	// Autobahn records outcomes at TCP connectionLost. Wait for the final case
	// to be recorded before requesting reports, not merely for ReadMessage EOF.
	_, statusErr := command(address, "/getCaseStatus", agent, count)
	_, reportErr := command(address, "/updateReports", agent, 0)
	return errors.Join(statusErr, reportErr)
}
