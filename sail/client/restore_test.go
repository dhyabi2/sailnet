package client

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dhyabi2/sail/nano"
)

const (
	someFrontier = "05D1E923B45C08A212E4B022B3BDF6262A2E767023742C68DB0881F3885A617B"
	someRep      = "nano_1banexkcfuieufzxksfrxqf6xy8e57ry1zdtq9yn7jntzhpwu4pg4hajojmq"
)

func newState(t *testing.T) *nano.ChainState {
	t.Helper()
	return &nano.ChainState{Path: filepath.Join(t.TempDir(), "chain.json")}
}

func frontier32(t *testing.T, s string) [32]byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		t.Fatalf("bad frontier %q", s)
	}
	var out [32]byte
	copy(out[:], b)
	return out
}

func rawBalance(t *testing.T, s string) *big.Int {
	t.Helper()
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad balance %q", s)
	}
	return v
}

// Restoring a wallet onto a new device is the case this exists for: the
// money arrived long ago, so there is no pending block to pocket, and
// without reading the balance from the ledger the client would sit there
// insisting the wallet is empty.
func TestRestoredWalletLearnsItHasMoney(t *testing.T) {
	st := newState(t)
	if _, _, _, _, cached := st.Get(); cached {
		t.Fatal("a fresh device should have no chain state")
	}
	info := nano.AccountInfo{
		Frontier:       someFrontier,
		Balance:        "100249499999999999999999999994", // 0.1002 XNO
		Representative: someRep,
	}
	if !cacheAccountInfo(st, info) {
		t.Fatal("the ledger's answer was not cached")
	}
	_, bal, _, opened, cached := st.Get()
	if !cached || !opened {
		t.Fatal("the wallet still looks unknown after reading the ledger")
	}
	want, _ := new(big.Int).SetString(info.Balance, 10)
	if bal.Cmp(want) != 0 {
		t.Fatalf("balance %s, want %s", bal, want)
	}
	// The anchor a client must afford is 0.0005 XNO; this wallet can.
	anchor, _ := new(big.Int).SetString("500000000000000000000000000", 10)
	if bal.Cmp(anchor) < 0 {
		t.Fatal("a funded wallet was still judged unable to pay")
	}
}

// A wallet used on this device before — and moved elsewhere since — leaves a
// chain state the account has grown far past. The ledger read that says so
// must replace it, or the client keeps a months-old balance, insists the
// wallet is empty, and asks for money it already has. Heights decide: a
// chain longer than ours is the ledger's to describe.
func TestStaleChainStateIsRefreshedFromTheLedger(t *testing.T) {
	st := newState(t)
	old := frontier32(t, someFrontier)
	var newFront [32]byte
	newFront[0], newFront[31] = 0xCC, 0xDD
	rep, err := nano.AddressToPubkey(someRep)
	if err != nil {
		t.Fatal(err)
	}
	st.Set(old, rawBalance(t, "749499999999999999999999999"), rep, true)
	st.SetHeight(54)
	if !cacheAccountInfo(st, nano.AccountInfo{Frontier: fmt.Sprintf("%X", newFront), Balance: "74749499999999999999999999994", Representative: someRep, BlockCount: "62"}) {
		t.Fatal("a chain the ledger grew past was not refreshed")
	}
	_, bal, _, _, cached := st.Get()
	if !cached {
		t.Fatal("the chain state was lost")
	}
	want, _ := new(big.Int).SetString("74749499999999999999999999994", 10)
	if bal.Cmp(want) != 0 {
		t.Fatalf("balance %s, want the ledger's %s", bal, want)
	}
	if st.Height() != 62 {
		t.Fatalf("height %d, want 62", st.Height())
	}
}

// A local chain state can be ahead of the ledger, when a block was just sent
// and is not yet confirmed. Overwriting it with the ledger's older view
// would build the next block on a stale frontier, so it is kept: the block
// count the ledger reports has not caught up to what was published.
func TestALocalChainStateIsNeverOverwritten(t *testing.T) {
	st := newState(t)
	var mine [32]byte
	mine[0], mine[31] = 0xAA, 0xBB
	rep, err := nano.AddressToPubkey(someRep)
	if err != nil {
		t.Fatal(err)
	}
	local := big.NewInt(42)
	st.Set(mine, local, rep, true)

	if cacheAccountInfo(st, nano.AccountInfo{Frontier: someFrontier, Balance: "999", Representative: someRep}) {
		t.Fatal("the ledger overwrote a chain state this wallet was already keeping")
	}
	front, bal, _, _, _ := st.Get()
	if front != mine || bal.Cmp(local) != 0 {
		t.Fatalf("local state changed: frontier %x balance %s", front, bal)
	}
}

