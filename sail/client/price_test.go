package client

import (
	"fmt"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/dhyabi2/sail/relay"
	"github.com/dhyabi2/sail/token"
)

// A relay that raises its price must lose demand and, above the cap, all of it.
func TestPriceCapAndWeight(t *testing.T) {
	m := &manager{}
	all := []*relay.RelayInfo{{Account: "a", MinRate: 200}, {Account: "b", MinRate: 200}, {Account: "c", MinRate: 220}, {Account: "d", MinRate: 2000}}
	median, cap := m.priceCap(all)
	if median != 220 && median != 200 {
		t.Fatalf("median %d", median)
	}
	if cap != 5*median {
		t.Fatalf("default cap %d, want 5x median", cap)
	}
	if all[3].MinRate <= cap {
		t.Fatal("a relay at 10x the median must be over the default cap")
	}
	// A relay still on the previous default price (four times the current
	// one) stays under the cap: our own price change must not exclude it.
	if got := 4 * median; got > cap {
		t.Fatalf("a relay at the previous default price (%d) is over the cap %d", got, cap)
	}
	m.opts.rate = 250
	if _, cap := m.priceCap(all); cap != 250 {
		t.Fatalf("explicit cap %d", cap)
	}
}

// Registrations are cheap and permanent, so anyone can publish a thousand
// records naming a price of almost nothing. They must not drag the median
// down and put every real relay over the cap: the price is read from relays
// that are actually there, which this client decides by its own measurement.
func TestFakeCheapRegistrationsCannotPriceOutRealRelays(t *testing.T) {
	t.Setenv("SAIL_HOME", t.TempDir())
	m := &manager{reg: &relay.Registry{}, rtt: map[string]time.Duration{}, key: EnsureWallet()}
	m.opts.hops = 3
	m.opts.anchor = big.NewInt(1)
	for i := 0; i < 4; i++ { // real relays: probed, market price
		a := fmt.Sprintf("real%d", i)
		m.reg.Add(&relay.RelayInfo{Account: a, MinRate: 50000, Flags: token.FlagPublic | token.FlagExit,
			Country: fmt.Sprintf("C%d", i), ASN: uint32(i + 1), Desc: relay.Descriptor{IP: net.IPv4(10, 0, 0, byte(i)), Port: 443}})
		m.rtt[a] = 50 * time.Millisecond
	}
	for i := 0; i < 200; i++ { // forged records: never seen, priced at nothing
		m.reg.Add(&relay.RelayInfo{Account: fmt.Sprintf("fake%d", i), MinRate: 1, Flags: token.FlagPublic | token.FlagExit,
			Country: "XX", ASN: 9999, Desc: relay.Descriptor{IP: net.IPv4(203, 0, 113, byte(i)), Port: 443}})
	}
	path, err := m.choosePath()
	if err != nil {
		t.Fatalf("no path while four real relays are up: %v", err)
	}
	for _, p := range path {
		if p.MinRate == 1 {
			t.Fatalf("path used a forged record: %s", p.Account)
		}
	}
	if _, cap := m.priceCap(m.reg.All()); cap >= 50000 {
		t.Fatal("this test proves nothing: the median over every record did not exclude the real price")
	}
}
