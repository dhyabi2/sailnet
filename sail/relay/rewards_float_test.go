package relay

import (
	"math/big"
	"testing"

	"github.com/dhyabi2/sail/token"
)

// The float pays peers, so it is priced at what peers charge. A relay that
// raises its own price must not thereby raise the bar its own earnings have
// to clear — that is what stopped payouts on 2026-09-07: the price went up
// ten times and every operator's payout threshold went up with it.

func rate(t *testing.T, xnoPerMiB string) uint32 {
	t.Helper()
	r, err := token.RateFromXNO(xnoPerMiB)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func floatServer(t *testing.T, peerRates ...string) *Server {
	t.Helper()
	reg := &Registry{relays: map[string]*RelayInfo{}}
	for i, r := range peerRates {
		acct := "nano_peer" + string(rune('a'+i))
		reg.relays[acct] = &RelayInfo{Account: acct, MinRate: rate(t, r)}
	}
	return &Server{
		Registry:  reg,
		PoolRaw:   mustXNO(t, "0.001"),
		PoolBytes: 32 << 20,
	}
}

func TestFloatIsPricedAtThePeersRateNotOurOwn(t *testing.T) {
	// Five peers at the going rate. Our own selling price is not an input to
	// Float at all any more — that is the whole fix — so the only thing that
	// can move this number is the peers.
	s := floatServer(t, "0.00005", "0.00005", "0.00005", "0.00005", "0.00005")

	// Eight top-ups of 32 MiB at 0.00005 XNO/MiB = 0.0128 XNO. That is the
	// figure payouts used before the price raise, and it must survive it.
	want := new(big.Int).Mul(RawFor(32<<20, token.RateToRaw(rate(t, "0.00005"))), big.NewInt(8))
	if got := s.Float(8); got.Cmp(want) != 0 {
		t.Fatalf("float = %s XNO, want %s XNO", formatXNO(got), formatXNO(want))
	}
}

func TestFloatFollowsThePeersWhenTheyReprice(t *testing.T) {
	s := floatServer(t, "0.00005", "0.00005", "0.00005")
	before := s.Float(8)
	for _, r := range s.Registry.relays {
		r.MinRate = rate(t, "0.0005") // the market itself gets ten times more expensive
	}
	after := s.Float(8)
	if after.Cmp(before) <= 0 {
		t.Fatalf("peers repriced up but the float did not follow: %s -> %s", formatXNO(before), formatXNO(after))
	}
}

func TestFloatIgnoresOneAbsurdlyExpensivePeer(t *testing.T) {
	sane := floatServer(t, "0.00005", "0.00005", "0.00005", "0.00005", "0.00005")
	want := sane.Float(8)
	gouged := floatServer(t, "0.00005", "0.00005", "0.00005", "0.00005", "0.05") // one peer asking a thousand times the going rate
	if got := gouged.Float(8); got.Cmp(want) != 0 {
		t.Fatalf("one gouging peer moved the float: %s, want %s", formatXNO(got), formatXNO(want))
	}
}

func TestFloatFallsBackToThePoolFloorWithNoRegistry(t *testing.T) {
	s := &Server{PoolRaw: mustXNO(t, "0.001"), PoolBytes: 32 << 20}
	want := new(big.Int).Mul(mustXNO(t, "0.001"), big.NewInt(8))
	if got := s.Float(8); got.Cmp(want) != 0 {
		t.Fatalf("cold start float = %s, want %s", formatXNO(got), formatXNO(want))
	}
}
