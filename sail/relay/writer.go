package relay

import (
	"net"
	"sort"
	"sync"
	"time"

	"github.com/dhyabi2/sail/shape"
	"github.com/dhyabi2/sail/wire"
)

// connWriter serialises cell writes on one tunnel connection and shapes them.
//
// Writes are queued and drained by one flusher goroutine, so cells produced
// back to back by a stream pump leave as a single burst instead of one record
// each. That is what makes coalescing worth anything: with a synchronous
// writer every producer waited for its own flush, so a 200 KiB download went
// out as 220 records of about one cell, a shape no ordinary HTTPS session has.
//
// The burst is then cut into TLS records by shape.Shaper, which replays the
// record sizes of real HTTPS for the first records of the connection and
// afterwards cuts at sizes drawn from the same measurement.
type connWriter struct {
	mu     sync.Mutex
	c      net.Conn
	sh     *shape.Shaper
	err    error
	q      chan []byte // data cells
	ctl    chan []byte // control cells: circuit 0, CREATE/EXTEND/PING, never queued behind data
	done   chan struct{}
	closed bool
	last   time.Time
	carry  [][]byte // cells taken while waiting for a held remainder
}

// writerQueue bounds the data in flight per connection. A full queue blocks
// the producer, which is the back-pressure the old synchronous writer had. It
// is kept small on purpose: on a slow path every queued cell is time a PONG
// spends waiting, and the client's keepalive would mistake that for a dead hop.
const writerQueue = 48

// newConnWriter starts a writer for one connection. client is true on the side
// that opened it (it masks WebSocket frames, so its framing overhead differs).
func newConnWriter(c net.Conn, client bool) *connWriter {
	w := &connWriter{c: c, sh: shape.NewShaper(client), q: make(chan []byte, writerQueue), ctl: make(chan []byte, 64), done: make(chan struct{})}
	go w.flush()
	return w
}

// chunker adapts the connection to shape.ChunkWriter: one Write is one TLS
// record, and WireSize accounts for the TLS and WebSocket overhead so the
// shaper can hit an exact size on the wire.
type chunker struct{ c net.Conn }

func (k chunker) WriteChunk(p []byte) error { _, err := k.c.Write(p); return err }

func (k chunker) WireSize(n int) int {
	over := 5 + 17 // TLS 1.3 record header, inner content type and AEAD tag
	if ws, ok := k.c.(*wire.WSConn); ok {
		over += ws.HeaderLen(n)
	}
	return n + over
}

// write queues one cell. The error returned is the last write error seen on
// this connection, so a broken tunnel still stops its callers.
func (w *connWriter) write(cell *wire.Cell) error { return w.writeBatch(cell) }

// writeBatch queues several cells at once. A stream pump that has just read a
// large chunk should use it: the cells then cannot be separated by the
// flusher's timing.
func (w *connWriter) writeBatch(cells ...*wire.Cell) error {
	w.mu.Lock()
	err, closed := w.err, w.closed
	w.mu.Unlock()
	if err != nil {
		return err
	}
	if closed {
		return net.ErrClosed
	}
	for _, c := range cells {
		q := w.q
		if isCtlCell(c) {
			q = w.ctl
		}
		select {
		case q <- c.Marshal():
		case <-w.done:
			return net.ErrClosed
		}
	}
	return nil
}

// Control cells are circuit-0 cells (PING/PONG, gossip) and CREATE/CREATED.
// Anything carried inside a circuit's onion layers keeps its order, because
// the replay windows are sequence-numbered per hop.
func isCtlCell(c *wire.Cell) bool {
	return c.CircID == 0 || c.Cmd == wire.CmdCreate || c.Cmd == wire.CmdCreated
}

func isCtl(b []byte) bool {
	return len(b) >= 5 && (b[0]|b[1]|b[2]|b[3] == 0 || b[4] == wire.CmdCreate || b[4] == wire.CmdCreated)
}

