package relay

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dhyabi2/sail/wire"
)

// Circuit is the client side of a telescoping circuit.
type Circuit struct {
	leaf    [32]byte // TLS leaf certificate hash seen at the entry
	Path    []*RelayInfo
	Tag     [32]byte
	conn    net.Conn
	w       *connWriter
	hops    []*wire.HopKeys
	mu      sync.Mutex
	nextS   uint16
	strms   map[uint16]*Stream
	ctl     chan ctlMsg
	Failed  int // index of the hop that failed during build (-1 = none)
	Quota   int64
	closed  bool
	Built   time.Time
	Bytes   int64
	Flow    bool         // the exit understands BEGIN2 / CREDIT: open windowed streams
	OnQuota func(int64)  // called when the entry pushes a low-quota notice
	recv    atomic.Int64 // unix nanos of the last cell received
	pingErr atomic.Value
}

// BytesMoved is the number of bytes received on the circuit so far.
func (c *Circuit) BytesMoved() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Bytes
}

// LastRecv is when the circuit last received a cell from the entry.
func (c *Circuit) LastRecv() time.Time { return time.Unix(0, c.recv.Load()) }

type ctlMsg struct {
	hop  int
	cmd  byte
	sid  uint16
	data []byte
}

// Stream is one TCP-like stream through the exit.
//
// A windowed stream (opened with BEGIN2 against an exit that advertises
// token.FlagFlow) buffers up to wire.StreamWindow cells on its own and
// returns CREDIT as the application reads, so the circuit reader never blocks
// on one slow consumer. A plain stream is a pipe, as before.
type Stream struct {
	c    *Circuit
	id   uint16
	pr   *io.PipeReader
	pw   *io.PipeWriter
	once sync.Once
	ok   chan error

	flow     bool
	rx       *cellQueue // windowed: cells from the exit
	rbuf     []byte     // windowed: unread remainder of the last cell
	done     chan struct{}
	doneOnce sync.Once
	cmu      sync.Mutex
	ccond    *sync.Cond
	credit   int64 // windowed: cells we may still send
}

func newStream(c *Circuit, id uint16) *Stream {
	st := &Stream{c: c, id: id, ok: make(chan error, 1), done: make(chan struct{})}
	if c.Flow {
		st.flow = true
		st.rx = newCellQueue()
		st.credit = wire.StreamWindow
		st.ccond = sync.NewCond(&st.cmu)
	} else {
		st.pr, st.pw = io.Pipe()
	}
	return st
}

// finish ends the stream locally: readers drain what is buffered then see EOF,
// writers fail.
func (s *Stream) finish() {
	s.doneOnce.Do(func() {
		close(s.done)
		if s.pw != nil {
			s.pw.Close()
		}
		if s.rx != nil {
			s.rx.close()
		}
		if s.ccond != nil {
			s.cmu.Lock()
			s.ccond.Broadcast()
			s.cmu.Unlock()
		}
	})
}

func (s *Stream) addCredit(n uint32) {
	s.cmu.Lock()
	s.credit += int64(n)
	s.ccond.Broadcast()
	s.cmu.Unlock()
}

// takeCredit waits for one cell of send window.
func (s *Stream) takeCredit() bool {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	for s.credit <= 0 {
		select {
		case <-s.done:
			return false
		default:
		}
		s.ccond.Wait()
	}
	s.credit--
	return true
}

func (s *Stream) beginCmd() byte {
	if s.flow {
		return wire.CmdBegin2
	}
	return wire.CmdBegin
}

// CoverTick and CoverBurst set the cadence of the client→entry link: one
// tick sends between one and CoverBurst cells, padding when idle. Zero
// disables. The burst is a ceiling per tick, set high enough never to bind.
var (
	CoverTick  = 25 * time.Millisecond
	CoverBurst = 4096 // per tick: the cadence is a floor of one cell, not a rate cap (64 capped links at 2.6 MB/s)
)

func init() { // SAIL_COVER_MS=0 disables cadence mode (measurement only)
	if v := os.Getenv("SAIL_COVER_MS"); v != "" {
		var ms int
		fmt.Sscan(v, &ms)
		CoverTick = time.Duration(ms) * time.Millisecond
	}
}

// Build opens a circuit through path one hop at a time. On error, Failed is
// the index of the hop that could not be reached or did not sign its ack.
// payment, if non-nil, is the JSON array of the two signed SAIL blocks that
// created tag; the entry publishes and verifies them itself (firewall mode).
// Signer signs a CREATE for the tag's owner key (see SignCreate); nil in static test mode.
type Signer func(clientPub, tag [32]byte) []byte

