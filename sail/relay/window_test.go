package relay

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/dhyabi2/sail/wire"
)

// A reader that drains a full window owes the exit that window back, even
// when it took most cells from the queue in the same Read call as the one
// before (tryPop) and the next pop finds the queue empty. If that credit is
// held until another cell arrives, no cell ever arrives: the exit has
// nothing left to send with, and Read waits forever on an open circuit.
func TestStreamReturnsCreditForEveryCellRead(t *testing.T) {
	near, far := net.Pipe()
	defer near.Close()
	defer far.Close()
	key := &wire.HopKeys{Forward: make([]byte, 32), Backward: make([]byte, 32)}
	peer := &wire.HopKeys{Forward: make([]byte, 32), Backward: make([]byte, 32)}
	c := &Circuit{hops: []*wire.HopKeys{key}, w: newConnWriter(near, true), strms: map[uint16]*Stream{}, Flow: true}
	defer c.w.stop()
	st := newStream(c, 1)

	// The exit filled the whole window while the reader was busy (for
	// instance while the entry link was being reattached).
	cell := make([]byte, wire.MaxData)
	for i := 0; i < wire.StreamWindow; i++ {
		if !st.rx.push(cell) {
			t.Fatalf("push %d refused", i)
		}
	}

	credits := make(chan uint32, 64)
	go func() {
		for {
			lc, err := wire.ReadCell(far)
			if err != nil {
				return
			}
			if lc.Cmd != wire.CmdData {
				continue // padding
			}
			_, cmd, _, data, err := wire.PeelForward(peer, lc.Payload)
			if err == nil && cmd == wire.CmdCredit && len(data) >= 4 {
				credits <- binary.BigEndian.Uint32(data)
			}
		}
	}()

	// Read everything with a buffer that does not divide into cells, as
	// io.Copy and the HTTP client do.
	buf := make([]byte, 32<<10)
	want := wire.StreamWindow * wire.MaxData
	for got := 0; got < want; {
		n, err := st.Read(buf)
		if err != nil {
			t.Fatalf("read after %d bytes: %v", got, err)
		}
		got += n
	}

	var returned uint32
	deadline := time.After(5 * time.Second)
	for returned < wire.StreamWindow*3/4 {
		select {
		case n := <-credits:
			returned += n
		case <-deadline:
			t.Fatalf("read the whole window (%d cells) but returned %d cells of credit: the exit can send nothing more", wire.StreamWindow, returned)
		}
	}
}
