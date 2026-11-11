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
	s.poolExhausted(rel, "T")
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
	p.credited, p.used, p.topping, p.wiped = size, 1<<20, false, false
	p.lastTop = time.Now().Add(-11 * time.Minute)
	p.claimAt = time.Now().Add(-11 * time.Minute)
	s.mu.Unlock()
	if claimTopUp(t, s, rel) {
		t.Fatal("a second claim 11 minutes after the first was paid for; the backoff should be 20 minutes")
	}
}

// Two "exhausted" cells in one batch (or from two circuits) are one claim.
// The second one finds the estimate we just wiped ourselves, which must not
// count as our metering agreeing with the peer and reset the backoff: that
// let a peer make us pay a new pool every ten minutes.
func TestExhaustedClaimsInOneBatchKeepTheBackoff(t *testing.T) {
	s := &Server{PoolRaw: mustXNO(t, "0.001"), PoolBytes: 32 << 20}
	rel := &RelayInfo{Account: "nano_peer", MinRate: 50000000}
	size := s.poolSize(rel)
	s.pools = map[string]*pool{rel.Account: {
		tag: "T", credited: size, used: 1 << 20,
		lastTop: time.Now().Add(-2 * time.Hour),
	}}
	for round := 0; round < 6; round++ {
		// Our wipe just zeroed lastTop, so recentTopUp lets every cell of the
		// batch through: play them straight into poolExhausted.
		s.poolExhausted(rel, "T")
		s.poolExhausted(rel, "T")
		// The top-up it bought lands, as topUpPool records it, and the peer
		// claims again eleven minutes later with our metering nearly full.
		s.mu.Lock()
		p := s.pools[rel.Account]
		p.credited, p.used, p.topping, p.wiped = size, 1<<20, false, false
		p.lastTop = time.Now().Add(-11 * time.Minute)
		p.claimAt = time.Now().Add(-11 * time.Minute)
		s.mu.Unlock()
		if claimTopUp(t, s, rel) {
			t.Fatalf("round %d: a claim 11 minutes after one we paid for was paid again; two cells in one batch reset the backoff", round)
		}
		// Let the backoff run out (claims stay counted) for the next round.
		s.mu.Lock()
		p.lastTop = time.Now().Add(-48 * time.Hour)
		p.claimAt = time.Now().Add(-48 * time.Hour)
		s.mu.Unlock()
	}
}

// A claim made against a pool tag we have since replaced is about the old
// pool and buys nothing.
func TestExhaustedClaimOnAStaleTagIsIgnored(t *testing.T) {
	s := &Server{PoolRaw: mustXNO(t, "0.001"), PoolBytes: 32 << 20}
	rel := &RelayInfo{Account: "nano_peer", MinRate: 50000000}
	size := s.poolSize(rel)
	s.pools = map[string]*pool{rel.Account: {
		tag: "NEW", credited: size, used: 1 << 20,
		lastTop: time.Now().Add(-2 * time.Hour),
	}}
	s.poolExhausted(rel, "OLD")
	s.mu.Lock()
	p := s.pools[rel.Account]
	wiped, claims := p.credited == p.used, p.claims
	s.mu.Unlock()
	if wiped || claims != 0 {
		t.Fatalf("a claim on a replaced tag wiped the pool (%v) or spent backoff (claims=%d)", wiped, claims)
	}
}

// The backoff survives a restart: a peer cannot reset it by waiting for us
// to reboot.
func TestExhaustedClaimBackoffIsSaved(t *testing.T) {
	file := t.TempDir() + "/pools.json"
	at := time.Now().Add(-time.Minute).Round(time.Second)
	s := &Server{PoolsFile: file}
	s.pools = map[string]*pool{"nano_peer": {tag: "T", credited: 5, used: 5, claims: 3, claimAt: at, wiped: true}}
	s.mu.Lock()
	s.savePoolsLocked()
	s.mu.Unlock()
	r := &Server{PoolsFile: file}
	r.LoadPools()
	p := r.pools["nano_peer"]
	if p == nil || p.claims != 3 || !p.claimAt.Equal(at) || !p.wiped {
		t.Fatalf("restored pool %+v; want claims=3 claimAt=%v wiped", p, at)
	}
}

