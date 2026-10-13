package client

import (
	"fmt"
	"testing"

	"github.com/dhyabi2/sail/relay"
)

// A hop that reports the circuit's pool exhausted is reporting its payer: the
// money that should have prepaid it never arrived. The client must mark that
// payer down and route around it on the next build when the network is large
// enough, instead of meeting every new stream with a fresh circuit that dies
// on its first byte.

func TestQuotaExhaustionMarksThePayerWithoutBackoff(t *testing.T) {
	m := mineManager(t, nil)
	path := []*relay.RelayInfo{{Account: "nano_real1"}, {Account: "nano_real2"}, {Account: "nano_real3"}}
	c := &relay.Circuit{Path: path}
	m.quotaKilled(c, 2) // the exit reported: its payer is the middle
	if s := m.scoreOf("nano_real2"); s >= 0.7 {
		t.Fatalf("the payer's score should drop, got %v", s)
	}
	for _, untouched := range []string{"nano_real1", "nano_real3"} {
		if s := m.scoreOf(untouched); s != 0.7 {
			t.Fatalf("%s is not the payer, score %v", untouched, s)
		}
	}
	// Nothing waits: the rebuild starts at once.
	if !m.lastFail.IsZero() {
		t.Fatal("a relay's dry pool must not put the next build behind the five-second fuse")
	}
	// The next build leaves the dry pool's payer out of the path, once.
	if skip := m.freshSkip(); !skip["nano_real2"] || len(skip) != 1 {
		t.Fatalf("the next build should skip the payer, got %v", skip)
	}
	if skip := m.freshSkip(); len(skip) != 0 {
		t.Fatalf("the skip is for one build only, got %v", skip)
	}
	// When the payer is our entry, the reporter is routed around instead:
	// a new entry would mean a new anchor.
	m3 := mineManager(t, nil)
	m3.quotaKilled(&relay.Circuit{Path: path}, 1)
	if skip := m3.freshSkip(); !skip["nano_real2"] || skip["nano_real1"] {
		t.Fatalf("an entry-paid pool should skip the reporting middle, got %v", skip)
	}
	// The entry reporting (hop 0: our own anchor ran out) and out-of-range
	// hops mark nobody and set no backoff: the rebuild pays again at once.
	m2 := mineManager(t, nil)
	c2 := &relay.Circuit{Path: path}
	m2.quotaKilled(c2, 0)
	m2.quotaKilled(c2, 7)
	m2.quotaKilled(c2, -1)
	for i := 0; i < 3; i++ {
		if s := m2.scoreOf(path[i].Account); s != 0.7 {
			t.Fatalf("hop 0 and out-of-range reports mark nobody: %s score %v", path[i].Account, s)
		}
	}
	if !m2.lastFail.IsZero() {
		t.Fatal("hop 0 and out-of-range reports must not make the next build wait (the five-second fuse)")
	}
	if skip := m2.freshSkip(); len(skip) != 0 {
		t.Fatalf("hop 0 and out-of-range reports skip nobody, got %v", skip)
	}
	// A late report from a circuit that has already been replaced says
	// nothing about the next build.
	m4 := mineManager(t, nil)
	m4.cur = &relay.Circuit{Path: path}
	m4.quotaKilled(&relay.Circuit{Path: path}, 2)
	if !m4.lastFail.IsZero() || len(m4.freshSkip()) != 0 || m4.scoreOf("nano_real2") != 0.7 {
		t.Fatal("a stale circuit's quota kill must not touch the current manager state")
	}
}

func onPath(path []*relay.RelayInfo, acct string) bool {
	for _, r := range path {
		if r.Account == acct {
			return true
		}
	}
	return false
}

// On a network with room to spare the dry pool's payer is left out of the
// next build; on three relays for three hops there is no path without it, so
// the build goes through it instead of failing.
func TestDryPoolSkipYieldsWhenTheNetworkIsTooSmall(t *testing.T) {
	path := []*relay.RelayInfo{{Account: "nano_real0"}, {Account: "nano_real1"}, {Account: "nano_real2"}}
	for i := 0; i < 10; i++ {
		m := mineManager(t, nil) // six relays
		m.quotaKilled(&relay.Circuit{Path: path}, 2)
		m.skip = m.freshSkip()
		p, err := m.choosePathAroundDry()
		if err != nil {
			t.Fatalf("six relays: %v", err)
		}
		if onPath(p, "nano_real1") {
			t.Fatalf("six relays: the dry pool's payer should be skipped, got %v", names(p))
		}
	}
	for i := 0; i < 10; i++ {
		m := mineManager(t, nil)
		small := &relay.Registry{}
		for _, a := range []string{"nano_real0", "nano_real1", "nano_real2"} {
			small.Add(m.reg.Get(a))
		}
		m.reg = small
		m.quotaKilled(&relay.Circuit{Path: path}, 2)
		m.skip = m.freshSkip()
		p, err := m.choosePathAroundDry()
		if err != nil {
			t.Fatalf("three relays: the build must ignore the skip, not fail: %v", err)
		}
		if len(p) != 3 || !onPath(p, "nano_real1") {
			t.Fatalf("three relays: want the only path, through the payer, got %v", names(p))
		}
		if !m.lastFail.IsZero() {
			t.Fatal("three relays: no backoff")
		}
	}
}

func names(p []*relay.RelayInfo) string {
	s := ""
	for _, r := range p {
		s += fmt.Sprintf("%s ", r.Account)
	}
	return s
}
