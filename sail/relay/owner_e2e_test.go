package relay

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/dhyabi2/sail/nano"
)

// Over the wire: the owner's wallet builds a three-hop circuit whose entry
// is the owner's own relay, with no payment credited there beforehand. A
// stranger presenting the same tag is refused at hop 0.

func TestOwnerBuildsACircuitOnItsOwnRelayWithoutPaying(t *testing.T) {
	reg := &Registry{}
	var servers []*Server
	var infos []*RelayInfo
	for i := 0; i < 3; i++ {
		s, ri, _ := startRelay(t, reg, i, i == 2)
		servers = append(servers, s)
		infos = append(infos, ri)
	}
	seed := make([]byte, 32)
	seed[0] = 42
	owner, _ := nano.DeriveKey(seed, 0)
	servers[0].Owner = owner.Public // --owner / --payout on the entry
	// The entry prepays the next hops from its float, as for any circuit.
	servers[1].Quota.Credit(PoolTag(infos[0].Account, infos[1].Account), 50<<20, "")
	servers[2].Quota.Credit(PoolTag(infos[1].Account, infos[2].Account), 50<<20, "")

	tag := OwnerTag(infos[0].Pub, owner.Public)
	if servers[0].Quota.Known(hex.EncodeToString(tag[:])) {
		t.Fatal("nothing should be credited before the owner connects")
	}
	c, err := Build(infos, tag, 10*time.Second, nil, func(pub, tg [32]byte) []byte { return SignCreate(owner, pub, tg) })
	if err != nil {
		t.Fatalf("owner's circuit failed at hop %d: %v", c.Failed, err)
	}
	defer c.Close()
	if bad := c.Ping(5 * time.Second); bad != -1 {
		t.Fatalf("ping failed at hop %d", bad)
	}
	if rem := servers[0].Quota.Remaining(hex.EncodeToString(tag[:])); rem < OwnerBytes/2 {
		t.Fatalf("owner tag should hold a large grant, remaining %d", rem)
	}

	// A stranger who knows the tag but not the key is refused at the entry.
	seed[0] = 43
	stranger, _ := nano.DeriveKey(seed, 0)
	c2, err := Build(infos, tag, 10*time.Second, nil, func(pub, tg [32]byte) []byte { return SignCreate(stranger, pub, tg) })
	if err == nil {
		c2.Close()
		t.Fatal("a stranger signing the owner tag must not get a circuit")
	}
	if c2 == nil || c2.Failed != 0 || !strings.Contains(err.Error(), "not signed by the payer") {
		t.Fatalf("expected refusal at hop 0 for a bad signature, got hop %v: %v", c2 != nil && c2.Failed == 0, err)
	}
}