// A claim timed into the window where our own metering is honestly under a
// quarter (a meterPool refill in flight, or a count the peer pushed down
// with cells of its own) is paid, but must not reset the backoff: that let
// a peer force a new pool ten minutes after every payment.
func TestExhaustedClaimInTheRefillWindowKeepsTheBackoff(t *testing.T) {
	s := &Server{PoolRaw: mustXNO(t, "0.001"), PoolBytes: 32 << 20}
	rel := &RelayInfo{Account: "nano_peer", MinRate: 50000000}
	size := s.poolSize(rel)
	// Three unbacked claims already paid for: the next waits an hour.
	s.pools = map[string]*pool{rel.Account: {
		tag: "T", credited: size, used: size - size/8,
		lastTop: time.Now().Add(-5 * time.Minute), claims: 3, claimAt: time.Now().Add(-5 * time.Minute),
		topping: true,
	}}
	s.poolExhausted(rel, "T")
	s.mu.Lock()
	p := s.pools[rel.Account]
	if p.claims != 3 {
		t.Errorf("a claim in the refill window changed the paid-claim count from 3 to %d", p.claims)
	}
	// The refill lands 11 minutes ago, as topUpPool records it.
	p.tag, p.credited, p.used, p.topping, p.wiped = "T2", size, 1<<20, false, false
	p.lastTop = time.Now().Add(-11 * time.Minute)
	p.claimAt = time.Now().Add(-16 * time.Minute)
	s.mu.Unlock()
	if claimTopUp2(t, s, rel, "T2") {
		t.Fatal("an unbacked claim 11 minutes after a payment was paid although three had been paid before (backoff an hour)")
	}
}

// The backoff is forgotten only after a week without a paid claim.
func TestExhaustedClaimBackoffDecaysAfterAQuietWeek(t *testing.T) {
	s := &Server{PoolRaw: mustXNO(t, "0.001"), PoolBytes: 32 << 20}
	rel := &RelayInfo{Account: "nano_peer", MinRate: 50000000}
	size := s.poolSize(rel)
	s.pools = map[string]*pool{rel.Account: {
		tag: "T", credited: size, used: 1 << 20, claims: 7,
		lastTop: time.Now().Add(-8 * 24 * time.Hour), claimAt: time.Now().Add(-8 * 24 * time.Hour),
	}}
	s.poolExhausted(rel, "T")
	s.mu.Lock()
	claims := s.pools[rel.Account].claims
	s.mu.Unlock()
	if claims != 1 {
		t.Fatalf("after a quiet week a paid claim left claims=%d; want the backoff started over at 1", claims)
	}
}

// claimTopUp2 is claimTopUp with the pool tag the claim was made against.
func claimTopUp2(t *testing.T, s *Server, rel *RelayInfo, tag string) bool {
	t.Helper()
	if s.recentTopUp(rel) {
		return false
	}
	s.poolExhausted(rel, tag)
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pools[rel.Account]
	return p.topping || p.credited == p.used
}

// An honest peer whose real "exhausted" our metering does not back (our
// count ran ahead of its own, say after a restart lost the unsaved part)
// must not be left unpaid for many hours: the link to it is down until the
// claim is paid. However many claims we paid before, one an hour after the
// last payment is paid again.
func TestExhaustedClaimBackoffIsAtMostAnHour(t *testing.T) {
	s := &Server{PoolRaw: mustXNO(t, "0.001"), PoolBytes: 32 << 20}
	rel := &RelayInfo{Account: "nano_peer", MinRate: 50000000}
	size := s.poolSize(rel)
	s.pools = map[string]*pool{rel.Account: {
		tag: "T", credited: size, used: 1 << 20, claims: 7,
		lastTop: time.Now().Add(-61 * time.Minute), claimAt: time.Now().Add(-61 * time.Minute),
	}}
	if !claimTopUp(t, s, rel) {
		t.Fatal("a claim 61 minutes after the last paid one was ignored; the backoff must stop at an hour")
	}
	// It still never pays sooner than that, however long it has been capped.
	s.mu.Lock()
	p := s.pools[rel.Account]
	p.credited, p.used, p.topping, p.wiped = size, 1<<20, false, false
	p.lastTop = time.Now().Add(-59 * time.Minute)
	p.claimAt = time.Now().Add(-59 * time.Minute)
	s.mu.Unlock()
	if claimTopUp(t, s, rel) {
		t.Fatal("a claim 59 minutes after the last paid one was paid; the capped backoff is an hour")
	}
}
