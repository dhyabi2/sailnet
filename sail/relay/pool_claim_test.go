package relay

import (
	"testing"
	"time"
)

// claimTopUp plays one "quota exhausted" from the peer the way the extend
// paths handle it (skipped inside a minute of a top-up, else poolExhausted)
// and reports whether it wiped our estimate or started a paid top-up.
func claimTopUp(t *testing.T, s *Server, rel *RelayInfo) bool {
	t.Helper()
	if s.recentTopUp(rel) {
		return false
	}
	s.poolExhausted(rel)
	deadline := time.Now().Add(200 * time.Millisecond)
	for {
		s.mu.Lock()
		p := s.pools[rel.Account]
		started := p.topping || p.credited == p.used
		s.mu.Unlock()
		if started || time.Now().After(deadline) {
			return started
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A downstream relay's "exhausted" is a plaintext, unauthenticated error
// cell. Believed every time, it let a peer we had just prepaid make us pay a
// new pool once a minute while our own metering said the pool was nearly
// untouched. A claim that contradicts our metering is honoured rarely, and
// never right after a payment.
func TestExhaustedClaimDoesNotBuyAPoolEveryMinute(t *testing.T) {
	s := &Server{PoolRaw: mustXNO(t, "0.001"), PoolBytes: 32 << 20}
	rel := &RelayInfo{Account: "nano_peer", MinRate: 50000000}
	size := s.poolSize(rel)
	s.pools = map[string]*pool{rel.Account: {tag: "T"}}
	topUps := 0
	for i := 0; i < 10; i++ {
		// We paid this peer just over a minute ago and have sent it 1 MiB.
		s.mu.Lock()
		p := s.pools[rel.Account]
		p.credited, p.used, p.topping = size, 1<<20, false
		p.lastTop = time.Now().Add(-61 * time.Second)
		s.mu.Unlock()
		if claimTopUp(t, s, rel) {
			topUps++
		}
	}
	if topUps > 0 {
		t.Fatalf("%d of 10 exhaustion claims reset the pool or started a top-up while our metering said %d of %d bytes were left", topUps, size-1<<20, size)
	}
}

// The claim still works when our own metering agrees the pool is spent.
func TestExhaustedClaimTopsUpASpentPool(t *testing.T) {
	s := &Server{PoolRaw: mustXNO(t, "0.001"), PoolBytes: 32 << 20}
	rel := &RelayInfo{Account: "nano_peer", MinRate: 50000000}
	size := s.poolSize(rel)
	s.pools = map[string]*pool{rel.Account: {
		tag: "T", credited: size, used: size - 1<<20,
		lastTop: time.Now().Add(-2 * time.Minute),
	}}
	if !claimTopUp(t, s, rel) {
		t.Fatal("a pool our metering says is spent was not topped up on the peer's exhaustion claim")
	}
}

// A claim against our metering long after the last payment is still paid
// for once (our estimate can drift ahead of the peer's count), and the next
// one waits twice as long.
func TestExhaustedClaimAgainstMeteringBacksOff(t *testing.T) {
	s := &Server{PoolRaw: mustXNO(t, "0.001"), PoolBytes: 32 << 20}
	rel := &RelayInfo{Account: "nano_peer", MinRate: 50000000}
	size := s.poolSize(rel)
	s.pools = map[string]*pool{rel.Account: {
		tag: "T", credited: size, used: 1 << 20,
		lastTop: time.Now().Add(-2 * time.Hour),
	}}
	if !claimTopUp(t, s, rel) {
		t.Fatal("a claim two hours after the last payment was ignored")
	}
	s.mu.Lock()
	p := s.pools[rel.Account]
	p.credited, p.used, p.topping = size, 1<<20, false
	p.lastTop = time.Now().Add(-11 * time.Minute)
	p.claimAt = time.Now().Add(-11 * time.Minute)
	s.mu.Unlock()
	if claimTopUp(t, s, rel) {
		t.Fatal("a second claim 11 minutes after the first was paid for; the backoff should be 20 minutes")
	}
}
