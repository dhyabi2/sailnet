package relay

import (
	"net"
	"testing"
	"time"

	"github.com/dhyabi2/sail/nano"
)

func TestSignedRecordRoundTrip(t *testing.T) {
	seed, _ := nano.NewSeed()
	k, _ := nano.DeriveKey(seed, 0)
	ri := &RelayInfo{Account: k.Address, Pub: k.Public, Country: "DE", ASN: 47583, MinRate: 200, Flags: 3, Desc: Descriptor{IP: net.ParseIP("148.230.105.31").To4(), Port: 443, CertFP: [6]byte{9, 8, 7, 6, 5, 4}}, Host: "tide.example"}
	rec := NewSignedRecord(k, ri)
	got, err := rec.Verify(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.Account != ri.Account || got.Country != "DE" || got.ASN != 47583 || got.Desc.Port != 443 || got.Desc.CertFP != ri.Desc.CertFP || got.Host != "tide.example" {
		t.Fatalf("mismatch: %+v", got)
	}
	rec.Country = "US" // tamper
	if _, err := rec.Verify(time.Now()); err == nil {
		t.Fatal("tampered record verified")
	}
	rec.Country = "DE"
	if _, err := rec.Verify(time.Now().Add(RecordTTL + time.Hour)); err == nil {
		t.Fatal("expired record verified")
	}
	r := &Registry{}
	if !r.AddGossip(rec) || r.Get(k.Address) == nil || len(r.All()) != 1 {
		t.Fatal("gossip record not visible")
	}
	if r.AddGossip(rec) {
		t.Fatal("duplicate accepted")
	}
	if n := len(r.Records(nil)); n != 1 {
		t.Fatalf("records: %d", n)
	}
}
