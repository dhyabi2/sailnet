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
	if n := len(stA.rx); n == 0 || n > wire.StreamWindow {
		t.Fatalf("stalled stream buffered %d cells", n)
	}
	stA.Close()
}
