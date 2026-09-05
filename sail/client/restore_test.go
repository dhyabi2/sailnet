package client

import (
	"math/big"
	"path/filepath"
	"testing"

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

// A local chain state can be ahead of the ledger, when a block was just sent
// and is not yet confirmed. Overwriting it with the ledger's older view
// would build the next block on a stale frontier, so it is never touched.
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
