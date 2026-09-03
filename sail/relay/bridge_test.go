package relay

import (
	"net"
	"testing"
)

func TestBridgeLineRoundTrip(t *testing.T) {
	ri := &RelayInfo{Account: "nano_363r5j7hp1b8qisbqjkiwkpgbp4w3i9rzfikro5j1qeaw6gr6h3f3xbjqgu1", Desc: Descriptor{IP: net.ParseIP("2.24.73.29").To4(), Port: 443, CertFP: [6]byte{1, 2, 3, 4, 5, 6}}, Host: "tide-notes.example"}
	line := ri.BridgeLine()
	got, err := ParseBridgeLine(line + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got.Account != ri.Account || !got.Desc.IP.Equal(ri.Desc.IP) || got.Desc.Port != 443 || got.Desc.CertFP != ri.Desc.CertFP || got.Host != ri.Host || !got.Unlisted {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	for _, bad := range []string{"", "sail-bridge:x", "sail-bridge:nano_1:1.2.3.4:443:0102030405:h", line + ":extra"} {
		if _, err := ParseBridgeLine(bad); err == nil {
			t.Errorf("accepted bad line %q", bad)
		}
	}
	// A bridge survives a ledger refresh and shadows nothing.
	r := &Registry{}
	r.Add(got)
	if r.Get(got.Account) == nil || len(r.All()) != 1 {
		t.Fatal("bridge not visible")
	}
	r.mu.Lock()
	r.relays = map[string]*RelayInfo{}
	r.mu.Unlock()
	if r.Get(got.Account) == nil {
		t.Fatal("bridge lost on refresh")
	}
}
