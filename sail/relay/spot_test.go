package relay

import (
	"encoding/json"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/dhyabi2/sail/nano"
	"github.com/dhyabi2/sail/token"
)

func spotRelay(t *testing.T) (*nano.Key, *RelayInfo) {
	t.Helper()
	seed := make([]byte, 32)
	seed[0] = 99
	key, _ := nano.DeriveKey(seed, 0)
	return key, &RelayInfo{Account: key.Address, Pub: key.Public, Country: "DE", ASN: 1, MinRate: 1000, Flags: 3, Desc: Descriptor{IP: net.IPv4(10, 0, 0, 1), Port: 443}}
}

func TestSpotOfferRidesTheRecordAndIsVerifiedOnItsOwn(t *testing.T) {
	key, ri := spotRelay(t)
	now := time.Now()
	ri.SpotRate, ri.SpotUntil = 500, now.Add(20*time.Minute).Unix()
	rec := NewSignedRecord(key, ri)
	if rec.Spot == nil {
		t.Fatal("an active offer should be attached to the relay's own record")
	}
	got, err := rec.Verify(now)
	if err != nil {
		t.Fatal(err)
	}
	if got.SpotRate != 500 || got.PriceNow(now) != 500 || got.PriceNow(now.Add(25*time.Minute)) != 1000 {
		t.Fatalf("spot = %d, price now %d, price later %d", got.SpotRate, got.PriceNow(now), got.PriceNow(now.Add(25*time.Minute)))
	}

	// Tampered offer: the record still stands, the offer does not.
	rec.Spot.Rate = 100
	got, err = rec.Verify(now)
	if err != nil || got.SpotRate != 0 || got.PriceNow(now) != 1000 {
		t.Fatalf("a tampered offer must be dropped and the record kept: err=%v spot=%d", err, got.SpotRate)
	}

	// An offer that is not lower than the published price is no offer.
	ri.SpotRate = 1000
	if rec := NewSignedRecord(key, ri); rec.Spot != nil {
		t.Fatal("an offer at the published price must not be attached")
	}
	// Expired offer: dropped.
	ri.SpotRate, ri.SpotUntil = 500, now.Add(-time.Minute).Unix()
	if rec := NewSignedRecord(key, ri); rec.Spot != nil {
		t.Fatal("an expired offer must not be attached")
	}
}

func TestARecordWithoutTheSpotFieldVerifiesAsBefore(t *testing.T) {
	key, ri := spotRelay(t)
	ri.SpotRate, ri.SpotUntil = 500, time.Now().Add(20*time.Minute).Unix()
	rec := NewSignedRecord(key, ri)
	// What a relay from before spot offers stores and forwards: the same
	// JSON with the field it does not know dropped.
	b, _ := json.Marshal(rec)
	var old struct {
		Account string `json:"a"`
		Country string `json:"cc"`
		ASN     uint32 `json:"asn"`
		MinRate uint32 `json:"rate"`
		Flags   uint16 `json:"flags"`
		Desc    string `json:"desc"`
		Host    string `json:"host,omitempty"`
		Time    int64  `json:"t"`
		Sig     string `json:"sig"`
	}
	if err := json.Unmarshal(b, &old); err != nil {
		t.Fatal(err)
	}
	fwd, _ := json.Marshal(old)
	var again SignedRecord
	if err := json.Unmarshal(fwd, &again); err != nil {
		t.Fatal(err)
	}
	got, err := again.Verify(time.Now())
	if err != nil {
		t.Fatalf("the main signature must not cover the offer: %v", err)
	}
	if got.SpotRate != 0 || got.MinRate != 1000 {
		t.Fatal("forwarded by an old relay, the record is intact and simply carries no offer")
	}
}

func TestSpotDecisionAndCreditRate(t *testing.T) {
	if r, ok := spotDecision(10, 20, 50, 1000); !ok || r != 500 {
		t.Fatalf("10%% load, 50%% off → 500, got %d %v", r, ok)
	}
	if _, ok := spotDecision(25, 20, 50, 1000); ok {
		t.Fatal("above the threshold: no offer")
	}
	if _, ok := spotDecision(10, 20, 0, 1000); ok {
		t.Fatal("no discount: no offer")
	}
	if _, ok := spotDecision(10, 20, 100, 1000); ok {
		t.Fatal("free is not a price")
	}
	key, ri := spotRelay(t)
	q, _ := NewQuota("", token.RateToRaw(1000))
	s := &Server{Key: key, Self: ri, Quota: q, SpotDiscount: 50, SpotBelow: 20, Capacity: 1000}
	if s.rateNow().Cmp(token.RateToRaw(1000)) != 0 {
		t.Fatal("no window: the published price")
	}
	// Two idle samples → an offer; a later busy sample does not cut the window short.
	now := time.Now()
	s.sampleSpot(now, time.Minute)
	s.Metrics.BytesRelayed.Store(600) // 10 bytes/s of 1000 → 1%
	s.sampleSpot(now.Add(time.Minute), time.Minute)
	rate, until := s.currentSpot(now.Add(time.Minute))
	if rate != 500 || until == 0 {
		t.Fatalf("idle relay should offer 500, got %d until %d", rate, until)
	}
	if s.rateNow().Cmp(token.RateToRaw(500)) != 0 {
		t.Fatal("while the window stands, payments are credited at the spot price")
	}
	s.Metrics.BytesRelayed.Store(600 + 60*1000) // 100% for the last minute
	s.sampleSpot(now.Add(2*time.Minute), time.Minute)
	if r, _ := s.currentSpot(now.Add(2 * time.Minute)); r != 500 {
		t.Fatal("a signed window is honoured to its end")
	}
	// The self record carries it.
	if rec := NewSignedRecord(key, s.selfWithSpot()); rec.Spot == nil || rec.Spot.Rate != 500 {
		t.Fatal("the relay's own record should carry the standing offer")
	}
	_ = big.NewInt(0)
}
