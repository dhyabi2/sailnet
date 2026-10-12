package nano

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const testRep = "nano_1banexkcfuieufzxksfrxqf6xy8e57ry1zdtq9yn7jntzhpwu4pg4hajojmq"

// The released build writes the representative under "representative" and
// knows nothing of heights or times. Its file must keep its representative
// here, and a file written here must keep it for that build: a block signed
// with a zero representative would hand the account's vote to no one.
func TestAChainStateFileFromTheReleasedBuildKeepsItsRepresentative(t *testing.T) {
	rep, err := AddressToPubkey(testRep)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "chain.json")
	old := fmt.Sprintf(`{
  "frontier": "05D1E923B45C08A212E4B022B3BDF6262A2E767023742C68DB0881F3885A617B",
  "balance": "1000",
  "representative": "%X",
  "opened": true
}`, rep)
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	st := LoadChainState(path)
	prev, bal, got, opened, ok := st.Get()
	if !ok || !opened || got != rep || bal.Cmp(big.NewInt(1000)) != 0 {
		t.Fatalf("loaded rep %X balance %s opened %v ok %v", got, bal, opened, ok)
	}

	prev[0] ^= 1
	st.Set(prev, big.NewInt(900), rep, true)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"representative": "`+strings.ToUpper(hex.EncodeToString(rep[:]))+`"`) {
		t.Fatalf("written file lost the representative key the released build reads:\n%s", data)
	}
	if _, _, got, _, _ := LoadChainState(path).Get(); got != rep {
		t.Fatalf("round-trip rep %X, want %X", got, rep)
	}
}

// A ledger read publishes nothing, so it must never grow the recorded
// height — not even when the node leaves block_count out.
func TestALedgerReadNeverGrowsTheHeight(t *testing.T) {
	rep, _ := AddressToPubkey(testRep)
	var noCount atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blockCount := `"block_count": "40",`
		if noCount.Load() {
			blockCount = ""
		}
		fmt.Fprintf(w, `{"frontier": "05D1E923B45C08A212E4B022B3BDF6262A2E767023742C68DB0881F3885A617B", "balance": "1000", %s "representative": "%s"}`, blockCount, testRep)
	}))
	defer srv.Close()
	seed := make([]byte, 32)
	seed[0] = 9
	k, err := DeriveKey(seed, 0)
	if err != nil {
		t.Fatal(err)
	}
	st := &ChainState{Path: filepath.Join(t.TempDir(), "chain.json")}
	a := &Account{Key: k, Client: &Client{URLs: []string{srv.URL}, HTTP: srv.Client(), Budget: NewBudget(50, 100)}, State: st}

	for i := 0; i < 3; i++ {
		if _, _, got, _, err := a.frontier(context.Background()); err != nil || got != rep {
			t.Fatalf("read %d: rep %X err %v", i, got, err)
		}
		if st.Height() != 40 {
			t.Fatalf("read %d: height %d, want the ledger's 40", i, st.Height())
		}
	}
	noCount.Store(true)
	for i := 0; i < 3; i++ {
		if _, _, _, _, err := a.frontier(context.Background()); err != nil {
			t.Fatal(err)
		}
		if st.Height() != 0 {
			t.Fatalf("read %d without block_count: height %d, want unknown (0)", i, st.Height())
		}
	}
}
