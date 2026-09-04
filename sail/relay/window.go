package relay

import (
	"sync"

	"github.com/dhyabi2/sail/wire"
)

// cellQueue is the receive side of a windowed stream: an unbounded-by-type
// but window-bounded FIFO of cells, plus the credit bookkeeping that grows
// the window while the consumer keeps up. Memory is only what is buffered.
type cellQueue struct {
	mu       sync.Mutex
	cond     *sync.Cond
	q        [][]byte
	closed   bool
	window   int // cells the peer may have in flight (what we granted)
	consumed int // cells taken since the last CREDIT
	drained  bool
}

func newCellQueue() *cellQueue {
	c := &cellQueue{window: wire.StreamWindow, drained: true}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// push stores a cell; false when the peer overran the window (or closed).
func (c *cellQueue) push(d []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || len(c.q) >= c.window {
		return false
	}
	c.q = append(c.q, d)
	c.cond.Signal()
	return true
}

// pop blocks for the next cell; nil when closed and empty. credit is how
// much window to hand back now (0 = nothing yet): what was consumed, plus
// growth when the consumer drained the queue between credits.
func (c *cellQueue) pop() (d []byte, credit uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.q) == 0 && !c.closed {
		c.drained = true
		c.cond.Wait()
	}
	if len(c.q) == 0 {
		return nil, 0
	}
	d = c.q[0]
	c.q[0] = nil
	c.q = c.q[1:]
	c.consumed++
	if c.consumed >= c.window/4 {
		grow := 0
		if c.drained && c.window < wire.MaxStreamWindow {
			grow = c.window // double: the reader is faster than the network
			if c.window+grow > wire.MaxStreamWindow {
				grow = wire.MaxStreamWindow - c.window
			}
		}
		credit = uint32(c.consumed + grow)
		c.window += grow
		c.consumed = 0
		c.drained = false
	}
	return d, credit
}

func (c *cellQueue) tryPop() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.q) == 0 {
		return nil
	}
	d := c.q[0]
	c.q[0] = nil
	c.q = c.q[1:]
	c.consumed++
	return d
}

func (c *cellQueue) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.q)
}

func (c *cellQueue) close() {
	c.mu.Lock()
	c.closed = true
	c.cond.Broadcast()
	c.mu.Unlock()
}
