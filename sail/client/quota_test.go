package client

import (
	"strings"
	"testing"

	"github.com/dhyabi2/sail/relay"
)

// A hop that reports the circuit's pool exhausted is reporting its payer: the
// money that should have prepaid it never arrived. The client must mark that
// payer down and wait out the build fuse, so a broken payer is routed around
// instead of meeting every new stream with a fresh circuit that dies on its
// first byte.

func TestQuotaExhaustionMarksThePayerAndBacksOff(t *testing.T) {
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
	// The next build pays the five-second fuse instead of rebuilding the same
	// dead path for every stream that arrives.
	if _, err := m.circuit(); err == nil || !strings.Contains(err.Error(), "retrying shortly") {
		t.Fatalf("want the backoff error, got %v", err)
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