// The same, with a height known on both sides: the ledger reports the
// frontier the just-published block replaced, one block shorter than the
// chain this wallet knows it has.
func TestABlockStillTravellingToTheLedgerIsKept(t *testing.T) {
	st := newState(t)
	rep, err := nano.AddressToPubkey(someRep)
	if err != nil {
		t.Fatal(err)
	}
	st.Set(frontier32(t, someFrontier), big.NewInt(10), rep, true)
	st.SetHeight(62)
	var mine [32]byte
	mine[0], mine[31] = 0xAA, 0xBB
	st.Set(mine, big.NewInt(9), rep, true) // the send: frontier moves, height 62 → 63
	if st.Height() != 63 {
		t.Fatalf("a published block should grow the known height, got %d", st.Height())
	}
	if cacheAccountInfo(st, nano.AccountInfo{Frontier: someFrontier, Balance: "10", Representative: someRep, BlockCount: "62"}) {
		t.Fatal("the ledger's older view overwrote a block still travelling to it")
	}
	front, _, _, _, _ := st.Get()
	if front != mine {
		t.Fatal("the local frontier was lost")
	}
}

// A cache written by an older build keeps no height and no time, exactly
// as the released build writes it. That build rewrote the file on every
// block, so its modification time says how old it is. Fresh, it may be a
// block still travelling, so it is trusted; grown old, it is exactly the
// stale state the ledger read exists to correct.
func TestALegacyCacheIsKeptFreshAndCorrectedWhenStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.json")
	old := frontier32(t, someFrontier)
	rep, _ := nano.AddressToPubkey(someRep)
	legacy := fmt.Sprintf(`{
  "frontier": "%X",
  "balance": "749499999999999999999999999",
  "representative": "%X",
  "opened": true
}`, old, rep)
	var newFront [32]byte
	newFront[0], newFront[31] = 0xEE, 0xFF
	ledger := nano.AccountInfo{Frontier: fmt.Sprintf("%X", newFront), Balance: "74749499999999999999999999994", Representative: someRep, BlockCount: "62"}

	// Written moments ago by the older build (no height, no time): kept.
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	st := nano.LoadChainState(path)
	if cacheAccountInfo(st, ledger) {
		t.Fatal("a state just written was overwritten before its block could land")
	}
	front, _, gotRep, _, cached := st.Get()
	if !cached || front != old || gotRep != rep {
		t.Fatalf("the fresh legacy state changed: frontier %X rep %X", front, gotRep)
	}

	// Written by the older build eleven minutes ago: corrected.
	stale := time.Now().Add(-11 * time.Minute)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}
	st = nano.LoadChainState(path)
	if !cacheAccountInfo(st, ledger) {
		t.Fatal("a stale cache with no height was never corrected")
	}
	front, bal, gotRep, _, cached := st.Get()
	if !cached || front != newFront || bal.Cmp(rawBalance(t, "74749499999999999999999999994")) != 0 {
		t.Fatalf("frontier %X balance %s, want the ledger's", front, bal)
	}
	if gotRep != rep || st.Height() != 62 {
		t.Fatalf("rep %X height %d, want the ledger's", gotRep, st.Height())
	}
}

// A recorded time in the future is a clock that cannot be trusted, not a
// state written moments ago: beyond a minute of skew it is treated as stale.
func TestACacheStampedInTheFutureIsNotTrustedAsFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.json")
	rep, _ := nano.AddressToPubkey(someRep)
	var newFront [32]byte
	newFront[0], newFront[31] = 0x12, 0x34
	ledger := nano.AccountInfo{Frontier: fmt.Sprintf("%X", newFront), Balance: "5", Representative: someRep, BlockCount: "7"}
	write := func(at time.Time) *nano.ChainState {
		data := fmt.Sprintf(`{"frontier": "%s", "balance": "9", "representative": "%X", "opened": true, "at": %d}`, someFrontier, rep, at.Unix())
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return nano.LoadChainState(path)
	}
	if !cacheAccountInfo(write(time.Now().Add(time.Hour)), ledger) {
		t.Fatal("a state stamped an hour ahead was trusted as fresh")
	}
	if cacheAccountInfo(write(time.Now().Add(20*time.Second)), ledger) {
		t.Fatal("a state within the clock skew allowance was overwritten")
	}
}

