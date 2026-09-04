package relay

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/dhyabi2/sail/wire"
)

// A cadence-mode writer sends at least one cell per tick, padding when idle.
func TestCoverCadenceSendsWhenIdle(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	w := newConnWriter(a, true)
	defer w.stop()
	w.SetCover(10*time.Millisecond, 4)
	// wire.ReadCell drops padding on purpose, so read raw cells here.
	r := bufio.NewReader(b)
	b.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	n := 0
	raw := make([]byte, wire.CellSize)
	for i := 0; i < 8; i++ {
		if _, err := io.ReadFull(r, raw); err != nil {
			break
		}
		if raw[4] != wire.CmdPadding {
			t.Fatalf("expected padding on an idle link, got cmd %d", raw[4])
		}
		n++
	}
	if n < 5 {
		t.Fatalf("idle link sent %d cells in 400 ms at a 10 ms cadence", n)
	}
}

// The ledger channel on circuit 0 answers a fresh client through the entry;
// with no ledger source configured the relay says so, which proves the
// request and the chunked reply both travel.
func TestLedgerOverEntry(t *testing.T) {
	reg := &Registry{}
	_, info, _ := startRelay(t, reg, 7, false)
	e := NewEntryRPC(info, 5*time.Second)
	defer e.Close()
	out, err := e.Call([]byte(`{"action":"block_count"}`), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "no ledger source") {
		t.Fatalf("unexpected answer %s", out)
	}
	out, err = e.Call([]byte(`{"action":"wallet_create"}`), 5*time.Second)
	if err != nil || !strings.Contains(string(out), "not allowed") {
		t.Fatalf("a wallet action must be refused: %s %v", out, err)
	}
}
