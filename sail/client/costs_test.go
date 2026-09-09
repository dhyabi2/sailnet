package client

import (
	"math/big"
	"path/filepath"
	"testing"
	"time"
)

func TestCostLedgerAddsUpAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "costs.json")
	l := loadCosts(path)
	xno := func(s string) *big.Int { v, _ := new(big.Int).SetString(s, 10); return v }
	rate := xno("500000000000000000000000000")                                                                     // 0.0005 XNO per MiB, raw
	l.paid("nano_relayA", "AAAA", xno("5000000000000000000000000000"), rate, 10<<20)                               // 0.005 XNO → 10 MiB
	l.paid("nano_relayB", "BBBB", xno("2500000000000000000000000000"), xno("250000000000000000000000000"), 10<<20) // cheaper relay
	l.used("AAAA", 4<<20)                                                                                          // 6 MiB used of 10
	l.used("AAAA", 8<<20)                                                                                          // a stale reading (more remaining) must not lower usage
	l.used("ZZZZ", 0)                                                                                              // unknown tag: ignored

	s := l.summary(24 * time.Hour)
	if s.Anchors != 2 || s.BoughtMiB != 20 || s.UsedMiB != 6 {
		t.Fatalf("summary %+v", s)
	}
	if s.XNO != "0.0075" && s.XNO != "0.00750000" {
		t.Fatalf("paid = %s", s.XNO)
	}
	if s.BestRelay != "nano_relayB" || s.WorstRelay != "nano_relayA" {
		t.Fatalf("best/worst = %s/%s", s.BestRelay, s.WorstRelay)
	}

	again := loadCosts(path)
	if len(again.Entries) != 2 || again.Entries[0].Used != 6<<20 {
		t.Fatal("the ledger must survive a restart")
	}
	if old := again.summary(-time.Second); old.Anchors != 0 {
		t.Fatal("a window that ends before now covers nothing")
	}
}

// The ledger is written from inside the circuit build, which holds m.mu.
// It must never wait for that lock: a client that paid an anchor and then
// hung forever is what this would have caught.
func TestCostLedgerNeverWaitsForTheManagerLock(t *testing.T) {
	t.Setenv("SAIL_HOME", t.TempDir())
	m := &manager{}
	m.mu.Lock()
	defer m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.costs().paid("nano_relay", "TAG", big.NewInt(1), big.NewInt(1), 1)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("costs() blocked on m.mu while the manager held it")
	}
}
