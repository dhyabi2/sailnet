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

// The exit's read path meters every chunk it forwards against the pool its
// payer prepaid. A pool can be a little negative while its top-up block is in
// flight — overdraftOK exists for exactly that, and the forward and backward
// paths already ride it out. These tests pin the download path to the same
// behaviour: a listed payer's pool in overdraft must carry the stream; a pool
// whose payer is not on the ledger must still kill the circuit, or the
// overdraft would be free money.

func overdraftTarget(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	body := strings.Repeat("0123456789abcdef", 4096) // 64 KiB
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	return target, body
}

func overdraftClient(c *Circuit) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			st, err := c.Open(addr, 5*time.Second)
			if err != nil {
				return nil, err
			}
			return fakeConn{st}, nil
		}}}
}

func TestExitRidesPoolOverdraftWhileRefillIsInFlight(t *testing.T) {
	reg := &Registry{}
	var servers []*Server
	var infos []*RelayInfo
	for i := 0; i < 3; i++ {
		s, ri, _ := startRelay(t, reg, i, i == 2)
		servers = append(servers, s)
		infos = append(infos, ri)
	}
	var tag [32]byte
	copy(tag[:], "overdraft-ride-payment-placeholder")
	servers[0].Quota.Credit(hex.EncodeToString(tag[:]), 50<<20, "")
	servers[1].Quota.Credit(PoolTag(infos[0].Account, infos[1].Account), 50<<20, "")
	// The exit's pool, prepaid by a listed relay, is already a little
	// negative: its top-up needs a Nano block and has not landed yet.
	exitTag := PoolTag(infos[1].Account, infos[2].Account)
	servers[2].Quota.Credit(exitTag, 1<<20, hex.EncodeToString(infos[1].Pub[:]))
	servers[2].Quota.Consume(exitTag, (1<<20)+2048)

	target, want := overdraftTarget(t)
	defer target.Close()

	c, err := Build(infos, tag, 10*time.Second, nil, nil)
	if err != nil {
		t.Fatalf("build failed at hop %d: %v", c.Failed, err)
	}
	defer c.Close()
	resp, err := overdraftClient(c).Get(target.URL)
	if err != nil {
		t.Fatalf("stream died in pool overdraft: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != want {
		t.Fatalf("body corrupted through overdrafting exit: got %d bytes, want %d", len(got), len(want))
	}
	if c.Closed() {
		t.Fatal("the circuit must survive a pool whose refill is in flight")
	}
}

func TestExitStillKillsOverdraftOfUnlistedPayer(t *testing.T) {
	reg := &Registry{}
	var servers []*Server
	var infos []*RelayInfo
	for i := 0; i < 2; i++ { // entry and middle share the ledger
		s, ri, _ := startRelay(t, reg, i, false)
		servers = append(servers, s)
		infos = append(infos, ri)
	}
	// The exit knows its payer only by gossip, say — it is in no registry the
	// exit trusts — so its overdraft is not covered: the first bytes past the
	// pool must still close the circuit instead of riding indefinitely on a
	// stranger's word. (The shared registry still lists the exit, so the
	// middle can extend to it without a ledger lookup.)
	s2, ri2, _ := startRelay(t, &Registry{}, 2, true)
	servers = append(servers, s2)
	infos = append(infos, ri2)
	reg.Add(ri2)
	var tag [32]byte
	copy(tag[:], "overdraft-kill-payment-placeholder")
	servers[0].Quota.Credit(hex.EncodeToString(tag[:]), 50<<20, "")
	servers[1].Quota.Credit(PoolTag(infos[0].Account, infos[1].Account), 50<<20, "")
	exitTag := PoolTag(infos[1].Account, infos[2].Account)
	servers[2].Quota.Credit(exitTag, 8<<10, hex.EncodeToString(infos[1].Pub[:]))

	target, want := overdraftTarget(t)
	defer target.Close()

	c, err := Build(infos, tag, 10*time.Second, nil, nil)
	if err != nil {
		t.Fatalf("build failed at hop %d: %v", c.Failed, err)
	}
	defer c.Close()
	resp, err := overdraftClient(c).Get(target.URL)
	if err == nil {
		got, _ := io.ReadAll(resp.Body)
		if len(got) == len(want) {
			t.Fatal("an uncovered overdraft must not carry the whole stream")
		}
	}
}
