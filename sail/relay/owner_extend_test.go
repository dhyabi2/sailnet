package relay

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/dhyabi2/sail/nano"
)

// The owner enters through someone else's relay and rides its own relay as
// the exit: the owner tag travels inside the EXTEND, the middle relay
// forwards it instead of its pool tag, and nothing of the middle's is spent.

func TestParseExtendAcceptsATagAndRefusesEverythingElse(t *testing.T) {
	pub := [32]byte{1}
	base := append([]byte("nano_next"), 0)
	base = append(base, pub[:]...)
	if acct, p, tag, ok := parseExtend(base); !ok || acct != "nano_next" || p != pub || tag != nil {
		t.Fatal("the plain form must parse as before, with no client tag")
	}
	withTag := append(append([]byte(nil), base...), make([]byte, 96)...)
	if _, _, tag, ok := parseExtend(withTag); !ok || len(tag) != 96 {
		t.Fatal("pub ‖ tag ‖ sig must parse with a 96-byte client tag")
	}
	for _, extra := range []int{1, 31, 33, 95, 97, 200} {
		if _, _, _, ok := parseExtend(append(append([]byte(nil), base...), make([]byte, extra)...)); ok {
			t.Fatalf("%d trailing bytes must still be a bad EXTEND", extra)
		}
	}
	if _, _, _, ok := parseExtend([]byte("no-nul-anywhere")); ok {
		t.Fatal("a payload without the separator must be a bad EXTEND")
	}
}

func TestOwnerRidesItsRelayAsExitThroughAStrangersEntry(t *testing.T) {
	reg := &Registry{}
	var servers []*Server
	var infos []*RelayInfo
	for i := 0; i < 3; i++ {
		s, ri, _ := startRelay(t, reg, i, i == 2)
		servers = append(servers, s)
		infos = append(infos, ri)
	}
	seed := make([]byte, 32)
	seed[0] = 77
	owner, _ := nano.DeriveKey(seed, 0)
	servers[2].Owner = owner.Public // the exit is the owner's relay

	// The owner pays the entry like anyone, and the entry prepays the middle.
	// Nobody prepays the exit: the owner tag covers that hop.
	var anchor [32]byte
	copy(anchor[:], "owner-pays-the-entry-like-anyone")
	servers[0].Quota.Credit(hex.EncodeToString(anchor[:]), 50<<20, "")
	servers[1].Quota.Credit(PoolTag(infos[0].Account, infos[1].Account), 50<<20, "")
	middleToExit := PoolTag(infos[1].Account, infos[2].Account)

	ownerTag := OwnerTag(infos[2].Pub, owner.Public)
	c, err := BuildTags(infos, anchor, map[int][32]byte{2: ownerTag}, 10*time.Second, nil,
		func(pub, tg [32]byte) []byte { return SignCreate(owner, pub, tg) })
	if err != nil {
		t.Fatalf("owner's circuit failed at hop %d: %v", c.Failed, err)
	}
	defer c.Close()
	if bad := c.Ping(5 * time.Second); bad != -1 {
		t.Fatalf("ping failed at hop %d", bad)
	}
	if !servers[2].Quota.Known(hex.EncodeToString(ownerTag[:])) {
		t.Fatal("the exit should hold the owner tag")
	}
	if servers[2].Quota.Known(middleToExit) {
		t.Fatal("no pool from the middle to the exit should have been needed")
	}

	// A stranger who learned the exit's owner tag is refused at the exit —
	// and the middle reports it as the client's tag, not as its pool.
	seed[0] = 78
	stranger, _ := nano.DeriveKey(seed, 0)
	c2, err := BuildTags(infos, anchor, map[int][32]byte{2: ownerTag}, 10*time.Second, nil,
		func(pub, tg [32]byte) []byte { return SignCreate(stranger, pub, tg) })
	if err == nil {
		c2.Close()
		t.Fatal("a stranger signing the owner tag must not reach the exit")
	}
	if c2 == nil || c2.Failed != 2 || !strings.Contains(err.Error(), "tag you supplied") {
		t.Fatalf("expected the middle to report a refused client tag at hop 2, got hop %d: %v", c2.Failed, err)
	}
}
