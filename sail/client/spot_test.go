package client

import (
	"math/big"
	"testing"
	"time"

	"github.com/dhyabi2/sail/relay"
	"github.com/dhyabi2/sail/token"
)

// A standing spot offer is the price the client pays; an expired one is not.
func TestAnchorIsSizedAtTheSpotPriceWhileItStands(t *testing.T) {
	t.Setenv("SAIL_HOME", t.TempDir())
	m := &manager{key: EnsureWallet()}
	m.opts.anchor = big.NewInt(1)
	r := &relay.RelayInfo{Account: "nano_x", MinRate: 1000, SpotRate: 250, SpotUntil: time.Now().Add(20 * time.Minute).Unix()}
	if got := m.anchorFor(r); got.Cmp(relay.RawFor(AnchorBytes, token.RateToRaw(250))) != 0 {
		t.Fatalf("anchor at spot price expected, got %s", token.FormatXNO(got))
	}
	r.SpotUntil = time.Now().Add(time.Minute).Unix() // inside the two-minute slack: treated as gone
	if got := m.anchorFor(r); got.Cmp(relay.RawFor(AnchorBytes, token.RateToRaw(1000))) != 0 {
		t.Fatalf("an offer about to expire is not relied on, got %s", token.FormatXNO(got))
	}
}
