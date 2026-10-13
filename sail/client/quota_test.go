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
	// The entry reporting (hop 0) and out-of-range hops mark nobody: the
	// entry's own failure handling lives where the build fails.
	m2 := mineManager(t, nil)
	c2 := &relay.Circuit{Path: path}
	m2.quotaKilled(c2, 0)
	m2.quotaKilled(c2, 7)
	for i := 0; i < 3; i++ {
		if s := m2.scoreOf(path[i].Account); s != 0.7 {
			t.Fatalf("hop 0 and out-of-range reports mark nobody: %s score %v", path[i].Account, s)
		}
	}
}
