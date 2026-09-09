package client

import (
	"fmt"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/dhyabi2/sail/relay"
	"github.com/dhyabi2/sail/token"
)

// Run a relay, ride it free — without being seen at it. A relay this wallet
// runs is the exit (or a middle), never the entry: the owner tag rides
// inside the EXTEND, so the entry sees an ordinary paying client and no
// observer of our relay finds our address in its inbound.

func mineManager(t *testing.T, exitFlags map[string]uint16) *manager {
	t.Helper()
	t.Setenv("SAIL_HOME", t.TempDir())
	m := &manager{reg: &relay.Registry{}, rtt: map[string]time.Duration{}, key: EnsureWallet()}
	m.opts.hops = 3
	m.opts.anchor = big.NewInt(1)
	for i := 0; i < 6; i++ {
		a := fmt.Sprintf("nano_real%d", i)
		flags := uint16(token.FlagPublic | token.FlagExit)
		if f, ok := exitFlags[a]; ok {
			flags = f
		}
		m.reg.Add(&relay.RelayInfo{Account: a, MinRate: 50000, Flags: flags,
			Country: fmt.Sprintf("C%d", i), ASN: uint32(i + 1), Desc: relay.Descriptor{IP: net.IPv4(10, 0, 0, byte(i)), Port: 443}})
		m.rtt[a] = time.Duration(10*(i+1)) * time.Millisecond // real0 fastest: it would be entry on merit
	}
	return m
}

func TestMyRelayIsTheExitAndNeverTheEntry(t *testing.T) {
	m := mineManager(t, nil)
	m.SetMine("not-an-account, nano_real0\nnano_real9")
	if !m.opts.mine["nano_real0"] || m.opts.mine["not-an-account"] || len(m.opts.mine) != 2 {
		t.Fatalf("SetMine parsed %v", m.opts.mine)
	}
	for i := 0; i < 8; i++ {
		path, err := m.choosePath()
		if err != nil {
			t.Fatal(err)
		}
		if path[0].Account == "nano_real0" {
			t.Fatal("our own relay must never be the entry")
		}
		if path[len(path)-1].Account != "nano_real0" {
			t.Fatalf("our own relay should be the exit, path ends at %s", path[len(path)-1].Account)
		}
		tags := m.hopTags(path)
		if _, ok := tags[len(path)-1]; !ok || len(tags) != 1 {
			t.Fatalf("exactly the exit should carry the owner tag, got %v", tags)
		}
	}
}

func TestMyRelayWithoutExitIsAMiddle(t *testing.T) {
	m := mineManager(t, map[string]uint16{"nano_real0": uint16(token.FlagPublic)})
	m.SetMine("nano_real0")
	path, err := m.choosePath()
	if err != nil {
		t.Fatal(err)
	}
	if path[0].Account == "nano_real0" || path[len(path)-1].Account == "nano_real0" || path[1].Account != "nano_real0" {
		t.Fatalf("a relay of ours that does not exit should be the middle, got %s → %s → %s", path[0].Account, path[1].Account, path[2].Account)
	}
}

func TestAnOldRelayBeforeOursGetsNoTagForAnHour(t *testing.T) {
	m := mineManager(t, nil)
	m.SetMine("nano_real0")
	path, _ := m.choosePath()
	prev := path[len(path)-2].Account
	m.extendOld = map[string]time.Time{prev: time.Now()}
	if tags := m.hopTags(path); len(tags) != 0 {
		t.Fatalf("no owner tag should be sent through a relay that answered bad EXTEND, got %v", tags)
	}
	m.extendOld[prev] = time.Now().Add(-2 * time.Hour)
	if tags := m.hopTags(path); len(tags) != 1 {
		t.Fatal("after an hour the tag should be tried through it again")
	}
}

func TestARefusingRelayOfMineIsRestedForAnHour(t *testing.T) {
	m := mineManager(t, nil)
	m.SetMine("nano_real0")
	m.mineRefused = map[string]time.Time{"nano_real0": time.Now().Add(-10 * time.Minute)}
	path, err := m.choosePath()
	if err != nil {
		t.Fatal(err)
	}
	// It may still be chosen as an ordinary hop — it is a relay like any
	// other — but it gets no owner tag, and it is not preferred as our exit.
	if tags := m.hopTags(path); len(tags) != 0 {
		t.Fatalf("a relay that refused us ten minutes ago must get no owner tag, got %v", tags)
	}
	m.mineRefused["nano_real0"] = time.Now().Add(-2 * time.Hour)
	if path, _ = m.choosePath(); path[len(path)-1].Account != "nano_real0" || len(m.hopTags(path)) != 1 {
		t.Fatalf("after an hour our relay should be the exit again with its tag, got %s", path[len(path)-1].Account)
	}
}

func TestNothingChangesForAClientWithNoRelays(t *testing.T) {
	m := mineManager(t, nil)
	path, err := m.choosePath()
	if err != nil {
		t.Fatal(err)
	}
	if tags := m.hopTags(path); tags != nil {
		t.Fatalf("a client that lists no relays must send no tags, got %v", tags)
	}
}

// After an old relay refuses the tagged EXTEND, the next attempt must take
// the same path with the tag dropped: same entry, same anchor, no new payment.
func TestAfterBadExtendTheSamePathIsRetriedWithoutTheTag(t *testing.T) {
	m := mineManager(t, nil)
	m.SetMine("nano_real0")
	path, err := m.choosePath()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.hopTags(path)) != 1 {
		t.Fatal("precondition: our exit carries a tag")
	}
	// What the build loop does on "hop N-1: bad EXTEND" for a tagged hop N.
	failed := len(path) - 1
	m.skip = map[string]bool{path[failed].Account: true}
	m.extendOld = map[string]time.Time{path[failed-1].Account: time.Now()}
	delete(m.skip, path[failed].Account)
	m.retryPath = path

	again := m.retryPath
	m.retryPath = nil
	if again == nil || again[0].Account != path[0].Account || again[failed].Account != path[failed].Account {
		t.Fatal("the retry must reuse the same path and the same entry")
	}
	if tags := m.hopTags(again); len(tags) != 0 {
		t.Fatalf("the retry must carry no tag through the old relay, got %v", tags)
	}
	if m.skip[path[failed].Account] {
		t.Fatal("our relay did nothing wrong and must not be skipped")
	}
}