// drain waits, briefly, for queued cells to reach the wire. Callers that are
// about to close the connection use it so a final DESTROY is not lost.
func (w *connWriter) drain(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		err := w.err
		w.mu.Unlock()
		if err != nil || len(w.q)+len(w.ctl) == 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// stop releases the flusher. Safe to call more than once.
func (w *connWriter) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		w.closed = true
		close(w.done)
	}
}

// flush drains the queue: it waits for the first cell, gives the producers the
// coalescing window to add more, then writes everything as one shaped burst.
func (w *connWriter) flush() {
	for {
		var batch [][]byte
		w.mu.Lock()
		batch, w.carry = w.carry, nil
		w.mu.Unlock()
		if len(batch) == 0 {
			select {
			case b := <-w.ctl:
				batch = append(batch, b)
			case b := <-w.q:
				batch = append(batch, b)
			case <-w.done:
				return
			}
		}
		p := shape.Get()
		if p.Coalesce > 0 {
			// Gather while cells keep arriving within the quiet gap of each
			// other, until the byte target, the delay cap, or silence.
			size := len(batch[0])
			cap := time.NewTimer(maxDelay(p))
			quiet := time.NewTimer(p.Coalesce)
		gather:
			for size < flushBytes(p) && len(batch) < writerQueue {
				select {
				case b := <-w.ctl:
					batch = append(batch, b)
					size += len(b)
					quiet.Reset(p.Coalesce)
				case b := <-w.q:
					batch = append(batch, b)
					size += len(b)
					quiet.Reset(p.Coalesce)
				case <-quiet.C:
					break gather
				case <-cap.C:
					break gather
				case <-w.done:
					cap.Stop()
					quiet.Stop()
					return
				}
			}
			cap.Stop()
			quiet.Stop()
		} else {
			for {
				select {
				case b := <-w.ctl:
					batch = append(batch, b)
					continue
				case b := <-w.q:
					batch = append(batch, b)
					continue
				default:
				}
				break
			}
		}
		// Control cells go first in the burst regardless of arrival order.
		sort.SliceStable(batch, func(i, j int) bool { return isCtl(batch[i]) && !isCtl(batch[j]) })
		idle := time.Since(w.last)
		w.last = time.Now()
		if idle > p.IdleGap && shape.Chance(p.PadAfterIdle) {
			batch = append([][]byte{wire.PaddingCell()}, batch...) // a burst after quiet starts with cover
		}
		if shape.Chance(p.PadTail) {
			batch = append(batch, wire.PaddingCell()) // and now and then ends with it
		}
		buf := make([]byte, 0, len(batch)*wire.CellSize)
		for _, b := range batch {
			buf = append(buf, b...)
		}
		w.c.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if err := w.sh.Write(chunker{w.c}, buf, wire.PaddingCell); err != nil {
			w.fail(err)
			return
		}
		// Bytes held back for the next burst must not wait forever: if
		// nothing follows within the delay cap, they go out on their own.
		if w.sh.Pending() > 0 {
			t := time.NewTimer(maxDelay(p))
			select {
			case b := <-w.ctl:
				t.Stop()
				w.pushBack(b)
			case b := <-w.q:
				t.Stop()
				w.pushBack(b)
			case <-t.C:
				if err := w.sh.Flush(chunker{w.c}); err != nil {
					w.fail(err)
					return
				}
			case <-w.done:
				t.Stop()
				return
			}
		}
	}
}

func (w *connWriter) fail(err error) {
	w.mu.Lock()
	w.err = err
	w.mu.Unlock()
	w.stop()
}

// pushBack returns a cell taken while waiting to the front of the next burst.
func (w *connWriter) pushBack(b []byte) {
	w.mu.Lock()
	w.carry = append(w.carry, b)
	w.mu.Unlock()
}

// Overhead reports the padding and payload bytes this connection has written.
func (w *connWriter) Overhead() (pad, data int) { return w.sh.Padding, w.sh.Data }

func maxDelay(p *shape.Params) time.Duration {
	if p.MaxDelay <= 0 {
		return 200 * time.Millisecond
	}
	return p.MaxDelay
}

func flushBytes(p *shape.Params) int {
	if p.FlushBytes <= 0 {
		return 16 * 1024
	}
	return p.FlushBytes
}
