package client

import (
	"testing"

	"github.com/dhyabi2/sail/relay"
)

// A relay that raises its price must lose demand and, above the cap, all of it.
func TestPriceCapAndWeight(t *testing.T) {
	m := &manager{}
	all := []*relay.RelayInfo{{Account: "a", MinRate: 200}, {Account: "b", MinRate: 200}, {Account: "c", MinRate: 220}, {Account: "d", MinRate: 2000}}
	median, cap := m.priceCap(all)
	if median != 220 && median != 200 {
		t.Fatalf("median %d", median)
	}
	if cap != 3*median {
		t.Fatalf("default cap %d, want 3x median", cap)
	}
	if all[3].MinRate <= cap {
		t.Fatal("a relay at 10x the median must be over the default cap")
	}
	m.opts.rate = 250
	if _, cap := m.priceCap(all); cap != 250 {
		t.Fatalf("explicit cap %d", cap)
	}
}