// A cache with a known height that is ahead of the ledger holds a block the
// ledger has not seen yet: just published, or confirmed while the node we
// asked lags behind. Rolling it back would sign the next block on a
// frontier already spent from and fork the chain, so the shorter answer is
// refused whether the cache was written moments or eleven minutes ago; a
// send or receive re-reads the account.
func TestAShorterLedgerChainNeverReplacesAHeightKnownCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.json")
	rep, _ := nano.AddressToPubkey(someRep)
	var mine [32]byte
	mine[0], mine[31] = 0xAA, 0xBB
	ledger := nano.AccountInfo{Frontier: someFrontier, Balance: "10", Representative: someRep, BlockCount: "62"}
	for _, c := range []struct {
		name string
		at   time.Time
	}{
		{"fresh", time.Now()},
		{"eleven minutes old", time.Now().Add(-11 * time.Minute)},
	} {
		data := fmt.Sprintf(`{"frontier": "%X", "balance": "9", "representative": "%X", "opened": true, "height": 63, "at": %d}`, mine, rep, c.at.Unix())
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		st := nano.LoadChainState(path)
		if cacheAccountInfo(st, ledger) {
			t.Fatalf("%s: a shorter ledger chain replaced the cache", c.name)
		}
		front, bal, _, _, _ := st.Get()
		if front != mine || bal.Cmp(big.NewInt(9)) != 0 || st.Height() != 63 {
			t.Fatalf("%s: frontier %X balance %s height %d, want the local state kept", c.name, front, bal, st.Height())
		}
	}
}

// Restore tools and tar can leave a file's mtime at the epoch; nothing that
// just wrote it would. Such a legacy file is stale, not of unknown age.
func TestALegacyCacheWithAnEpochMtimeIsStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.json")
	rep, _ := nano.AddressToPubkey(someRep)
	legacy := fmt.Sprintf(`{"frontier": "%s", "balance": "9", "representative": "%X", "opened": true}`, someFrontier, rep)
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	var newFront [32]byte
	newFront[0], newFront[31] = 0x21, 0x43
	st := nano.LoadChainState(path)
	if !cacheAccountInfo(st, nano.AccountInfo{Frontier: fmt.Sprintf("%X", newFront), Balance: "5", Representative: someRep, BlockCount: "7"}) {
		t.Fatal("a legacy cache with an epoch mtime was never refreshed")
	}
	if front, bal, _, _, _ := st.Get(); front != newFront || bal.Cmp(big.NewInt(5)) != 0 || st.Height() != 7 {
		t.Fatalf("frontier %X balance %s height %d, want the ledger's", front, bal, st.Height())
	}
}

// Nothing an RPC can answer should corrupt the wallet's idea of its chain.
func TestNonsenseFromTheLedgerIsIgnored(t *testing.T) {
	bad := []nano.AccountInfo{
		{},
		{Frontier: "", Balance: "1", Representative: someRep},
		{Frontier: "nothex", Balance: "1", Representative: someRep},
		{Frontier: someFrontier[:60], Balance: "1", Representative: someRep},
		{Frontier: someFrontier + "AA", Balance: "1", Representative: someRep},
		{Frontier: someFrontier, Balance: "", Representative: someRep},
		{Frontier: someFrontier, Balance: "not a number", Representative: someRep},
		{Frontier: someFrontier, Balance: "-5", Representative: someRep},
		{Frontier: someFrontier, Balance: "1", Representative: ""},
		{Frontier: someFrontier, Balance: "1", Representative: "nano_notarealaddress"},
	}
	for i, info := range bad {
		st := newState(t)
		if cacheAccountInfo(st, info) {
			t.Errorf("case %d: nonsense was accepted: %+v", i, info)
		}
		if _, _, _, _, cached := st.Get(); cached {
			t.Errorf("case %d: nonsense was written to the chain state", i)
		}
	}
	if cacheAccountInfo(nil, nano.AccountInfo{Frontier: someFrontier, Balance: "1", Representative: someRep}) {
		t.Error("a nil chain state was accepted")
	}
}
