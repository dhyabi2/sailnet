package relay

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dhyabi2/sail/nano"
)

// The price on the public page is the price of the live network, not of the
// ledger's memory of it.
//
// Registrations are permanent and cost one raw, so every price a relay ever
// published stays on the ledger for ever. Thirty retired records at a tenth
// of today's price outnumbered eight live relays, and the website reported
// 0.000005 XNO/MiB — a price no relay on the network charges — while clients,
// which have always medianed over live relays, used the real 0.00005.
func TestStatsPriceIgnoresDeadRegistrations(t *testing.T) {
	// add registers a relay; live ones also get a gossip record, which is
	// what LastSeen (and so "alive") is actually made of.
	add := func(reg *Registry, i int, rate uint32, live bool) {
		seed := make([]byte, 32)
		seed[0], seed[1] = byte(i), byte(rate)
		key, err := nano.DeriveKey(seed, 0)
		if err != nil {
			t.Fatal(err)
		}
		ri := &RelayInfo{Account: key.Address, Pub: key.Public, MinRate: rate, Flags: 3,
			Country: "XX", Desc: Descriptor{IP: net.IPv4(203, 0, 113, byte(i%250)), Port: 443}}
		reg.Add(ri)
		if live && !reg.AddGossip(NewSignedRecord(key, ri)) {
			t.Fatalf("relay %d: gossip record rejected, the test cannot mark it live", i)
		}
	}

	reg := &Registry{}
	for i := 0; i < 30; i++ { // retired registrations at the old, ten-times-cheaper price
		add(reg, i, 50000, false)
	}
	for i := 0; i < 8; i++ { // the network as it is now
		add(reg, 100+i, 500000, true)
	}

	key, _ := nano.DeriveKey(make([]byte, 32), 0)
	stats := func(r *Registry) map[string]any {
		s := &Server{Key: key, Registry: r, Metrics: Metrics{Started: time.Now()}}
		rec := httptest.NewRecorder()
		s.serveStats(rec, httptest.NewRequest("GET", "/stats", nil))
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("stats is not JSON: %v (%s)", err, rec.Body.String())
		}
		return out
	}

	out := stats(reg)
	if got := out["alive"]; got != float64(8) {
		t.Fatalf("alive %v, want the 8 relays with gossip records", got)
	}
	if got := out["priceXnoPerMiB"]; got != "0.00005000" {
		t.Fatalf("price %v, want the live network's 0.00005 XNO/MiB, not the dead records' 0.000005", got)
	}

	// With fewer live relays than a circuit needs there is no market to
	// report, so it falls back to every record rather than quoting one node.
	thin := &Registry{}
	for i := 0; i < 30; i++ {
		add(thin, i, 50000, false)
	}
	add(thin, 200, 500000, true)
	if got := stats(thin)["priceXnoPerMiB"]; got != "0.00000500" {
		t.Fatalf("price %v, want the all-records fallback 0.000005 when one relay cannot make a market", got)
	}
}

var _ = fmt.Sprintf
