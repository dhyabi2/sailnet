package relay

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dhyabi2/sail/nano"
)

func startRelay(t *testing.T, reg *Registry, idx int, exit bool) (*Server, *RelayInfo, net.Listener) {
	seed := make([]byte, 32)
	seed[0] = byte(idx + 1)
	key, _ := nano.DeriveKey(seed, 0)
	cert, fp, err := SelfSignedCert("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	q, _ := NewQuota("", big.NewInt(1))
	s := &Server{Key: key, Quota: q, TLS: cert, Registry: reg, Exit: exit, AllowPrivate: true, Decoy: "<html>hello</html>"}
	srv := &http.Server{Handler: s.Handler(), TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}, TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){}}
	go srv.ServeTLS(ln, "", "")
	port := ln.Addr().(*net.TCPAddr).Port
	ri := &RelayInfo{Account: key.Address, Pub: key.Public, Country: fmt.Sprintf("C%d", idx), ASN: uint32(idx), MinRate: 1, Flags: 3, Desc: Descriptor{IP: net.ParseIP("127.0.0.1"), Port: uint16(port), CertFP: fp}}
	reg.Add(ri)
	return s, ri, ln
}

func TestThreeHopCircuit(t *testing.T) {
	reg := &Registry{}
	var servers []*Server
	var infos []*RelayInfo
	for i := 0; i < 3; i++ {
		s, ri, _ := startRelay(t, reg, i, i == 2)
		servers = append(servers, s)
		infos = append(infos, ri)
	}
	// Prepay: client anchors the entry; entry/middle have pools at the next hop.
	var tag [32]byte
	copy(tag[:], "e2etest-payment-hash-placeholder")
	servers[0].Quota.Credit(hex.EncodeToString(tag[:]), 50<<20, "")
	servers[1].Quota.Credit(PoolTag(infos[0].Account, infos[1].Account), 50<<20, "")
	servers[2].Quota.Credit(PoolTag(infos[1].Account, infos[2].Account), 50<<20, "")

	// Decoy: a plain HTTPS GET must look like a website.
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := hc.Get("https://" + infos[0].Desc.Addr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "hello") {
		t.Fatalf("decoy not served: %s", body)
	}
	resp2, _ := hc.Get("https://" + infos[0].Desc.Addr() + "/t/0011223344556677/00000000000000000000000000000000")
	if resp2.StatusCode != 404 {
		t.Fatalf("bad token should 404, got %d", resp2.StatusCode)
	}

	// Target web server reachable only via the exit.
	big := strings.Repeat("0123456789abcdef", 12800) // 200 KiB
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/big" {
			w.Write([]byte(big))
			return
		}
		fmt.Fprintf(w, "you reached me from %s", r.RemoteAddr)
	}))
	defer target.Close()

	c, err := Build(infos, tag, 10*time.Second, nil, nil)
	if err != nil {
		t.Fatalf("build failed at hop %d: %v", c.Failed, err)
	}
	defer c.Close()
	if bad := c.Ping(5 * time.Second); bad != -1 {
		t.Fatalf("ping failed at hop %d", bad)
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		st, err := c.Open(addr, 5*time.Second)
		if err != nil {
			return nil, err
		}
		return fakeConn{st}, nil
	}}
	r3, err := (&http.Client{Transport: tr, Timeout: 10 * time.Second}).Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	b3, _ := io.ReadAll(r3.Body)
	if !strings.Contains(string(b3), "you reached me") {
		t.Fatalf("bad body %s", b3)
	}
	t.Logf("through 3 hops: %s", b3)
	r4, err := (&http.Client{Transport: tr, Timeout: 20 * time.Second}).Get(target.URL + "/big")
	if err != nil {
		t.Fatal(err)
	}
	b4, _ := io.ReadAll(r4.Body)
	if string(b4) != big {
		t.Fatalf("large body corrupted: got %d bytes, want %d", len(b4), len(big))
	}
	t.Logf("200 KiB body intact through 3 hops")
	q, err := c.QueryQuota(5 * time.Second)
	if err != nil || q <= 0 || q >= 50<<20 {
		t.Fatalf("quota query: %d %v", q, err)
	}
	t.Logf("entry quota remaining: %d bytes", q)

	// Failure attribution: no quota at the middle hop → build must blame hop 1.
	var tag2 [32]byte
	copy(tag2[:], "second-payment-hash-placeholder!")
	servers[0].Quota.Credit(hex.EncodeToString(tag2[:]), 50<<20, "")
	reg2 := &Registry{}
	_, dead, ln := startRelay(t, reg2, 9, false)
	ln.Close()
	path := []*RelayInfo{infos[0], dead, infos[2]}
	reg.Add(dead)
	c2, err := Build(path, tag2, 5*time.Second, nil, nil)
	if err == nil {
		t.Fatal("expected failure")
	}
	if c2.Failed != 1 {
		t.Fatalf("expected hop 1 blamed, got %d (%v)", c2.Failed, err)
	}
	t.Logf("attributed correctly: %v", err)
	c2.Close()
}

type fakeConn struct{ *Stream }

func (fakeConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (fakeConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (fakeConn) SetDeadline(time.Time) error      { return nil }
func (fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (fakeConn) SetWriteDeadline(time.Time) error { return nil }
