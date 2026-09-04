package relay

import (
	"context"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A circuit whose entry link dies is reattached to a new connection with
// the same keys: streams opened afterwards work, nothing was rebuilt or
// paid again, and the exit never saw a change.
func TestResumeAfterLinkDrop(t *testing.T) {
	reg := &Registry{}
	var infos []*RelayInfo
	var servers []*Server
	for i := 0; i < 2; i++ {
		s, ri, _ := startRelay(t, reg, 20+i, i == 1)
		servers = append(servers, s)
		infos = append(infos, ri)
	}
	var tag [32]byte
	copy(tag[:], "resume-test-payment-hash-placeho")
	servers[0].Quota.Credit(hex.EncodeToString(tag[:]), 50<<20, "")
	servers[1].Quota.Credit(PoolTag(infos[0].Account, infos[1].Account), 50<<20, "")

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello "+r.URL.Path)
	}))
	defer target.Close()

	c, err := Build(infos, tag, 10*time.Second, nil, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer c.Close()
	get := func(path string) (string, error) {
		tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			st, err := c.Open(addr, 5*time.Second)
			if err != nil {
				return nil, err
			}
			return fakeConn{st}, nil
		}, DisableKeepAlives: true}
		r, err := (&http.Client{Transport: tr, Timeout: 10 * time.Second}).Get(target.URL + path)
		if err != nil {
			return "", err
		}
		b, _ := io.ReadAll(r.Body)
		return string(b), nil
	}
	if b, err := get("/before"); err != nil || !strings.Contains(b, "hello /before") {
		t.Fatalf("before drop: %q %v", b, err)
	}
	created := servers[0].Metrics.Circuits.Load()

	// The entry link dies underneath the circuit (network flap).
	c.conn.Close()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if c.Closed() {
			t.Fatal("circuit closed instead of resuming")
		}
		if b, err := get("/after"); err == nil && strings.Contains(b, "hello /after") {
			if servers[0].Metrics.Circuits.Load() != created {
				t.Fatal("a new circuit was created; expected a resume")
			}
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("stream did not work after the link drop")
}
