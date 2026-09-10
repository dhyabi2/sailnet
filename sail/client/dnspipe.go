package client

import (
	"encoding/binary"
	"io"
	"sync"
	"time"
)

// dnsPipe is one DNS-over-TCP connection to the resolver at the exit, kept
// open across queries, with queries in flight at once (RFC 7766 pipelining).
// Before this every lookup opened its own stream through the circuit: a
// BEGIN, a wait for CONNECTED, the query, the answer — two round trips to
// the relay per name, and a phone that fires dozens of lookups at once
// against a far relay ran them into the stream timeout. Now a lookup is
// one round trip on a connection that is already there, and a burst of
// lookups shares it. The resolver or the exit closing the idle connection
// only means the next lookup opens a new one.
type dnsPipe struct {
	mu      sync.Mutex // conn, waiters, nextID
	wmu     sync.Mutex // one query on the wire at a time; never held with mu, so an answer can always be dispatched
	open    func() (io.ReadWriteCloser, error)
	conn    io.ReadWriteCloser
	waiters map[uint16]chan []byte
	nextID  uint16
}

func newDNSPipe(open func() (io.ReadWriteCloser, error)) *dnsPipe {
	return &dnsPipe{open: open, waiters: map[uint16]chan []byte{}}
}

// resolve sends q and returns the answer with q's own id restored. Two apps
// may ask with the same id at once, so each query travels under an id of
// the pipe's choosing.
func (p *dnsPipe) resolve(q []byte, timeout time.Duration) ([]byte, error) {
	if len(q) < 12 {
		return nil, errors("dns: short query")
	}
	p.mu.Lock()
	if p.conn == nil {
		c, err := p.open()
		if err != nil {
			p.mu.Unlock()
			return nil, err
		}
		p.conn = c
		go p.read(c)
	}
	conn := p.conn
	p.nextID++
	for p.waiters[p.nextID] != nil {
		p.nextID++
	}
	id := p.nextID
	ch := make(chan []byte, 1)
	p.waiters[id] = ch
	p.mu.Unlock()
	msg := make([]byte, 2+len(q))
	binary.BigEndian.PutUint16(msg, uint16(len(q)))
	copy(msg[2:], q)
	binary.BigEndian.PutUint16(msg[2:], id)
	p.wmu.Lock()
	_, err := conn.Write(msg)
	p.wmu.Unlock()
	if err != nil {
		p.mu.Lock()
		delete(p.waiters, id)
		p.drop(conn)
		p.mu.Unlock()
		return nil, err
	}
	select {
	case ans := <-ch:
		if ans == nil {
			return nil, errors("dns: the connection to the resolver closed")
		}
		copy(ans[:2], q[:2])
		return ans, nil
	case <-time.After(timeout):
		p.mu.Lock()
		delete(p.waiters, id)
		p.mu.Unlock()
		return nil, errors("dns: timeout through the circuit")
	}
}

// read hands each answer to the query that asked, until the connection ends;
// then every query still waiting learns that it will get no answer.
func (p *dnsPipe) read(conn io.ReadWriteCloser) {
	for {
		var hdr [2]byte
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			break
		}
		ans := make([]byte, binary.BigEndian.Uint16(hdr[:]))
		if _, err := io.ReadFull(conn, ans); err != nil {
			break
		}
		if len(ans) < 2 {
			continue
		}
		p.mu.Lock()
		if ch := p.waiters[binary.BigEndian.Uint16(ans)]; ch != nil {
			delete(p.waiters, binary.BigEndian.Uint16(ans))
			ch <- ans
		}
		p.mu.Unlock()
	}
	p.mu.Lock()
	p.drop(conn)
	p.mu.Unlock()
}

// drop forgets conn (if it is still the live one) and fails its waiters. Caller holds mu.
func (p *dnsPipe) drop(conn io.ReadWriteCloser) {
	conn.Close()
	if p.conn != conn {
		return
	}
	p.conn = nil
	for id, ch := range p.waiters {
		ch <- nil
		delete(p.waiters, id)
	}
}