func Build(path []*RelayInfo, tag [32]byte, timeout time.Duration, payment []byte, sign Signer) (*Circuit, error) {
	if len(path) == 0 {
		return nil, errors.New("empty path")
	}
	c := &Circuit{Path: path, Tag: tag, strms: map[uint16]*Stream{}, ctl: make(chan ctlMsg, 16), Failed: -1, nextS: 1}
	conn, err := DialRelay(path[0], timeout)
	if err != nil {
		c.Failed = 0
		return c, fmt.Errorf("hop 0 (%s): %w", short(path[0].Account), err)
	}
	c.conn, c.w = conn, newConnWriter(conn, true)
	c.leaf = LeafHash(conn)
	// Cadence mode on the entry link: both sides send at least one cell per
	// tick from here on, so the link's rhythm no longer follows the user.
	if CoverTick > 0 {
		ms := int(CoverTick / time.Millisecond)
		c.w.write(&wire.Cell{Cmd: wire.CmdCover, Payload: []byte{byte(ms >> 8), byte(ms), byte(CoverBurst >> 8), byte(CoverBurst)}})
		c.w.SetCover(CoverTick, CoverBurst)
	}

	// CREATE with hop 0 (plaintext over TLS).
	priv, pub, err := wire.GenX25519()
	if err != nil {
		return c, err
	}
	create := append(pub[:], tag[:]...)
	if sign != nil {
		create = append(create, sign(pub, tag)...)
	} else {
		create = append(create, make([]byte, 64)...)
	}
	if len(payment) > 0 && len(payment) <= wire.PayloadSize-128 {
		create = append(create, payment...)
	}
	if err := c.w.write(&wire.Cell{CircID: 1, Cmd: wire.CmdCreate, Payload: create}); err != nil {
		c.Failed = 0
		return c, err
	}
	// The entry may need to verify a fresh payment on the ledger (RPC-budgeted), so
	// the CREATE ack gets a long deadline; extensions use the normal timeout.
	conn.SetReadDeadline(time.Now().Add(timeout + 3*time.Minute))
	ack, err := wire.ReadCell(conn)
	if err != nil {
		c.Failed = 0
		return c, fmt.Errorf("hop 0: no CREATED: %w", err)
	}
	if ack.Cmd != wire.CmdCreated {
		c.Failed = 0
		return c, fmt.Errorf("hop 0 refused: %s", ack.Payload)
	}
	keys, err := c.acceptAck(0, priv, pub, ack.Payload)
	if err != nil {
		c.Failed = 0
		return c, err
	}
	c.hops = append(c.hops, keys)
	conn.SetReadDeadline(time.Time{})
	go c.readLoop()

	// EXTEND through each established hop.
	for i := 1; i < len(path); i++ {
		priv, pub, err := wire.GenX25519()
		if err != nil {
			return c, err
		}
		payload := append([]byte(path[i].Account), 0)
		payload = append(payload, pub[:]...)
		if err := c.send(wire.CmdExtend, 0, payload); err != nil {
			c.Failed = i - 1
			return c, err
		}
		msg, err := c.waitCtl(wire.CmdExtended, timeout)
		if err != nil {
			c.Failed = i
			return c, fmt.Errorf("extend to hop %d (%s): %w", i, short(path[i].Account), err)
		}
		if msg.hop != i-1 {
			c.Failed = i - 1
			return c, fmt.Errorf("EXTENDED came from hop %d, expected %d", msg.hop, i-1)
		}
		keys, err := c.acceptAck(i, priv, pub, msg.data)
		if err != nil {
			c.Failed = i
			return c, err
		}
		c.hops = append(c.hops, keys)
	}
	c.Built = time.Now()
	return c, nil
}

// acceptAck verifies CREATED = hopPub[32] ‖ sig[64] against the relay's on-ledger key.
func (c *Circuit) acceptAck(i int, priv, clientPub [32]byte, payload []byte) (*wire.HopKeys, error) {
	if len(payload) < 96 {
		return nil, fmt.Errorf("hop %d: short CREATED", i)
	}
	var hopPub [32]byte
	copy(hopPub[:], payload[:32])
	if len(payload) < 128 {
		return nil, errors.New("short CREATED")
	}
	var certHash [32]byte
	copy(certHash[:], payload[96:128])
	if i == 0 && c.leaf != ([32]byte{}) && certHash != c.leaf {
		return nil, errors.New("entry's certificate does not match what its ledger key signed for (interception?)")
	}
	if !VerifyAck(c.Path[i].Pub, clientPub, hopPub, certHash, payload[32:96]) {
		return nil, fmt.Errorf("hop %d (%s): ack signature does not match its ledger key (impostor?)", i, short(c.Path[i].Account))
	}
	return wire.DeriveHopKeys(priv, hopPub, clientPub, hopPub)
}

