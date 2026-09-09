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

// Run a relay, ride it free: a relay this wallet names as its own is the
// entry whenever it answers, ahead of every other choice.

func TestMyRelayIsTheEntry(t *testing.T) {
	t.Setenv("SAIL_HOME", t.TempDir())
	m := &manager{reg: &relay.Registry{}, rtt: map[string]time.Duration{}, key: EnsureWallet()}
	m.opts.hops = 3
	m.opts.anchor = big.NewInt(1)
	for i := 0; i < 5; i++ {
		a := fmt.Sprintf("nano_real%d", i)
		m.reg.Add(&relay.RelayInfo{Account: a, MinRate: 50000, Flags: token.FlagPublic | token.FlagExit,
			Country: fmt.Sprintf("C%d", i), ASN: uint32(i + 1), Desc: relay.Descriptor{IP: net.IPv4(10, 0, 0, byte(i)), Port: 443}})
		m.rtt[a] = time.Duration(10*(i+1)) * time.Millisecond // real0 is the fastest, so it would win on merit
	}
	m.SetMine("not-an-account, nano_real3\nnano_real9")
	if !m.opts.mine["nano_real3"] || m.opts.mine["not-an-account"] || len(m.opts.mine) != 2 {
		t.Fatalf("SetMine parsed %v", m.opts.mine)
	}
	for i := 0; i < 5; i++ { // the draw is weighted; ours must not be a matter of luck
		path, err := m.choosePath()
		if err != nil {
			t.Fatal(err)
		}
		if path[0].Account != "nano_real3" {
			t.Fatalf("entry = %s, want our own relay nano_real3", path[0].Account)
		}
	}
	// Skipped this round (it refused the owner tag): the ordinary choice applies.
	m.skip = map[string]bool{"nano_real3": true}
	path, err := m.choosePath()
	if err != nil {
		t.Fatal(err)
	}
	if path[0].Account == "nano_real3" {
		t.Fatal("a relay skipped for refusing the owner tag must not be the entry")
	}
}

// A relay listed as ours that refused the owner tag is left alone for an
// hour, not retried on every build; it can never cost XNO, only time.
func TestARefusingRelayOfMineIsRestedForAnHour(t *testing.T) {
	t.Setenv("SAIL_HOME", t.TempDir())
	m := &manager{reg: &relay.Registry{}, rtt: map[string]time.Duration{}, key: EnsureWallet()}
	m.opts.hops = 3
	m.opts.anchor = big.NewInt(1)
	for i := 0; i < 5; i++ {
		a := fmt.Sprintf("nano_real%d", i)
		m.reg.Add(&relay.RelayInfo{Account: a, MinRate: 50000, Flags: token.FlagPublic | token.FlagExit,
			Country: fmt.Sprintf("C%d", i), ASN: uint32(i + 1), Desc: relay.Descriptor{IP: net.IPv4(10, 0, 0, byte(i)), Port: 443}})
		m.rtt[a] = 20 * time.Millisecond
	}
	m.SetMine("nano_real3")
	m.mineRefused = map[string]time.Time{"nano_real3": time.Now().Add(-10 * time.Minute)}
	path, err := m.choosePath()
	if err != nil {
		t.Fatal(err)
	}
	if path[0].Account == "nano_real3" {
		t.Fatal("a relay that refused us ten minutes ago must not be the entry yet")
	}
	m.mineRefused["nano_real3"] = time.Now().Add(-2 * time.Hour)
	m.entry = nil
	if path, _ = m.choosePath(); path[0].Account != "nano_real3" {
		t.Fatalf("after an hour our relay should be tried again, got %s", path[0].Account)
	}
}
