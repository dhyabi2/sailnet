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

	"github.com/dhyabi2/sail/nano"
	"github.com/dhyabi2/sail/wire"
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
	return &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{
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

// oneHopOverdraft pays an exit directly for a one-hop circuit with a tag
// owned by payer (the wallet that signs the CREATE), lets the pool run
// almost dry, and downloads 64 KiB through it. It returns how much arrived.
func oneHopOverdraft(t *testing.T, payer *nano.Key, payerIP string, onBuilt func(*Circuit)) (got, want int) {
	t.Helper()
	reg := &Registry{}
	exit, exitInfo, _ := startRelay(t, reg, 0, true)
	// The payer's wallet is on the ledger list: anyone can register one.
	reg.Add(&RelayInfo{Account: payer.Address, Pub: payer.Public, MinRate: 1, Flags: 3, Desc: Descriptor{IP: net.ParseIP(payerIP), Port: 443}})
	var tag [32]byte
	copy(tag[:], "one-hop-direct-payment-block-hsh") // a payment block's hash, not a static pool tag
	tagHex := strings.ToUpper(hex.EncodeToString(tag[:]))
	// What the payment path credits (creditFromLedgerImpl / acceptSuppliedPayment):
	// the bytes paid for, owned by the wallet that signed the send.
	exit.Quota.Credit(tagHex, 16<<10, hex.EncodeToString(payer.Public[:]))

	target, body := overdraftTarget(t)
	defer target.Close()
	c, err := Build([]*RelayInfo{exitInfo}, tag, 10*time.Second, nil, func(pub, tg [32]byte) []byte { return SignCreate(payer, pub, tg) })
	if err != nil {
		t.Fatalf("build failed at hop %d: %v", c.Failed, err)
	}
	defer c.Close()
	if onBuilt != nil {
		onBuilt(c)
	}
	resp, err := overdraftClient(c).Get(target.URL)
	if err != nil {
		return 0, len(body)
	}
	b, _ := io.ReadAll(resp.Body)
	return len(b), len(body)
}

func TestClientPaidOneHopTagGetsNoOverdraftEvenWhenItsWalletIsListed(t *testing.T) {
	seed := make([]byte, 32)
	seed[0] = 77
	client, _ := nano.DeriveKey(seed, 0)
	// The client listed its wallet, but it is not a relay extending to us:
	// its circuit arrives from its own address, not the listed one.
	got, want := oneHopOverdraft(t, client, "203.0.113.7", nil)
	// The exit meters each read after forwarding it, so one read (up to
	// 32 cells of data) can land past the boundary, plus one in flight.
	if limit := 16<<10 + 2*32*wire.MaxData; got >= want || got > limit {
		t.Fatalf("a client-paid tag rode the relay overdraft: %d of %d bytes downloaded on 16 KiB paid", got, want)
	}
}

func TestListedRelayPoolPaidByLedgerBlockRidesTheOverdraft(t *testing.T) {
	// The production pool: a block hash, prepaid by a listed relay, arriving
	// on a connection from that relay's listed address.
	seed := make([]byte, 32)
	seed[0] = 78
	payer, _ := nano.DeriveKey(seed, 0)
	got, want := oneHopOverdraft(t, payer, "127.0.0.1", nil)
	if got != want {
		t.Fatalf("a listed relay's pool died in overdraft: got %d of %d bytes", got, want)
	}
}

// The quota-killed handler runs off the circuit's read loop: the client's
// handler takes the manager's lock, which a circuit build holds throughout,
// so a handler that waits must not hold up the read loop — or the Close that
// follows it. Set after Build, while the read loop runs (race detector).
func TestQuotaKilledHandlerDoesNotBlockTheReadLoop(t *testing.T) {
	seed := make([]byte, 32)
	seed[0] = 79
	client, _ := nano.DeriveKey(seed, 0)
	release := make(chan struct{})
	defer close(release)
	calledAt := make(chan time.Time, 4)
	closedAt := make(chan time.Time, 1)
	oneHopOverdraft(t, client, "203.0.113.7", func(c *Circuit) {
		c.SetQuotaHandlers(func(int64) {}, func(hop int) {
			if hop != 0 {
				t.Errorf("the exit is hop 0 of a one-hop circuit, handler got %d", hop)
			}
			calledAt <- time.Now()
			<-release // a manager busy building
		})
		go func() {
			for !c.Closed() {
				time.Sleep(10 * time.Millisecond)
			}
			closedAt <- time.Now()
		}()
	})
	var called time.Time
	select {
	case called = <-calledAt:
	case <-time.After(10 * time.Second):
		t.Fatal("the quota-killed handler was never called")
	}
	closed := <-closedAt // the helper closes the circuit on return at the latest
	if d := closed.Sub(called); d > 3*time.Second {
		t.Fatalf("a blocked quota-killed handler held up the circuit's close for %v", d)
	}
}