func (c *Circuit) send(cmd byte, sid uint16, data []byte) error {
	box, err := wire.OnionSeal(c.hops, cmd, sid, data)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.Bytes += wire.CellSize
	c.mu.Unlock()
	return c.w.write(&wire.Cell{CircID: 1, Cmd: wire.CmdData, Payload: box})
}

// sendTo sends a cell that terminates at hop n (for per-hop PING).
func (c *Circuit) sendTo(n int, cmd byte, sid uint16, data []byte) error {
	box, err := wire.OnionSeal(c.hops[:n+1], cmd, sid, data)
	if err != nil {
		return err
	}
	return c.w.write(&wire.Cell{CircID: 1, Cmd: wire.CmdData, Payload: box})
}

func (c *Circuit) waitCtl(cmd byte, timeout time.Duration) (ctlMsg, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case m := <-c.ctl:
			if m.cmd == cmd {
				return m, nil
			}
			if m.cmd == wire.CmdError {
				return m, fmt.Errorf("hop %d: %s", m.hop, m.data)
			}
		case <-t.C:
			return ctlMsg{}, errors.New("timeout")
		}
	}
}

func (c *Circuit) readLoop() {
	defer c.Close()
	for {
		cell, err := wire.ReadCell(c.conn)
		if err != nil {
			if !c.Closed() {
				log.Printf("circuit: link to the entry dropped: %v", err)
			}
			return
		}
		c.recv.Store(time.Now().UnixNano())
		if cell.Cmd == wire.CmdError {
			c.ctl <- ctlMsg{hop: 0, cmd: wire.CmdError, data: cell.Payload}
			return
		}
		if cell.Cmd != wire.CmdData { // padding, pongs and anything else on the link are dropped here
			continue
		}
		hop, cmd, sid, data, err := wire.PeelBackward(c.hops, cell.Payload)
		if err != nil {
			continue
		}
		c.mu.Lock()
		c.Bytes += wire.CellSize
		st := c.strms[sid]
		last := hop == len(c.hops)-1
		c.mu.Unlock()
		// Stream traffic may only come from the last hop: an entry or middle
		// relay must not be able to inject bytes into, or close, a stream.
		if !last && (cmd == wire.CmdData || cmd == wire.CmdConnected || cmd == wire.CmdEnd) {
			continue
		}
		switch cmd {
		case wire.CmdData:
			if st != nil {
				if st.flow {
					if !st.rx.push(data) { // the exit overran the window: protocol violation, drop the stream
						st.finish()
					}
				} else {
					st.pw.Write(data)
				}
			}
		case wire.CmdCredit:
			if st != nil && st.flow && len(data) >= 4 && last {
				st.addCredit(binary.BigEndian.Uint32(data))
			}
		case wire.CmdConnected:
			if st != nil {
				st.once.Do(func() { st.ok <- nil })
			}
		case wire.CmdEnd:
			if st != nil {
				st.once.Do(func() { st.ok <- fmt.Errorf("stream refused: %s", data) })
				st.finish()
				c.mu.Lock()
				delete(c.strms, sid)
				c.mu.Unlock()
			}
		case wire.CmdQuota:
			if len(data) >= 8 {
				q := int64(binary.BigEndian.Uint64(data))
				c.mu.Lock()
				c.Quota = q
				c.mu.Unlock()
				if sid == wire.QuotaLowStream && hop == 0 && c.OnQuota != nil {
					go c.OnQuota(q) // unsolicited: the entry says the quota is running low
				}
			}
			select {
			case c.ctl <- ctlMsg{hop: hop, cmd: cmd, sid: sid, data: data}:
			default:
			}
		default:
			select {
			case c.ctl <- ctlMsg{hop: hop, cmd: cmd, sid: sid, data: data}:
			default:
			}
		}
	}
}

// Ping checks each hop individually; returns the first hop index that did not answer (-1 if all ok).
func (c *Circuit) Ping(timeout time.Duration) int {
	for i := range c.hops {
		if err := c.sendTo(i, wire.CmdPing, 0, []byte("p")); err != nil {
			c.pingErr.Store("send: " + err.Error())
			return i
		}
		m, err := c.waitCtl(wire.CmdPong, timeout)
		if err != nil {
			c.pingErr.Store("wait: " + err.Error())
			return i
		}
		if m.hop != i {
			c.pingErr.Store(fmt.Sprintf("pong from hop %d, wanted %d", m.hop, i))
			return i
		}
	}
	return -1
}

