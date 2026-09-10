package client

import (
	"encoding/binary"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// A resolver that answers out of order and echoes the question, so the
// test can see that answers go back to the right asker under their own id.
func fakeResolver(t *testing.T, opens *atomic.Int32, closeAfter int) func() (io.ReadWriteCloser, error) {
	return func() (io.ReadWriteCloser, error) {
		opens.Add(1)
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			var pending [][]byte
			n := 0
			for {
				var hdr [2]byte
				if _, err := io.ReadFull(b, hdr[:]); err != nil {
					return
				}
				q := make([]byte, binary.BigEndian.Uint16(hdr[:]))
				if _, err := io.ReadFull(b, q); err != nil {
					return
				}
				n++
				if closeAfter > 0 && n > closeAfter {
					return // the resolver hangs up on an idle-ish connection
				}
				pending = append(pending, q)
				if len(pending) == 2 { // answer the later one first
					for i := len(pending) - 1; i >= 0; i-- {
						ans := append([]byte(nil), pending[i]...)
						ans[2] |= 0x80
						msg := make([]byte, 2+len(ans))
						binary.BigEndian.PutUint16(msg, uint16(len(ans)))
						copy(msg[2:], ans)
						b.Write(msg)
					}
					pending = nil
				}
			}
		}()
		return a, nil
	}
}

func query(id uint16, name byte) []byte {
	q := make([]byte, 12, 16)
	binary.BigEndian.PutUint16(q, id)
	return append(q, 1, name, 0, 0)
}

func TestDNSPipeSharesOneConnectionAndKeepsIDsApart(t *testing.T) {
	var opens atomic.Int32
	p := newDNSPipe(fakeResolver(t, &opens, 0))
	done := make(chan error, 2)
	for _, x := range []struct {
		id   uint16
		name byte
	}{{7, 'a'}, {7, 'b'}} { // two apps, same id, at once
		go func(id uint16, name byte) {
			ans, err := p.resolve(query(id, name), 3*time.Second)
			if err != nil {
				done <- err
				return
			}
			if binary.BigEndian.Uint16(ans) != id || ans[13] != name {
				t.Errorf("answer for %c came back as id %d name %c", name, binary.BigEndian.Uint16(ans), ans[13])
			}
			done <- nil
		}(x.id, x.name)
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if opens.Load() != 1 {
		t.Fatalf("two lookups at once should share one connection, opened %d", opens.Load())
	}
}

func TestDNSPipeReopensAfterTheResolverHangsUp(t *testing.T) {
	var opens atomic.Int32
	p := newDNSPipe(fakeResolver(t, &opens, 2))
	ask := func() error {
		done := make(chan error, 2)
		for i := 0; i < 2; i++ {
			go func() { _, err := p.resolve(query(1, 'x'), 3*time.Second); done <- err }()
		}
		e1, e2 := <-done, <-done
		if e1 != nil {
			return e1
		}
		return e2
	}
	if err := ask(); err != nil {
		t.Fatal(err)
	}
	// The third query is where the resolver hangs up: that pair fails fast...
	if err := ask(); err == nil {
		t.Fatal("a closed connection must fail the queries on it, not hang")
	}
	// ...and the next pair gets a fresh connection.
	if err := ask(); err != nil {
		t.Fatalf("after the resolver hung up the pipe must reopen: %v", err)
	}
	if opens.Load() != 2 {
		t.Fatalf("expected 2 connections, got %d", opens.Load())
	}
}
