package relay

import (
	"math/big"
	"testing"

	"github.com/dhyabi2/sail/token"
)

// Every prepaid amount in the network is a quantity of service. These tests
// pin that down: the same megabytes cost a hundred times more XNO when the
// price is a hundred times higher, and nothing that depends on the amount
// has to be re-tuned when it moves.
func TestRawForBuysWhatItIsSizedFor(t *testing.T) {
	for _, rate := range []uint32{500, 500000, 50000000} {
		raw := token.RateToRaw(rate)
		amount := RawFor(32<<20, raw)
		if got := BytesFor(amount, raw); got < 32<<20 {
			t.Fatalf("rate %d: %d bytes bought, want at least %d", rate, got, 32<<20)
		}
	}
	if RawFor(0, token.RateToRaw(500)).Sign() != 0 {
		t.Fatal("no bytes, no money")
	}
	if RawFor(1<<20, nil).Sign() != 0 {
		t.Fatal("no price, no money")
	}
	// A free relay is free: BytesFor and RawFor agree that there is nothing
	// to pay rather than dividing by zero.
	if RawFor(1<<20, big.NewInt(0)).Sign() != 0 {
		t.Fatal("zero rate must cost nothing")
	}
}

// A pool sized in bytes follows the peer's price, so a peer that charges a
// hundred times more is still prepaid — and still usable. Sized in XNO it
// fell under the 8 MiB floor in ensurePool and was silently never paid,
// which is how a price rise used to partition the network.
func TestPoolFollowsPeerPrice(t *testing.T) {
	s := &Server{PoolRaw: mustXNO(t, "0.001"), PoolBytes: 32 << 20}
	cheap := &RelayInfo{MinRate: 500}     // 0.00000005 XNO/MiB
	dear := &RelayInfo{MinRate: 50000000} // 0.005      XNO/MiB
	if got := s.poolSize(cheap); got < 8<<20 {
		t.Fatalf("cheap peer pool buys %d bytes", got)
	}
	if got := s.poolSize(dear); got < 8<<20 {
		t.Fatalf("dear peer pool buys %d bytes, would never be topped up", got)
	}
	// The flag is a floor, never a ceiling: a cheap peer is not prepaid less
	// than the operator asked for.
	if s.poolRaw(cheap).Cmp(s.PoolRaw) < 0 {
		t.Fatal("pool went under the operator's floor")
	}
	// The regression this replaced: a flat pool at a dearer peer's price
	// bought under the 8 MiB that ensurePool insists on, so the peer was
	// never prepaid and never usable. Sized in bytes, the same operator
	// setting works at that price.
	flatAtDearPrice := BytesFor(mustXNO(t, "0.001"), token.RateToRaw(dear.MinRate))
	if flatAtDearPrice >= 8<<20 {
		t.Fatalf("expected the old flat pool to fall under the floor at 0.005 XNO/MiB, it bought %d bytes", flatAtDearPrice)
	}
	if s.poolSize(dear) < 8<<20 {
		t.Fatal("sizing in bytes must lift the same setting back over the floor")
	}

	// With PoolBytes off, the old flat behaviour is exactly preserved.
	flat := &Server{PoolRaw: mustXNO(t, "0.001")}
	if flat.poolRaw(dear).Cmp(flat.PoolRaw) != 0 {
		t.Fatal("PoolBytes unset must keep the flat amount")
	}
	if (&Server{}).poolRaw(dear) != nil {
		t.Fatal("no PoolRaw means static pool tags, not a payment")
	}
}

// An app built against an older, cheaper price pays an anchor it cannot be
// told to change. It gets a usable circuit once a day rather than a
// kilobyte, and the grant does not repeat within the day.
func TestFloorCreditIsOncePerWalletPerDay(t *testing.T) {
	s := &Server{MinCredit: 10 << 20}
	const tiny = 100 << 10
	if got := s.floorCredit("wallet-a", tiny); got != 10<<20 {
		t.Fatalf("first payment credited %d, want the floor", got)
	}
	if got := s.floorCredit("wallet-a", tiny); got != tiny {
		t.Fatalf("second payment credited %d, want the real price", got)
	}
	if got := s.floorCredit("wallet-b", tiny); got != 10<<20 {
		t.Fatalf("another wallet credited %d, want the floor", got)
	}
	// A payment that already buys enough is never touched, and an unknown
	// payer is never granted anything.
	if got := s.floorCredit("wallet-c", 40<<20); got != 40<<20 {
		t.Fatalf("a full payment was rewritten to %d", got)
	}
	if got := s.floorCredit("", tiny); got != tiny {
		t.Fatal("an anonymous payment must not earn the floor")
	}
	if got := (&Server{}).floorCredit("wallet-d", tiny); got != tiny {
		t.Fatal("MinCredit unset must grant nothing")
	}
}

func mustXNO(t *testing.T, s string) *big.Int {
	t.Helper()
	v, err := token.ParseXNO(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
