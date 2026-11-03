package relay

import (
	"math/big"
	"strings"
	"testing"

	"github.com/dhyabi2/sail/nano"
)

// On 2026-10-28 three relays registered at 0.02 XNO/MiB, forty times the
// going rate, and every relay that prepaid them sent its whole wallet in one
// top-up. These tests pin the two guards that stop that.

func TestNextHopCapIsFiveTimesTheMedian(t *testing.T) {
	s := floatServer(t, "0.0005", "0.0005", "0.0005", "0.0005", "0.02")
	if got, want := s.nextHopCap(), 5*rate(t, "0.0005"); got != want {
		t.Fatalf("cap = %s, want %s", rateXNO(got), rateXNO(want))
	}
}

func TestOverPricedPeerIsRefusedAndFairOnesAreNot(t *testing.T) {
	s := floatServer(t, "0.0005", "0.0005", "0.0005", "0.0005", "0.02", "0.002")
	for acct, rel := range s.Registry.relays {
		over, _ := s.overPriced(rel)
		gouging := rel.MinRate == rate(t, "0.02")
		if over != gouging {
			t.Errorf("%s at %s: overPriced = %v, want %v", acct, rateXNO(rel.MinRate), over, gouging)
		}
	}
}

func TestOperatorCapOverridesTheMedian(t *testing.T) {
	s := floatServer(t, "0.0005", "0.0005", "0.0005", "0.0005", "0.02")
	s.MaxNextRate = rate(t, "0.05")
	if over, _ := s.overPriced(s.Registry.relays["nano_peere"]); over {
		t.Fatal("an operator who set --max-next-rate 0.05 must still prepay a 0.02 peer")
	}
}

func TestNoMarketMeansNoCap(t *testing.T) {
	s := &Server{PoolRaw: mustXNO(t, "0.001"), PoolBytes: 32 << 20}
	if c := s.nextHopCap(); c != 0 {
		t.Fatalf("cap with no registry = %d, want 0 (no cap)", c)
	}
}

func TestTopUpRefusesAnOverPricedPeerBeforeSpending(t *testing.T) {
	s := floatServer(t, "0.0005", "0.0005", "0.0005", "0.0005", "0.02")
	s.Nano = &nano.Client{} // never reached: the refusal comes first
	rel := s.Registry.relays["nano_peere"]
	s.pools = map[string]*pool{rel.Account: {topping: true}}
	_, err := s.topUpPool(rel)
	if err == nil || !strings.Contains(err.Error(), "over this relay's cap") {
		t.Fatalf("top-up to a 0.02 peer: err = %v, want a cap refusal", err)
	}
	if s.pools[rel.Account].topping {
		t.Fatal("a refused top-up left the pool marked as topping, so it would never be retried")
	}
}

func TestOneTopUpCannotTakeTheWholeWallet(t *testing.T) {
	x := func(v string) *big.Int { return mustXNO(t, v) }
	cases := []struct{ amount, base, avail, want string }{
		{"0.64", "0.016", "0.12", "0.06"},   // the 2026-10-28 drain: half, not all
		{"0.016", "0.016", "0.12", "0.016"}, // a normal pool is untouched
		{"0.03", "0.016", "0.02", "0.016"},  // poor relay: still one base pool
		{"0.03", "0.016", "0.01", "0.01"},   // poorer still: what there is, as before
	}
	for _, c := range cases {
		if got := capTopUp(x(c.amount), x(c.base), x(c.avail)); got.Cmp(x(c.want)) != 0 {
			t.Errorf("capTopUp(%s, base %s, avail %s) = %s, want %s", c.amount, c.base, c.avail, formatXNO(got), c.want)
		}
	}
}
