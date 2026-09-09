package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// `sailnode stats` shows an operator their own relay, from the /stats the
// relay already serves: one HTTPS request to loopback, no ledger, no RPC,
// nothing on the network. The handler reads memory only, so polling it
// every second costs the relay nothing it is not already doing.

type relayStats struct {
	Registered    int      `json:"registered"`
	Alive         int      `json:"alive"`
	Exits         int      `json:"exits"`
	Countries     []string `json:"countries"`
	Price         string   `json:"priceXnoPerMiB"`
	RelayedMiB    float64  `json:"relayedMiB"`
	Circuits      int      `json:"circuits"`
	UptimeSeconds int64    `json:"uptimeSeconds"`
	Error         string   `json:"error"`
}

func fetchStats(addr string, timeout time.Duration) (*relayStats, error) {
	hc := &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} // our own relay, on loopback: the certificate is ours and self-signed
	resp, err := hc.Get("https://" + addr + "/stats")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parseStats(body)
}

func parseStats(body []byte) (*relayStats, error) {
	var s relayStats
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("not a /stats answer: %v", err)
	}
	if s.Error != "" {
		return nil, fmt.Errorf("relay: %s", s.Error)
	}
	return &s, nil
}

func fmtDur(sec int64) string {
	d := time.Duration(sec) * time.Second
	if d >= 24*time.Hour {
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func printStats(s *relayStats) {
	fmt.Printf("network:  %d relays alive of %d registered, %d exits, %d countries, price %s XNO/MiB\n", s.Alive, s.Registered, s.Exits, len(s.Countries), strings.TrimRight(strings.TrimRight(s.Price, "0"), "."))
	fmt.Printf("this relay: %.1f MiB relayed, %d circuits open, up %s\n", s.RelayedMiB, s.Circuits, fmtDur(s.UptimeSeconds))
}

func runStats(args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:443", "where this relay listens (the first --listen address)")
	watch := fs.Int("watch", 0, "refresh every N seconds and show the rate (0 = once)")
	asJSON := fs.Bool("json", false, "print the relay's /stats answer as is")
	fs.Parse(args)
	s, err := fetchStats(*addr, 8*time.Second)
	if err != nil {
		log.Fatalf("stats: %v (is the relay running, and is --addr its listen address?)", err)
	}
	if *asJSON {
		b, _ := json.MarshalIndent(s, "", "  ")
		fmt.Println(string(b))
		if *watch == 0 {
			return
		}
	}
	printStats(s)
	if *watch <= 0 {
		return
	}
	// Live: the same request again every N seconds, and the difference as a rate.
	prev, at := s.RelayedMiB, time.Now()
	for {
		time.Sleep(time.Duration(*watch) * time.Second)
		n, err := fetchStats(*addr, 8*time.Second)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s  (no answer: %v)\n", time.Now().Format("15:04:05"), err)
			continue
		}
		dt := time.Since(at).Seconds()
		d := n.RelayedMiB - prev
		fmt.Printf("%s  %.1f MiB relayed  %+.1f MiB in %.0fs (%.2f MiB/s)  %d circuits  %d/%d relays alive\n", time.Now().Format("15:04:05"), n.RelayedMiB, d, dt, d/dt, n.Circuits, n.Alive, n.Registered)
		prev, at = n.RelayedMiB, time.Now()
	}
}
