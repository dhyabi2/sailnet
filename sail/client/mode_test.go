package client

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/dhyabi2/sail/relay"
	"github.com/dhyabi2/sail/token"
)

// The Network switch: Direct is one free hop through a relay of ours; Open
// never treats ours as special; My relays is the exit-or-middle behaviour.

func TestDirectIsOneHopThroughOurRelayAndNothingElse(t *testing.T) {
	m := mineManager(t, nil)
	m.SetMine("nano_real0")
	m.SetMode(ModeDirect)
	if m.opts.hops != 1 {
		t.Fatalf("Direct is one hop, got %d", m.opts.hops)
	}
	path, err := m.choosePath()
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 1 || path[0].Account != "nano_real0" {
		t.Fatalf("Direct must be exactly our relay, got %v", path)
	}
	if tags := m.hopTags(path); len(tags) != 0 {
		t.Fatal("no later hops, so no hop tags; the owner tag goes on the CREATE itself")
	}
	// Leaving Direct restores the configured hops.
	m.SetMode(ModeMine)
	if m.opts.hops != 3 {
		t.Fatalf("hops should be back to 3, got %d", m.opts.hops)
	}
}

func TestDirectRefusesRatherThanPayingStrangers(t *testing.T) {
	m := mineManager(t, map[string]uint16{"nano_real0": uint16(token.FlagPublic)}) // ours cannot exit
	m.SetMine("nano_real0")
	m.SetMode(ModeDirect)
	if _, err := m.choosePath(); err == nil {
		t.Fatal("with no usable relay of ours, Direct must say so, not silently pay a stranger")
	}
}

func TestOpenNetworkNeverUsesOurRelays(t *testing.T) {
	m := mineManager(t, nil)
	m.SetMine("nano_real0")
	m.SetMode(ModeOpen)
	for i := 0; i < 6; i++ {
		path, err := m.choosePath()
		if err != nil {
			t.Fatal(err)
		}
		if path[len(path)-1].Account == "nano_real0" && len(m.hopTags(path)) != 0 {
			t.Fatal("in Open network our relay may be chosen like any other, but never with an owner tag")
		}
		if len(m.hopTags(path)) != 0 {
			t.Fatal("Open network sends no owner tags")
		}
	}
}

func TestUnknownModeIsMine(t *testing.T) {
	m := mineManager(t, nil)
	m.SetMode("whatever")
	if m.Mode() != ModeMine {
		t.Fatalf("mode = %s", m.Mode())
	}
}

// Rule 6: what the pairing path tells the user never names an address. The
// relay here is a black hole, so the dial fails the way it did on a phone
// whose relay had port 443 closed — and the message stays a message.
func TestPairingErrorNamesNoAddress(t *testing.T) {
	m := &manager{reg: &relay.Registry{}, key: EnsureWallet(), opts: clientOpts{timeout: time.Second}}
	ri := &relay.RelayInfo{Account: "nano_1blackhole11111111111111111111111111111111111111111111111111", Desc: relay.Descriptor{IP: net.ParseIP("192.0.2.77").To4(), Port: 443}}
	m.reg.Add(ri)
	err := m.PairRelay(ri.Account, "123456")
	if err == nil {
		t.Fatal("a relay nobody can reach must not pair")
	}
	if strings.Contains(err.Error(), "192.0.2.77") || strings.Contains(Redact(err.Error()), "device") {
		t.Fatalf("the error carries an address: %q", err)
	}
}

// A relay of ours that is restarting is not a relay that refused us: only
// the relay's own answer rests it for an hour, never a dropped connection.
func TestOnlyARelaysOwnAnswerCountsAsRefusal(t *testing.T) {
	for _, e := range []string{"hop 0: no CREATED: EOF", "hop 0 (nano_1abc…): dial tcp: i/o timeout", "hop 0: no CREATED: read: connection reset"} {
		if ownerRefusal(errors(e)) {
			t.Fatalf("%q is not a refusal", e)
		}
	}
	if !ownerRefusal(errors("hop 0 refused: unknown payment tag")) {
		t.Fatal("the relay's own refusal must count")
	}
}

// Direct uses the paired relay even when this client has scored it down or
// never probed it: it is the user's own, not a stranger to be judged.
func TestDirectUsesOurRelayWhateverItsScore(t *testing.T) {
	m := mineManager(t, nil)
	m.SetMine("nano_real5")
	m.SetMode(ModeDirect)
	delete(m.rtt, "nano_real5") // never probed
	m.score = map[string]float64{"nano_real5": 0.05}
	m.scoreAt = map[string]time.Time{"nano_real5": time.Now()}
	path, err := m.choosePath()
	if err != nil {
		t.Fatalf("Direct must still use our relay: %v", err)
	}
	if len(path) != 1 || path[0].Account != "nano_real5" {
		t.Fatalf("got %v", path)
	}
}
