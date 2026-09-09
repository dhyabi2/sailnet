package main

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStatsParsesTheRelaysAnswer(t *testing.T) {
	s, err := parseStats([]byte(`{"ok":true,"registered":54,"alive":15,"exits":14,"countries":["DE","US"],"priceXnoPerMiB":"0.00050000","relayedMiB":106.8,"circuits":16,"uptimeSeconds":28920}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Alive != 15 || s.Registered != 54 || s.RelayedMiB != 106.8 || s.Circuits != 16 || len(s.Countries) != 2 {
		t.Fatalf("parsed %+v", s)
	}
	if _, err := parseStats([]byte(`{"error":"no registry"}`)); err == nil {
		t.Fatal("a relay error must be reported, not shown as zeros")
	}
	if _, err := parseStats([]byte(`<html>decoy</html>`)); err == nil {
		t.Fatal("the decoy page is not a stats answer")
	}
	if got := fmtDur(28920); got != "8h02m" {
		t.Fatalf("fmtDur = %s", got)
	}
}

func TestStatsIsOneRequestToLoopback(t *testing.T) {
	hits := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/stats" {
			t.Errorf("asked %s, want /stats", r.URL.Path)
		}
		w.Write([]byte(`{"registered":1,"alive":1,"exits":1,"countries":["DE"],"priceXnoPerMiB":"0.0005","relayedMiB":1,"circuits":0,"uptimeSeconds":5}`))
	}))
	srv.TLS = &tls.Config{}
	srv.StartTLS()
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	if _, err := fetchStats("127.0.0.1:"+port, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("one request expected, got %d", hits)
	}
}