// PingErr describes why the last Ping failed.
func (c *Circuit) PingErr() string {
	v, _ := c.pingErr.Load().(string)
	return v
}

// QueryQuota asks the entry hop how many prepaid bytes remain.
func (c *Circuit) QueryQuota(timeout time.Duration) (int64, error) {
	if err := c.sendTo(0, wire.CmdQuota, 0, nil); err != nil {
		return 0, err
	}
	m, err := c.waitCtl(wire.CmdQuota, timeout)
	if err != nil {
		return 0, err
	}
	if len(m.data) < 8 {
		return 0, errors.New("short quota")
	}
	return int64(binary.BigEndian.Uint64(m.data)), nil
}

// TopUp hands the entry a signed send block and returns the circuit's new
// remaining quota. The stream id 1 keeps its reply apart from keepalive
// quota queries on id 0.
func (c *Circuit) TopUp(payment []byte, timeout time.Duration) (int64, error) {
	if err := c.sendTo(0, wire.CmdTopUp, 1, payment); err != nil {
		return 0, err
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case m := <-c.ctl:
			if m.cmd == wire.CmdError && m.sid == 1 {
				return 0, fmt.Errorf("%s", m.data)
			}
			if m.cmd == wire.CmdQuota && m.sid == 1 && len(m.data) >= 8 {
				return int64(binary.BigEndian.Uint64(m.data)), nil
			}
		case <-t.C:
			return 0, errors.New("top-up: no answer")
		}
	}
}

// Open begins a stream to host:port through the exit.
func (c *Circuit) Open(target string, timeout time.Duration) (*Stream, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("circuit closed")
	}
	id := c.nextS
	c.nextS++
	st := newStream(c, id)
	c.strms[id] = st
	c.mu.Unlock()
	if err := c.send(st.beginCmd(), id, []byte(target)); err != nil {
		return nil, err
	}
	select {
	case err := <-st.ok:
		if err != nil {
			return nil, err
		}
		return st, nil
	case <-time.After(timeout):
		c.send(wire.CmdEnd, id, nil)
		return nil, errors.New("connect timeout through exit")
	}
}

// OpenOptimistic sends BEGIN and returns the stream immediately; data written
// before CONNECTED is buffered by the exit and flushed once connected.
func (c *Circuit) OpenOptimistic(target string) (*Stream, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("circuit closed")
	}
	id := c.nextS
	c.nextS++
	st := newStream(c, id)
	c.strms[id] = st
	c.mu.Unlock()
	if err := c.send(st.beginCmd(), id, []byte(target)); err != nil {
		return nil, err
	}
	return st, nil
}

func (s *Stream) Read(p []byte) (int, error) {
	if !s.flow {
		return s.pr.Read(p)
	}
	if len(s.rbuf) == 0 {
		d, credit := s.rx.pop() // blocks; nil once the stream ended and the queue is empty
		if d == nil {
			return 0, io.EOF
		}
		s.rbuf = d
		if credit > 0 {
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], credit)
			s.c.send(wire.CmdCredit, s.id, b[:])
		}
	}
	n := copy(p, s.rbuf)
	s.rbuf = s.rbuf[n:]
	// Fill the rest of p from cells already queued: fewer, larger reads.
	for len(s.rbuf) == 0 && n < len(p) {
		d := s.rx.tryPop()
		if d == nil {
			break
		}
		k := copy(p[n:], d)
		s.rbuf = d[k:]
		n += k
	}
	return n, nil
}

func (s *Stream) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > wire.MaxData {
			n = wire.MaxData
		}
		if s.flow && !s.takeCredit() {
			return total, errors.New("stream closed")
		}
		if err := s.c.send(wire.CmdData, s.id, p[:n]); err != nil {
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}

// Close ends the stream.
func (s *Stream) Close() error {
	s.c.mu.Lock()
	delete(s.c.strms, s.id)
	s.c.mu.Unlock()
	s.finish()
	return s.c.send(wire.CmdEnd, s.id, nil)
}

// Close tears the circuit down.
func (c *Circuit) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	for _, st := range c.strms {
		st.finish()
	}
	c.mu.Unlock()
	if c.w != nil {
		c.w.write(&wire.Cell{CircID: 1, Cmd: wire.CmdDestroy})
		c.w.drain(200 * time.Millisecond)
		c.w.stop()
	}
	if c.conn != nil {
		c.conn.Close()
	}
}

// Closed reports whether the circuit is down.
// Streams is the number of open streams on the circuit.
func (c *Circuit) Streams() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.strms)
}

func (c *Circuit) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}
