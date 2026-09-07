package client

import (
	"math/big"
	"testing"

	"github.com/dhyabi2/sail/relay"
	"github.com/dhyabi2/sail/token"
)

func rate(t *testing.T, s string) uint32 {
	t.Helper()
	r, err := token.RateFromXNO(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The anchor is a purchase of service, so it buys the same megabytes at any
// price. This is the whole point: the old fixed 0.0005 XNO bought 10 MiB at
// one price and 100 KiB at a hundred times that, which is not enough to open
// a page, and the client spent its session re-anchoring instead of browsing.
func TestAnchorBuysTheSameServiceAtAnyPrice(t *testing.T) {
	floor, err := token.ParseXNO(AnchorXNO)
	if err != nil {
		t.Fatal(err)
	}
	m := &manager{}
	m.opts.anchor = floor
	for _, price := range []string{"0.0000005", "0.00005", "0.005", "0.05"} {
		r := rate(t, price)
		amount := m.anchorFor(&relay.RelayInfo{MinRate: r})
		if got := relay.BytesFor(amount, token.RateToRaw(r)); got < AnchorBytes {
			t.Fatalf("at %s XNO/MiB an anchor of %s buys %d bytes, want %d",
				price, token.FormatXNO(amount), got, AnchorBytes)
		}
		if amount.Cmp(floor) < 0 {
			t.Fatalf("at %s XNO/MiB the anchor fell under the --anchor floor", price)
		}
	}
	// A relay that publishes no price is free: pay the floor, not nothing,
	// and never divide by a zero rate.
	if m.anchorFor(&relay.RelayInfo{}).Cmp(floor) != 0 {
		t.Fatal("a free relay must still get the floor")
	}
	if m.anchorFor(nil).Cmp(floor) != 0 {
		t.Fatal("no relay, the floor")
	}
}

// What a wallet must hold to connect is an anchor at the cheapest relay it
// could actually use — not at the dearest, which would tell a user to fund
// far more than connecting needs.
func TestAnchorNeedIsTheCheapestUsableRelay(t *testing.T) {
	floor, _ := token.ParseXNO(AnchorXNO)
	m := &manager{}
	m.opts.anchor = floor
	m.reg = &relay.Registry{}
	m.reg.Add(&relay.RelayInfo{Account: "dear", MinRate: rate(t, "0.05")})
	m.reg.Add(&relay.RelayInfo{Account: "cheap", MinRate: rate(t, "0.005")})
	need := m.anchorNeed()
	if want := m.anchorFor(&relay.RelayInfo{MinRate: rate(t, "0.005")}); need.Cmp(want) != 0 {
		t.Fatalf("need %s, want the cheapest relay's anchor %s", token.FormatXNO(need), token.FormatXNO(want))
	}
	// An empty registry cannot mean "you need nothing"; it means the floor.
	empty := &manager{reg: &relay.Registry{}}
	empty.opts.anchor = floor
	if empty.anchorNeed().Cmp(floor) != 0 {
		t.Fatal("with no relay list the requirement is the floor")
	}
}

// A hundredfold price rise must not multiply what a top-up costs per byte:
// the top-up is sized from the circuit's measured rate, and only its floor
// moves with the price.
func TestTopUpFloorTracksThePrice(t *testing.T) {
	floor, _ := token.ParseXNO(AnchorXNO)
	m := &manager{}
	m.opts.anchor = floor
	cheap := m.anchorFor(&relay.RelayInfo{MinRate: rate(t, "0.00005")})
	dear := m.anchorFor(&relay.RelayInfo{MinRate: rate(t, "0.005")})
	if dear.Cmp(cheap) <= 0 {
		t.Fatal("a dearer relay must be prepaid more, not the same")
	}
	ratio := new(big.Int).Div(dear, cheap)
	if ratio.Int64() != 100 {
		t.Fatalf("a 100x price should cost 100x per anchor, got %s", ratio)
	}
}
