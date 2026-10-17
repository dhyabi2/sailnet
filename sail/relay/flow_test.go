package relay

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dhyabi2/sail/wire"
)

// A windowed stream carries far more than one window in both directions and
// a stalled stream does not hold back another on the same circuit.
func TestWindowedStreams(t *testing.T) {
	reg := &Registry{}
	s, ri, _ := startRelay(t, reg, 7, true)
	var tag [32]byte
	copy(tag[:], "flow-test-payment-hash-placehold")
	s.Quota.Credit(hex.EncodeToString(tag[:]), 200<<20, "")

	body := bytes.Repeat([]byte("0123456789abcdef"), 6<<20/16) // 6 MiB, three windows
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			got, _ := io.ReadAll(r.Body)
			if !bytes.Equal(got, body) {
				http.Error(w, "upload corrupted", 500)
				return
			}
			w.Write([]byte("ok"))
			return
		}
		w.Write(body)
	}))
	defer target.Close()

	c, err := Build([]*RelayInfo{ri}, tag, 10*time.Second, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Flow = true
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		st, err := c.Open(addr, 5*time.Second)
		if err != nil {
			return nil, err
		}
		return fakeConn{st}, nil
	}}
	hc := &http.Client{Transport: tr, Timeout: 60 * time.Second}

	// Down: three windows' worth, read slowly enough that credits must flow.
	r, err := hc.Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r.Body)
	if !bytes.Equal(got, body) {
		t.Fatalf("download: got %d bytes, want %d", len(got), len(body))
	}
	// Up: the same through the client's send window.
	r2, err := hc.Post(target.URL, "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(r2.Body); string(b) != "ok" {
		t.Fatalf("upload: %s", b)
	}

	// Head of line: stream A is opened and never read; the exit fills A's
	// window and must then leave the circuit alone so B still completes.
	stA, err := c.Open(target.Listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	stA.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
	time.Sleep(500 * time.Millisecond) // let A's window fill
	done := make(chan error, 1)
	go func() {
		r3, err := hc.Get(target.URL)
		if err != nil {
			done <- err
			return
		}
		b, _ := io.ReadAll(r3.Body)
		if !bytes.Equal(b, body) {
			done <- io.ErrUnexpectedEOF
			return
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stream B behind a stalled A: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("stream B blocked behind stalled stream A")
	}
	if n := stA.rx.len(); n == 0 || n > wire.MaxStreamWindow {
		t.Fatalf("stalled stream buffered %d cells", n)
	}
	stA.Close()
}

func TestTryPopReturnsCreditOnLargeReads(t *testing.T) {
	q := newCellQueue()
	for i := 0; i < wire.StreamWindow; i++ {
		if !q.push([]byte("x")) {
			t.Fatalf("push %d failed", i)
		}
	}
	// One blocking pop followed by tryPop for the rest of the window must
	// grant credit at the quarter-window threshold, not swallow it.
	_, c0 := q.pop()
	total := c0
	for {
		d, c := q.tryPop()
		total += c
		if d == nil {
			break
		}
	}
	if total < uint32(wire.StreamWindow) {
		t.Fatalf("draining a full window via tryPop returned %d credit, want at least %d", total, wire.StreamWindow)
	}
}

func TestPingSkipsStalePongFromEarlierHop(t *testing.T) {
	reg := &Registry{}
	var infos []*RelayInfo
	for i := 0; i < 2; i++ {
		s, ri, _ := startRelay(t, reg, i+40, i == 1)
		s.Quota.Credit("50494E475354414C45544553545041594D454E54484153483030303030303030", 50<<20, "")
		if i == 1 {
			s.Quota.Credit(PoolTag(infos[0].Account, ri.Account), 50<<20, "")
		}
		infos = append(infos, ri)
	}
	var tag [32]byte
	hex.Decode(tag[:], []byte("50494E475354414C45544553545041594D454E54484153483030303030303030"))
	c, err := Build(infos, tag, 10*time.Second, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Leave a stale pong from hop 1 in ctl before Ping starts: hop 0's ping
	// must skip it and wait for hop 0's own pong instead of failing with
	// "pong from hop 1, wanted 0".
	c.ctl <- ctlMsg{hop: 1, cmd: wire.CmdPong}
	if bad := c.Ping(5 * time.Second); bad != -1 {
		t.Fatalf("Ping failed at hop %d (%s) because of a stale pong", bad, c.PingErr())
	}
}
