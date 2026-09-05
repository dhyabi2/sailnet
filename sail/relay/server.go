package relay

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	mathrand "math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dhyabi2/sail/nano"
	"github.com/dhyabi2/sail/shape"
	"github.com/dhyabi2/sail/token"
	"github.com/dhyabi2/sail/wire"
	utls "github.com/refraction-networking/utls"
	"golang.org/x/crypto/blake2b"
)

// Server is one Sailnet relay.
type Server struct {
	watch    atomic.Pointer[Watcher] // upstream confirmation feed, started on first CmdWatch
	Key      *nano.Key
	Nano     *nano.Client
	Quota    *Quota
	TLS      tls.Certificate
	Registry *Registry
	Self     *RelayInfo // our own record, gossiped to peers and clients (nil for home nodes and tests)
	// BridgeSecret, when set, is required in the tunnel token: probers without
	// it get the decoy site. Connections that present it may also open one
	// small free bootstrap circuit (BootstrapBytes), so a first-run client in
	// a censored network can reach the ledger through the bridge before it
	// has any cached state.
	BridgeSecret   [16]byte
	BootstrapBytes int64
	// GetCertificate, when set (ACME), supplies the live certificate instead of
	// TLS; the ack then binds whatever leaf is being served right now.
	GetCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	Host           string  // the name the certificate is for
	Faucet         *Faucet // optional: /faucet on the HTTPS listener

	parkMu sync.Mutex
	parks  map[[32]byte]*circuit // circuits whose client link dropped, kept for a minute

	lastRegRefresh time.Time // on-demand registry re-read for newcomers, rate-limited

	connMu       sync.Mutex
	conns        map[*connWriter]bool // live client/relay connections, for a graceful shutdown
	draining     bool
	Exit         bool
	AllowPrivate bool     // test mode only: let the exit reach loopback/LAN targets
	PoolRaw      *big.Int // downstream pool top-up size (raw XNO); nil = static pool tags (test mode)
	Decoy        string   // HTML served to everyone else

	mu        sync.Mutex
	pools     map[string]*pool // downstream relay account → pool
	PoolsFile string           // if set, pools survive restarts (no re-prepaying every peer on start)

	// anti-spam for unverified payment tags: each verification costs RPC calls,
	// so unknown tags are rate-limited per source IP and globally, and tags that
	// failed are remembered for a while.
	homeMu  sync.Mutex
	homes   map[string]*homeLink // home-node account → its reverse tunnel
	bridges map[*connWriter]map[uint32]bridge

	verMu     sync.Mutex
	verBusy   map[string]bool      // tag → verification in flight (retries wait, not spam)
	verBad    map[string]time.Time // tag → when it failed
	verPerIP  map[string][]time.Time
	verGlobal []time.Time

	Metrics Metrics
}

// Metrics are the relay's running counters (logged hourly, saved to metrics.json).
type Metrics struct {
	Circuits     atomic.Int64
	Payments     atomic.Int64
	PaymentsBad  atomic.Int64
	BytesRelayed atomic.Int64
	BytesExit    atomic.Int64
	Streams      atomic.Int64
	RejectedSpam atomic.Int64
	Started      time.Time
}

// allowVerify decides whether we may spend RPC calls verifying tag from ip.
func (s *Server) allowVerify(tag, ip string) bool {
	s.verMu.Lock()
	defer s.verMu.Unlock()
	now := time.Now()
	if s.verBad == nil {
		s.verBad, s.verPerIP = map[string]time.Time{}, map[string][]time.Time{}
	}
	if t, bad := s.verBad[tag]; bad && now.Sub(t) < 10*time.Minute {
		return false
	}
	if s.verBusy == nil {
		s.verBusy = map[string]bool{}
	}
	if s.verBusy[tag] {
		return true // same tag, verification already running: let it wait, don't count it
	}
	prune := func(ts []time.Time, window time.Duration) []time.Time {
		out := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < window {
				out = append(out, t)
			}
		}
		return out
	}
	s.verPerIP[ip] = prune(s.verPerIP[ip], time.Minute)
	s.verGlobal = prune(s.verGlobal, time.Minute)
	if len(s.verPerIP[ip]) >= 3 || len(s.verGlobal) >= 12 {
		return false
	}
	s.verPerIP[ip] = append(s.verPerIP[ip], now)
	s.verGlobal = append(s.verGlobal, now)
	return true
}

func (s *Server) markBad(tag string) {
	s.verMu.Lock()
	if s.verBad == nil {
		s.verBad = map[string]time.Time{}
	}
	s.verBad[tag] = time.Now()
	s.verMu.Unlock()
}

type pool struct {
	tag      string // hash of our SAIL transfer to the peer (or a static test tag)
	lastTop  time.Time
	credited int64 // bytes we believe we have prepaid there
	used     int64
	topping  bool // a top-up is in flight (one at a time per peer)
	rateAt   time.Time
	rateN    int64
	rate     int64 // bytes per second through this pool, recent window
}

// homeLink is a home node's outbound tunnel that we accept circuits into.
type homeLink struct {
	w      *connWriter
	nextID atomic.Uint32
	pool   string // the home node prepaid us for relaying its traffic
}

// connWriter serialises cell writes on one connection.

// circuit is one hop's view of a circuit.
type circuit struct {
	id       uint32 // circID on the inbound connection
	tag      string
	keys     *wire.HopKeys
	in       *connWriter
	next     *connWriter // toward the next hop (nil if we are last)
	nextID   uint32
	poolAcct string // downstream relay whose pool this circuit consumes (metered per cell)
	streams  map[uint16]*exitStream
	pending  map[uint16][][]byte // data that arrived before the stream connected (optimistic BEGIN)
	lowSent  bool                // a low-quota notice was pushed since the last top-up
	resume   [32]byte            // proves ownership when the client reattaches after a dropped link
	parkedAt time.Time
	sealMu   sync.Mutex // seal + ring push, so ring order is sequence order
	parkCond *sync.Cond // wakes deliver() when the client link is back (or the circuit is destroyed)
	ring     []ringCell // last backward cells sent, for retransmission after a reattach
	rateAt   time.Time  // rate window start (for the low-quota threshold)
	rateN    int64      // bytes relayed in the window
	rate     int64      // bytes per second over the last window
	mu       sync.Mutex
	closed   bool
}

// exitStream is one stream at the exit. A windowed stream (BEGIN2) keeps the
// client's upstream cells in its own queue and drains them to the target from
// a goroutine, and sends downstream only while the client has granted window;
// so a slow target or a slow client never blocks the circuit's reader.
type exitStream struct {
	conn   net.Conn
	flow   bool
	mu     sync.Mutex
	cond   *sync.Cond
	credit int64      // cells we may still send downstream
	up     *cellQueue // client → target, bounded by the window we granted
	done   chan struct{}
	once   sync.Once
}

func newExitStream(conn net.Conn, flow bool) *exitStream {
	st := &exitStream{conn: conn, flow: flow, done: make(chan struct{})}
	if flow {
		st.cond = sync.NewCond(&st.mu)
		st.credit = wire.StreamWindow
		st.up = newCellQueue()
	}
	return st
}

func (st *exitStream) Close() {
	st.once.Do(func() {
		close(st.done)
		st.conn.Close()
		if st.up != nil {
			st.up.close()
		}
		if st.cond != nil {
			st.mu.Lock()
			st.cond.Broadcast()
			st.mu.Unlock()
		}
	})
}

func (st *exitStream) addCredit(n uint32) {
	st.mu.Lock()
	st.credit += int64(n)
	st.cond.Broadcast()
	st.mu.Unlock()
}

// takeCredit blocks until at least one cell of window is available and
// returns how many cells (at most max) may be sent; 0 means the stream ended.
func (st *exitStream) takeCredit(max int) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	for st.credit <= 0 {
		select {
		case <-st.done:
			return 0
		default:
		}
		st.cond.Wait()
	}
	n := int(st.credit)
	if n > max {
		n = max
	}
	st.credit -= int64(n)
	return n
}

// pumpUp drains the client's cells to the target and returns window.
func (s *Server) pumpUp(c *circuit, sid uint16, st *exitStream, already int) {
	if already > 0 { // optimistic data written before the pump started
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(already))
		s.reply(c, wire.CmdCredit, sid, b[:])
	}
	for {
		d, credit := st.up.pop()
		if d == nil {
			return
		}
		st.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := st.conn.Write(d); err != nil {
			st.Close()
			return
		}
		if credit > 0 {
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], credit)
			s.reply(c, wire.CmdCredit, sid, b[:])
		}
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	if s.Faucet != nil {
		mux.Handle("/faucet", s.Faucet)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// The tunnel looks like a WebSocket upgrade. Only a request whose path
		// carries today's token (and the bridge secret, if any) is a tunnel;
		// everything else, including a correct-looking WebSocket request on
		// another path, gets the decoy site or 404 exactly like a small site would.
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && CheckTunnelPath(s.Key.Public, s.BridgeSecret, r.URL.Path, time.Now()) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.NotFound(w, r)
				return
			}
			conn, rw, err := hj.Hijack()
			if err != nil {
				return
			}
			accept := wsAccept(r.Header.Get("Sec-WebSocket-Key"))
			rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n")
			rw.Flush()
			ws := wire.NewWSConn(conn, rw.Reader, false) // cells ride in real WebSocket frames from here on
			ws.Ping()                                    // a small first record, like a real WebSocket session
			go s.serveConn(ws, bufio.NewReaderSize(ws, 64<<10), false)
			return
		}
		// Everyone else sees an ordinary small website.
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(s.Decoy))
	})
	return mux
}

// ListenAndServe runs the HTTPS listener.
func (s *Server) ListenAndServe(addr string) error {
	if s.pools == nil {
		s.pools = map[string]*pool{}
	}
	// HTTP/1.1 only: the tunnel is an HTTP/1.1 Upgrade, and a browser-like
	// ClientHello offers h2, which must not be negotiated.
	tcfg := &tls.Config{Certificates: []tls.Certificate{s.TLS}, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	if s.GetCertificate != nil {
		tcfg.Certificates = nil
		tcfg.GetCertificate = s.GetCertificate
		tcfg.NextProtos = append(tcfg.NextProtos, "acme-tls/1") // TLS-ALPN-01 validation on :443
	}
	srv := &http.Server{Addr: addr, Handler: s.Handler(), TLSConfig: tcfg, ReadHeaderTimeout: 10 * time.Second, TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){}}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return srv.ServeTLS(ln, "", "")
}

// ServeHomeTunnel runs the relay's cell loop on an outbound connection to a
// harbour: circuits created by the harbour arrive here as CREATE cells.
// The tunnel is kept alive with a PING on circuit 0 every 30 s (the harbour
// drops any connection silent for 90 s).
func (s *Server) ServeHomeTunnel(conn net.Conn, r *bufio.Reader) { s.serveConn(conn, r, true) }

// serveConn runs the cell loop for one inbound tunnel connection.
func (s *Server) serveConn(conn net.Conn, r *bufio.Reader, heartbeat bool) {
	defer conn.Close()
	in := newConnWriter(conn, false)
	defer in.stop()
	circs := map[uint32]*circuit{}
	var rpcParts map[uint16][]byte // chunked ledger requests in progress
	s.connMu.Lock()
	if s.conns == nil {
		s.conns = map[*connWriter]bool{}
	}
	s.conns[in] = true
	draining := s.draining
	s.connMu.Unlock()
	defer func() {
		s.connMu.Lock()
		delete(s.conns, in)
		s.connMu.Unlock()
	}()
	if draining {
		in.write(&wire.Cell{Cmd: wire.CmdError, Payload: []byte("relay restarting; use another entry")})
		return
	}
	defer func() {
		for _, c := range circs {
			s.park(c) // extended circuits wait a minute for the client to reattach
		}
		s.detachHome(in)
		s.homeMu.Lock()
		delete(s.bridges, in)
		s.homeMu.Unlock()
		if w := s.watch.Load(); w != nil {
			w.Unwatch(in)
		}
	}()
	if heartbeat {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			for {
				select {
				case <-stop:
					return
				case <-time.After(20*time.Second + time.Duration(mathrand.Intn(20000))*time.Millisecond): // jittered
					if in.write(&wire.Cell{Cmd: wire.CmdPing}) != nil {
						conn.Close()
						return
					}
				}
			}
		}()
	}
	idle := 90 * time.Second
	if heartbeat {
		idle = 5 * time.Minute // an idle harbour sends nothing; the PINGs' write errors detect a dead link
	}
	for {
		conn.SetReadDeadline(time.Now().Add(idle))
		cell, err := wire.ReadCell(r)
		if err != nil {
			return
		}
		if cell.Cmd == wire.CmdPadding {
			continue // cover traffic
		}
		if cell.Cmd == wire.CmdPing && cell.CircID == 0 { // tunnel keepalive from a home node
			in.write(&wire.Cell{Cmd: wire.CmdPong})
			continue
		}
		if cell.Cmd == wire.CmdRelays && cell.CircID == 0 { // gossip: who do you know?
			s.sendRelays(in)
			continue
		}
		if cell.Cmd == wire.CmdCover && cell.CircID == 0 && len(cell.Payload) >= 3 { // cadence mode on this link
			tick := time.Duration(int(cell.Payload[0])<<8|int(cell.Payload[1])) * time.Millisecond
			burst := int(cell.Payload[2]) // oldest clients: a single byte
			if len(cell.Payload) >= 5 {   // current: byte 2 is a legacy marker, the value is in 3..4
				burst = int(cell.Payload[3])<<8 | int(cell.Payload[4])
			} else if len(cell.Payload) >= 4 { // the shape shipped on 2026-09-04
				burst = int(cell.Payload[2])<<8 | int(cell.Payload[3])
			}
			in.SetCover(tick, burst)
			continue
		}
		if cell.Cmd == wire.CmdRPC && cell.CircID == 0 { // ledger for a client that has no circuit yet
			go s.handleRPC(cell, in, conn.RemoteAddr().String())
			continue
		}
		if cell.Cmd == wire.CmdRPCMulti && cell.CircID == 0 { // a request too large for one cell, in chunks
			if rpcParts == nil {
				rpcParts = map[uint16][]byte{}
			}
			if len(cell.Payload) == 0 { // end of request
				body := rpcParts[cell.StreamID]
				delete(rpcParts, cell.StreamID)
				if len(body) > 0 {
					go s.handleRPC(&wire.Cell{Cmd: wire.CmdRPC, StreamID: cell.StreamID, Payload: body}, in, conn.RemoteAddr().String())
				}
				continue
			}
			if len(rpcParts[cell.StreamID])+len(cell.Payload) <= 64<<10 {
				rpcParts[cell.StreamID] = append(rpcParts[cell.StreamID], cell.Payload...)
			}
			continue
		}
		if cell.Cmd == wire.CmdWatch && cell.CircID == 0 { // push this account's confirmations to me
			if acct := string(cell.Payload); len(acct) == 65 && acct[:5] == "nano_" {
				s.watcher().Watch(acct, in)
			}
			continue
		}
		switch cell.Cmd {
		case wire.CmdHomeHello:
			if err := s.handleHomeHello(cell, in); err != nil {
				in.write(&wire.Cell{CircID: cell.CircID, Cmd: wire.CmdError, Payload: []byte(err.Error())})
				return
			}
		case wire.CmdCreate:
			if len(cell.Payload) > 128+len("via:") && string(cell.Payload[128:132]) == "via:" {
				// CREATE for a home node behind us: forward over its reverse tunnel.
				s.forwardCreateToHome(cell, in)
				continue
			}
			c, err := s.handleCreate(cell, in)
			if err != nil {
				in.write(&wire.Cell{CircID: cell.CircID, Cmd: wire.CmdError, Payload: []byte(err.Error())})
				continue
			}
			circs[cell.CircID] = c
		case wire.CmdDestroy:
			if c := circs[cell.CircID]; c != nil {
				c.destroy()
				delete(circs, cell.CircID)
			}
		case wire.CmdResume:
			if c := s.handleResume(cell, in); c != nil {
				circs[cell.CircID] = c
			} else {
				in.write(&wire.Cell{CircID: cell.CircID, Cmd: wire.CmdError, Payload: []byte("nothing to resume")})
			}
		default:
			if s.bridged(in, cell) {
				continue
			}
			c := circs[cell.CircID]
			if c == nil {
				continue
			}
			// Cells already in the buffer that belong to the same circuit
			// travel as one batch: one peel loop, one metering, one write.
			batch := []*wire.Cell{cell}
			for len(batch) < 64 && r.Buffered() >= wire.CellSize {
				more, err := wire.ReadCell(r)
				if err != nil {
					break
				}
				if more.CircID != cell.CircID || more.Cmd != wire.CmdData {
					s.handleRelayBatch(c, batch)
					batch = batch[:0]
					if more.Cmd == wire.CmdPadding {
						continue
					}
					if oc := circs[more.CircID]; oc != nil && more.Cmd == wire.CmdData && !s.bridged(in, more) {
						s.handleRelay(oc, more)
					}
					continue
				}
				batch = append(batch, more)
			}
			if len(batch) > 0 {
				s.handleRelayBatch(c, batch)
			}
		}
	}
}

// detachHome forgets a home node whose reverse tunnel was this connection.
func (s *Server) detachHome(in *connWriter) {
	s.homeMu.Lock()
	defer s.homeMu.Unlock()
	for acct, hl := range s.homes {
		if hl.w == in {
			delete(s.homes, acct)
			log.Printf("home node %s detached", short(acct))
		}
	}
}

// handleHomeHello registers a home node's reverse tunnel: payload =
// account(65 bytes) ‖ sig[64] over "sailnet-home" ‖ ourPub ‖ poolTag[32].
func (s *Server) handleHomeHello(cell *wire.Cell, in *connWriter) error {
	if len(cell.Payload) < 65+64+32 {
		return errors.New("bad HOME_HELLO")
	}
	acct := string(cell.Payload[:65])
	pub, err := nano.AddressToPubkey(acct)
	if err != nil {
		return err
	}
	h, _ := blake2b.New256(nil)
	h.Write([]byte("sailnet-home"))
	h.Write(s.Key.Public[:])
	h.Write(cell.Payload[65+64 : 65+64+32])
	if !nano.Verify(pub, h.Sum(nil), cell.Payload[65:65+64]) {
		return errors.New("bad home signature")
	}
	pool := strings.ToUpper(hex.EncodeToString(cell.Payload[65+64 : 65+64+32]))
	if !s.Quota.Known(pool) {
		if _, err := s.creditFromLedgerOwner(pool); err != nil {
			return fmt.Errorf("home pool payment not accepted: %v", err)
		}
	}
	if owner := s.Quota.Owner(pool); owner != "" && owner != hex.EncodeToString(pub[:]) {
		return errors.New("home pool was not paid by this account")
	}
	s.homeMu.Lock()
	if s.homes == nil {
		s.homes = map[string]*homeLink{}
	}
	s.homes[acct] = &homeLink{w: in, pool: pool}
	s.homeMu.Unlock()
	log.Printf("home node %s attached (pool %s…)", short(acct), pool[:8])
	return in.write(&wire.Cell{CircID: cell.CircID, Cmd: wire.CmdHomeOK})
}

// forwardCreateToHome relays a CREATE (payload ends with "via:<account>") over
// the home node's tunnel and pumps that circuit's cells both ways. The home
// node's pool with us is metered for every cell relayed.
func (s *Server) forwardCreateToHome(cell *wire.Cell, in *connWriter) {
	acct := string(cell.Payload[132:])
	s.homeMu.Lock()
	hl := s.homes[acct]
	s.homeMu.Unlock()
	if hl == nil {
		in.write(&wire.Cell{CircID: cell.CircID, Cmd: wire.CmdError, Payload: []byte("home node " + short(acct) + " is not attached here")})
		return
	}
	if s.Quota.Remaining(hl.pool) <= 0 {
		in.write(&wire.Cell{CircID: cell.CircID, Cmd: wire.CmdError, Payload: []byte("home node's relay pool is empty")})
		return
	}
	id := hl.nextID.Add(1)
	create := append([]byte(nil), cell.Payload[:128]...) // clientPub ‖ tag ‖ payer signature
	if err := hl.w.write(&wire.Cell{CircID: id, Cmd: wire.CmdCreate, Payload: create}); err != nil {
		in.write(&wire.Cell{CircID: cell.CircID, Cmd: wire.CmdError, Payload: []byte("home tunnel write failed")})
		return
	}
	// Register a bridge: cells from `in` with cell.CircID → home with id, and back.
	s.homeMu.Lock()
	if s.bridges == nil {
		s.bridges = map[*connWriter]map[uint32]bridge{}
	}
	if s.bridges[in] == nil {
		s.bridges[in] = map[uint32]bridge{}
	}
	s.bridges[in][cell.CircID] = bridge{to: hl.w, id: id, pool: hl.pool}
	if s.bridges[hl.w] == nil {
		s.bridges[hl.w] = map[uint32]bridge{}
	}
	s.bridges[hl.w][id] = bridge{to: in, id: cell.CircID, pool: hl.pool}
	s.homeMu.Unlock()
}

type bridge struct {
	to   *connWriter
	id   uint32
	pool string
}

// bridged forwards a cell if its (conn, circID) is a home bridge. Returns true if handled.
func (s *Server) bridged(in *connWriter, cell *wire.Cell) bool {
	s.homeMu.Lock()
	b, ok := s.bridges[in][cell.CircID]
	s.homeMu.Unlock()
	if !ok {
		return false
	}
	if s.Quota.Consume(b.pool, wire.CellSize) < 0 {
		return true // pool exhausted: drop
	}
	s.Metrics.BytesRelayed.Add(wire.CellSize)
	b.to.write(&wire.Cell{CircID: b.id, Cmd: cell.Cmd, StreamID: cell.StreamID, Payload: cell.Payload})
	return true
}

// handleCreate: payload = clientPub[32] ‖ tag[32]. The tag is the hash of
// fragment B of a SAIL transfer to this relay (or a pool tag). Reply CREATED =
// hopPub[32] ‖ sig[64].
func (s *Server) handleCreate(cell *wire.Cell, in *connWriter) (*circuit, error) {
	if len(cell.Payload) < 128 {
		return nil, errors.New("bad CREATE")
	}
	var clientPub, tagB [32]byte
	copy(clientPub[:], cell.Payload[:32])
	copy(tagB[:], cell.Payload[32:64])
	sig := cell.Payload[64:128]
	tag := strings.ToUpper(hex.EncodeToString(cell.Payload[32:64]))
	ip, _, _ := net.SplitHostPort(in.c.RemoteAddr().String())
	if !s.Quota.Known(tag) {
		if !s.allowVerify(tag, ip) { // every unverified tag, supplied block or not, is rate-limited
			s.Metrics.RejectedSpam.Add(1)
			return nil, errors.New("too many unverified payment attempts; try again later")
		}
	}
	if !s.Quota.Known(tag) && s.BridgeSecret != ([16]byte{}) && s.BootstrapBytes > 0 && len(cell.Payload) == 128 {
		// A client that holds this bridge's secret and has nothing to pay with
		// yet: grant a small bootstrap quota (rate-limited per IP above) so it
		// can reach the ledger through us and then pay like everyone else.
		s.Quota.Credit(tag, s.BootstrapBytes, "") // small, per-IP rate-limited, not bound to a wallet the client does not have yet
		s.Metrics.Payments.Add(1)
		log.Printf("bridge bootstrap: %d KiB granted", s.BootstrapBytes>>10)
	}
	if !s.Quota.Known(tag) && len(cell.Payload) > 128 {
		// Firewall mode: the client supplies its signed XNO send block. We
		// verify it locally, publish it ourselves and credit from the ledger,
		// so the client needs no RPC of its own.
		if err := s.acceptSuppliedPayment(tag, cell.Payload[128:]); err != nil {
			log.Printf("supplied payment rejected: %v", err)
			if !errors.Is(err, errTransient) {
				s.markBad(tag)
			}
		}
	}
	if !s.Quota.Known(tag) {
		s.verMu.Lock()
		s.verBusy[tag] = true
		s.verMu.Unlock()
		err := s.creditFromLedger(tag)
		s.verMu.Lock()
		delete(s.verBusy, tag)
		s.verMu.Unlock()
		if err != nil {
			if !errors.Is(err, errTransient) {
				s.markBad(tag)
			}
			s.Metrics.PaymentsBad.Add(1)
			_ = ip
			log.Printf("payment rejected: %v", err) // no client address, no tag: nothing in the log links a user
			return nil, fmt.Errorf("payment %s… not accepted: %v (pay SAIL to %s)", tag[:8], err, s.Key.Address)
		}
		s.Metrics.Payments.Add(1)
	}
	// The tag is public on the ledger; only its owner's key may spend it.
	if owner := s.Quota.Owner(tag); owner != "" {
		ob, err := hex.DecodeString(owner)
		var op [32]byte
		if err != nil || len(ob) != 32 {
			return nil, errors.New("tag owner unknown")
		}
		copy(op[:], ob)
		if !VerifyCreate(op, clientPub, tagB, sig) {
			s.Metrics.RejectedSpam.Add(1)
			return nil, errors.New("CREATE not signed by the payer of this tag")
		}
	}
	s.Metrics.Circuits.Add(1)
	if rem := s.Quota.Remaining(tag); rem <= 0 && !s.overdraftOK(tag, rem) {
		return nil, fmt.Errorf("prepaid quota for %s… is exhausted", tag[:8])
	}
	priv, pub, err := wire.GenX25519()
	if err != nil {
		return nil, err
	}
	keys, err := wire.DeriveHopKeys(priv, clientPub, clientPub, pub)
	if err != nil {
		return nil, err
	}
	c := &circuit{id: cell.CircID, tag: tag, keys: keys, in: in, streams: map[uint16]*exitStream{}, resume: wire.ResumeKey(keys)}
	certHash := s.leafHash()
	reply := append(pub[:], SignAck(s.Key, clientPub, pub, certHash)...)
	reply = append(reply, certHash[:]...)
	if err := in.write(&wire.Cell{CircID: cell.CircID, Cmd: wire.CmdCreated, Payload: reply}); err != nil {
		return nil, err
	}
	return c, nil
}

// handleRelay meters a cell that passes through this hop and either
// forwards it (one layer peeled) or, when it is for us, acts on it. The
// low-quota warning goes out 30 s of the current rate ahead, so the client
// tops the circuit up in place instead of losing every stream.
func (s *Server) handleRelay(c *circuit, cell *wire.Cell) {
	s.handleRelayBatch(c, []*wire.Cell{cell})
}

func (s *Server) handleTerminal(c *circuit, cmd byte, sid uint16, data []byte) {
	switch cmd {
	case wire.CmdPing:
		s.reply(c, wire.CmdPong, sid, data)
	case wire.CmdQuota:
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(max64(s.Quota.Remaining(c.tag), 0)))
		s.reply(c, wire.CmdQuota, sid, b[:])
	case wire.CmdTopUp:
		go s.handleTopUp(c, sid, data)
	case wire.CmdExtend:
		c.mu.Lock()
		extended := c.next != nil
		c.mu.Unlock()
		if extended { // one next hop per circuit: no unbounded outbound connections from one client
			s.reply(c, wire.CmdError, sid, []byte("circuit already extended"))
			return
		}
		go s.handleExtend(c, sid, data)
	case wire.CmdBegin:
		if !s.Exit {
			s.reply(c, wire.CmdEnd, sid, []byte("not an exit"))
			return
		}
		go s.handleBegin(c, sid, string(data), false)
	case wire.CmdBegin2:
		if !s.Exit {
			s.reply(c, wire.CmdEnd, sid, []byte("not an exit"))
			return
		}
		go s.handleBegin(c, sid, string(data), true)
	case wire.CmdCredit:
		c.mu.Lock()
		st := c.streams[sid]
		c.mu.Unlock()
		if st != nil && st.flow && len(data) >= 4 {
			st.addCredit(binary.BigEndian.Uint32(data))
		}
	case wire.CmdData:
		c.mu.Lock()
		st := c.streams[sid]
		if st == nil { // optimistic: the client sends data right after BEGIN
			if c.pending == nil {
				c.pending = map[uint16][][]byte{}
			}
			if len(c.pending[sid]) < 64 {
				c.pending[sid] = append(c.pending[sid], append([]byte(nil), data...))
			}
		}
		c.mu.Unlock()
		if st != nil {
			if st.flow {
				if !st.up.push(append([]byte(nil), data...)) { // the client overran its window: protocol violation
					st.Close()
				}
			} else {
				st.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
				st.conn.Write(data)
			}
		}
	case wire.CmdEnd:
		c.mu.Lock()
		if st := c.streams[sid]; st != nil {
			st.Close()
			delete(c.streams, sid)
		}
		c.mu.Unlock()
	}
}

// ringCell is a sent backward cell with its hop-0 sequence number.
type ringCell struct {
	seq  uint64
	cell *wire.Cell
}

const ringCells = 8192 // above the link queue (4096): everything unsent at a drop is still here

// sealBackward seals one reply and records it for retransmission.
func (c *circuit) sealBackward(cmd byte, sid uint16, data []byte) (*wire.Cell, error) {
	c.sealMu.Lock()
	defer c.sealMu.Unlock()
	box, err := wire.SealBackward(c.keys, cmd, sid, data)
	if err != nil {
		return nil, err
	}
	cell := &wire.Cell{CircID: c.id, Cmd: wire.CmdData, Payload: box}
	c.remember(c.keys.SentBwd(), cell)
	return cell, nil
}

// wrapBackward adds our layer to a cell from the next hop and records it.
func (c *circuit) wrapBackward(box []byte) (*wire.Cell, error) {
	c.sealMu.Lock()
	defer c.sealMu.Unlock()
	out, err := wire.WrapBackward(c.keys, box)
	if err != nil {
		return nil, err
	}
	cell := &wire.Cell{CircID: c.id, Cmd: wire.CmdData, Payload: out}
	c.remember(c.keys.SentBwd(), cell)
	return cell, nil
}

func (c *circuit) remember(seq uint64, cell *wire.Cell) {
	if len(c.ring) >= ringCells {
		copy(c.ring, c.ring[1:])
		c.ring = c.ring[:ringCells-1]
	}
	c.ring = append(c.ring, ringCell{seq, cell})
}

// deliver sends cells toward the client. While the client's link is being
// reattached the caller waits (back-pressure on the exit): every sent cell
// is in the ring, and the reattachment retransmits from the client's last
// accepted sequence, so nothing is dropped and nothing is duplicated.
func (c *circuit) deliver(cells ...*wire.Cell) {
	c.mu.Lock()
	in := c.in
	for in == nil && !c.closed {
		if c.parkCond == nil {
			c.parkCond = sync.NewCond(&c.mu)
		}
		c.parkCond.Wait()
		in = c.in
	}
	if in == nil {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	if len(cells) == 1 {
		in.write(cells[0])
	} else {
		in.writeBatch(cells...)
	}
}

// park keeps an extended circuit alive for a minute after its client link
// dropped, so the client can reattach (CmdResume) instead of rebuilding.
func (s *Server) park(c *circuit) {
	c.mu.Lock()
	if c.closed || c.next == nil {
		c.mu.Unlock()
		c.destroy()
		return
	}
	c.in = nil
	c.parkedAt = time.Now()
	key := c.resume
	c.mu.Unlock()
	s.parkMu.Lock()
	if s.parks == nil {
		s.parks = map[[32]byte]*circuit{}
	}
	s.parks[wire.ResumeID(key)] = c
	s.parkMu.Unlock()
	time.AfterFunc(time.Minute, func() {
		s.parkMu.Lock()
		if s.parks[wire.ResumeID(key)] == c {
			delete(s.parks, wire.ResumeID(key))
			s.parkMu.Unlock()
			c.mu.Lock()
			still := c.in == nil
			c.mu.Unlock()
			if still {
				c.destroy()
			}
			return
		}
		s.parkMu.Unlock()
	})
}

// handleResume reattaches a parked circuit to this connection.
func (s *Server) handleResume(cell *wire.Cell, in *connWriter) *circuit {
	if len(cell.Payload) != 80 {
		return nil
	}
	var id [32]byte
	copy(id[:], cell.Payload[:32])
	// The client often reconnects before the dead connection's reader has
	// noticed and parked the circuit: give the park a moment.
	var c *circuit
	for wait := 0; wait < 30 && c == nil; wait++ {
		s.parkMu.Lock()
		c = s.parks[id]
		if c != nil {
			delete(s.parks, id)
		}
		s.parkMu.Unlock()
		if c == nil {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if c == nil {
		return nil
	}
	ts, recv, ok := wire.VerifyResume(c.resume, cell.Payload)
	if !ok || time.Since(time.Unix(ts, 0)).Abs() > 2*time.Minute {
		s.parkMu.Lock()
		s.parks[id] = c // not the owner: keep it parked for the real one
		s.parkMu.Unlock()
		return nil
	}
	// Tell the client what we last accepted, then retransmit what it
	// missed, in order, before anything new is written.
	var ack [8]byte
	binary.BigEndian.PutUint64(ack[:], c.keys.RecvFwd())
	in.write(&wire.Cell{CircID: cell.CircID, Cmd: wire.CmdResumed, Payload: ack[:]})
	c.sealMu.Lock()
	c.mu.Lock()
	c.id = cell.CircID
	var again []*wire.Cell
	for _, r := range c.ring {
		if r.seq > recv {
			r.cell.CircID = cell.CircID
			again = append(again, r.cell)
		}
	}
	if len(again) > 0 {
		in.writeBatch(again...) // before anyone blocked in deliver() may write new cells
	}
	c.in = in
	if c.parkCond != nil {
		c.parkCond.Broadcast()
	}
	c.mu.Unlock()
	c.sealMu.Unlock()
	return c
}

// buffered reports how many bytes a connection has already read into its
// buffer, or 0 when it cannot say.
func buffered(c any) int {
	if b, ok := c.(interface{ Buffered() int }); ok {
		return b.Buffered()
	}
	return 0
}

// readMore appends cells that are already waiting in the buffer (no new
// syscall, no blocking) to batch, up to max cells.
func readMore(r io.Reader, batch []*wire.Cell, max int) []*wire.Cell {
	for len(batch) < max && buffered(r) >= wire.CellSize {
		cell, err := wire.ReadCell(r)
		if err != nil {
			break
		}
		batch = append(batch, cell)
	}
	return batch
}

// handleRelayBatch is handleRelay for several cells of one circuit that
// arrived together: one lock for metering, one write to the next hop.
func (s *Server) handleRelayBatch(c *circuit, cells []*wire.Cell) {
	var fwd []*wire.Cell
	type term struct {
		cmd  byte
		sid  uint16
		data []byte
	}
	var terms []term
	for _, cell := range cells {
		terminal, cmd, sid, data, err := wire.PeelForward(c.keys, cell.Payload)
		if err != nil {
			continue // not for us and not authenticated: drop silently
		}
		if terminal {
			terms = append(terms, term{cmd, sid, data})
		} else {
			fwd = append(fwd, &wire.Cell{CircID: c.nextID, Cmd: wire.CmdData, Payload: data})
		}
	}
	n := int64(len(fwd)+len(terms)) * wire.CellSize
	if n == 0 {
		return
	}
	s.Metrics.BytesRelayed.Add(n)
	rem := s.Quota.Consume(c.tag, n)
	if rem < 0 && !s.overdraftOK(c.tag, rem) {
		s.reply(c, wire.CmdError, 0, []byte("quota exhausted"))
		c.destroy()
		return
	}
	s.lowQuotaCheck(c, rem, n)
	if len(fwd) > 0 {
		c.mu.Lock()
		next := c.next
		c.mu.Unlock()
		if next != nil {
			next.writeBatch(fwd...)
			s.meterPool(c.poolAcct, int64(len(fwd))*wire.CellSize)
		}
	}
	for _, t := range terms {
		s.handleTerminal(c, t.cmd, t.sid, t.data)
	}
}

// lowQuotaCheck warns the client once when the quota is nearly spent (see
// handleRelay for the reasoning).
func (s *Server) lowQuotaCheck(c *circuit, rem, n int64) {
	c.mu.Lock()
	now := time.Now()
	if c.rateAt.IsZero() {
		c.rateAt = now
	}
	c.rateN += n
	if d := now.Sub(c.rateAt); d >= 2*time.Second {
		c.rate = c.rateN * int64(time.Second) / int64(d)
		c.rateN, c.rateAt = 0, now
	}
	warn := !c.lowSent && (rem < s.Quota.Total(c.tag)/4 || rem < c.rate*30)
	if warn {
		c.lowSent = true
	}
	c.mu.Unlock()
	if warn {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(max64(rem, 0)))
		s.reply(c, wire.CmdQuota, wire.QuotaLowStream, b[:])
	}
}

func (s *Server) reply(c *circuit, cmd byte, sid uint16, data []byte) {
	cell, err := c.sealBackward(cmd, sid, data)
	if err != nil {
		return
	}
	c.deliver(cell)
}

// replyChunks splits data into cell-sized pieces and queues them together, so
// the shaper sees one burst rather than a cell at a time.
func (s *Server) replyChunks(c *circuit, sid uint16, data []byte) {
	var cells []*wire.Cell
	for len(data) > 0 {
		n := len(data)
		if n > wire.MaxData {
			n = wire.MaxData
		}
		cell, err := c.sealBackward(wire.CmdData, sid, data[:n])
		if err != nil {
			return
		}
		cells = append(cells, cell)
		data = data[n:]
	}
	c.deliver(cells...)
}

// handleExtend: data = nextRelayAccount (string). We dial it, prepay from our
// pool if needed, CREATE with the client's X25519 pub carried after a NUL.
func (s *Server) handleExtend(c *circuit, sid uint16, data []byte) {
	i := indexByte(data, 0)
	if i < 0 || len(data[i+1:]) != 32 {
		s.reply(c, wire.CmdError, sid, []byte("bad EXTEND"))
		return
	}
	nextAcct := string(data[:i])
	var clientPub [32]byte
	copy(clientPub[:], data[i+1:])
	rel := s.Registry.Get(nextAcct)
	if rel == nil && s.Registry != nil {
		// A relay this registry has not seen yet (it registered minutes ago):
		// re-read the ledger now, at most once a minute, instead of refusing
		// every circuit through a newcomer until the ten-minute refresh.
		s.mu.Lock()
		due := time.Since(s.lastRegRefresh) > time.Minute
		if due {
			s.lastRegRefresh = time.Now()
		}
		s.mu.Unlock()
		if due {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			s.Registry.Refresh(ctx)
			cancel()
			rel = s.Registry.Get(nextAcct)
		}
	}
	if rel == nil {
		s.reply(c, wire.CmdError, sid, []byte("unknown relay "+nextAcct))
		return
	}
	if !s.Registry.Listed(rel.Account) && s.PoolRaw != nil {
		s.mu.Lock()
		due := time.Since(s.lastRegRefresh) > time.Minute
		if due {
			s.lastRegRefresh = time.Now()
		}
		s.mu.Unlock()
		if due {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			s.Registry.Refresh(ctx)
			cancel()
		}
	}
	if !s.Registry.Listed(rel.Account) && s.PoolRaw != nil {
		s.reply(c, wire.CmdError, sid, []byte("next hop is not on the ledger"))
		return
	}
	tag, ready := s.poolTag(rel)
	if !ready {
		go s.ensurePool(rel)
		s.reply(c, wire.CmdError, sid, []byte("pool to "+short(rel.Account)+" is warming up; retry shortly"))
		return
	}
	dialTarget := rel
	if rel.Flags&token.FlagHome != 0 { // home node: its descriptor is its harbour's endpoint
		if hb := s.Registry.Harbour(rel); hb != nil {
			dialTarget = hb
		} else {
			s.reply(c, wire.CmdError, sid, []byte("home node's harbour is unknown"))
			return
		}
	}
	conn, err := DialRelay(dialTarget, 6*time.Second) // a relay that does not answer in 6 s is not worth a longer wait for the user
	if err != nil {
		s.reply(c, wire.CmdError, sid, []byte("dial next: "+err.Error()))
		return
	}
	nw := newConnWriter(conn, true)
	tagBytes, _ := hex.DecodeString(tag)
	var tagArr [32]byte
	copy(tagArr[:], tagBytes)
	create := append(clientPub[:], tagBytes...)
	create = append(create, SignCreate(s.Key, clientPub, tagArr)...)
	if rel.Flags&token.FlagHome != 0 {
		create = append(create, []byte("via:"+rel.Account)...)
	}
	if err := nw.write(&wire.Cell{CircID: 1, Cmd: wire.CmdCreate, Payload: create}); err != nil {
		conn.Close()
		s.reply(c, wire.CmdError, sid, []byte("create next: "+err.Error()))
		return
	}
	conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	ack, err := wire.ReadCell(conn)
	if err != nil || ack.Cmd != wire.CmdCreated {
		conn.Close()
		msg := "next hop refused"
		if err == nil {
			p := strings.ToLower(string(ack.Payload))
			switch {
			case strings.Contains(p, "exhausted") || strings.Contains(p, "not found") || strings.Contains(p, "levy") || strings.Contains(p, "not signed"):
				// Our prepaid pool there is really gone (or was never seen): forget
				// the estimate and top up, unless we just did (a fresh top-up is
				// "not yet confirmed" for a few seconds, which must not buy another).
				if s.recentTopUp(rel) {
					msg = "next hop: pool is warming up; retry shortly"
				} else {
					s.poolExhausted(rel)
					msg = "next hop: pool is warming up; retry shortly"
				}
			case strings.Contains(p, "warming up") || strings.Contains(p, "not yet confirmed") || strings.Contains(p, "try again"):
				msg = "next hop: pool is warming up; retry shortly" // transient at the peer: wait, do not pay
			}
			log.Printf("extend to %s refused: %s", short(rel.Account), p)
		} else {
			log.Printf("extend to %s: no CREATED: %v", short(rel.Account), err)
		}
		s.reply(c, wire.CmdError, sid, []byte(msg))
		return
	}
	conn.SetReadDeadline(time.Time{})
	c.next, c.nextID, c.poolAcct = nw, 1, rel.Account
	s.reply(c, wire.CmdExtended, sid, ack.Payload)
	// Pump replies from the next hop back to the client, adding our layer.
	go func() {
		defer conn.Close()
		for {
			first, err := wire.ReadCell(conn)
			if err != nil {
				return
			}
			batch := readMore(conn, []*wire.Cell{first}, 64)
			s.meterPool(c.poolAcct, int64(len(batch))*wire.CellSize)
			out := make([]*wire.Cell, 0, len(batch))
			for _, cell := range batch {
				if cell.Cmd != wire.CmdData {
					continue
				}
				wrapped, err := c.wrapBackward(cell.Payload)
				if err != nil {
					return
				}
				out = append(out, wrapped)
			}
			if len(out) == 0 {
				continue
			}
			if rem := s.Quota.Consume(c.tag, int64(len(out))*wire.CellSize); rem < 0 && !s.overdraftOK(c.tag, rem) { // downloads are paid for, not just uploads
				c.deliver(&wire.Cell{CircID: c.id, Cmd: wire.CmdError, Payload: []byte("quota exhausted")})
				return
			}
			c.deliver(out...)
		}
	}()
}

var errTransient = errors.New("transient")

// acceptSuppliedPayment validates a client-supplied signed XNO send block (JSON,
// process shape) whose hash is the tag, publishes it, and credits the quota
// from the amount the ledger reports. Nano itself rejects an unaffordable
// send, so a block that the network accepts is a valid payment.
func (s *Server) acceptSuppliedPayment(tag string, raw []byte) error {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return errors.New("bad block json")
	}
	b, err := nano.BlockFromJSON(m)
	if err != nil {
		return err
	}
	if !b.VerifySigned() {
		return errors.New("bad signature")
	}
	h, _ := b.Hash()
	if !strings.EqualFold(hex.EncodeToString(h[:]), tag) {
		return errors.New("block hash is not the tag")
	}
	if b.Link != s.Key.Public {
		return errors.New("send is not addressed to this relay")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := s.Nano.Process(ctx, b, "send"); err != nil && !strings.Contains(strings.ToLower(err.Error()), "old block") {
		return fmt.Errorf("%w: publish: %v", errTransient, err)
	}
	// Confirmation normally lands within a second or two; wait for it here
	// rather than making the client retry (a client that mistakes this for a
	// rejection pays twice).
	err = nil
	for i := 0; i < 5; i++ {
		if err = s.creditFromLedger(tag); err == nil || !strings.Contains(err.Error(), "not yet confirmed") {
			return err
		}
		time.Sleep(2 * time.Second)
	}
	return err
}

// Drain prepares a restart: new connections are turned away and every open
// connection is told to move, so clients rebuild through another entry
// before this process exits. Returns once the connections are gone or after
// the grace period.
func (s *Server) Drain(grace time.Duration) {
	s.connMu.Lock()
	s.draining = true
	var ws []*connWriter
	for w := range s.conns {
		ws = append(ws, w)
	}
	s.connMu.Unlock()
	for _, w := range ws {
		w.write(&wire.Cell{Cmd: wire.CmdError, Payload: []byte("relay restarting; use another entry")})
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		s.connMu.Lock()
		n := len(s.conns)
		s.connMu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// overdraftOK lets a listed relay's pool run a little negative while its
// top-up is in flight: a pool refill needs a Nano block and proof-of-work,
// and a circuit must not die waiting for it. Clients get no overdraft; they
// top up in place before the boundary (CmdTopUp).
func (s *Server) overdraftOK(tag string, rem int64) bool {
	const overdraft = 128 << 20 // about 15 s at 8 MB/s: the refill starts 60 s ahead, this covers a slow block
	if rem < -overdraft || s.Registry == nil {
		return false
	}
	owner := s.Quota.Owner(tag)
	if owner == "" {
		return false
	}
	for _, r := range s.Registry.All() {
		if hex.EncodeToString(r.Pub[:]) == owner {
			return true
		}
	}
	return false
}

// handleTopUp adds a client's signed send block to its circuit's quota.
func (s *Server) handleTopUp(c *circuit, sid uint16, raw []byte) {
	fail := func(msg string) { s.reply(c, wire.CmdError, sid, []byte("top-up: "+msg)) }
	if s.Nano == nil {
		fail("no ledger")
		return
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		fail("bad block json")
		return
	}
	b, err := nano.BlockFromJSON(m)
	if err != nil || !b.VerifySigned() {
		fail("bad block")
		return
	}
	if b.Link != s.Key.Public {
		fail("send is not addressed to this relay")
		return
	}
	if owner := s.Quota.Owner(c.tag); owner != "" {
		pub, _ := nano.AddressToPubkey(b.Account)
		if hex.EncodeToString(pub[:]) != owner {
			fail("top-up must come from the wallet that paid the anchor")
			return
		}
	}
	h, _ := b.Hash()
	hash := strings.ToUpper(hex.EncodeToString(h[:]))
	if s.Quota.Known(hash) {
		fail("block already used")
		return
	}
	// The block is well-formed, signed by the anchor's owner and addressed
	// to us: the circuit keeps flowing on a provisional credit while the
	// ledger confirms it. A block that never confirms takes the credit back
	// and the circuit ends at the boundary as before.
	// Sized to outlast the confirmation at the circuit's current rate.
	c.mu.Lock()
	provisional := c.rate * 60
	c.mu.Unlock()
	if provisional < 48<<20 {
		provisional = 48 << 20
	}
	if provisional > 1<<30 {
		provisional = 1 << 30
	}
	s.Quota.Add(c.tag, provisional)
	confirmed := false
	defer func() {
		if !confirmed {
			s.Quota.Add(c.tag, -provisional)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := s.Nano.Process(ctx, b, "send"); err != nil && !strings.Contains(strings.ToLower(err.Error()), "old block") {
		// The client may have published it already through its own ledger
		// path; what counts is whether the ledger has it.
		if err2 := s.creditFromLedger(hash); err2 != nil {
			fail("publish: " + stripErr(err))
			return
		}
	}
	for i := 0; i < 8; i++ {
		if err = s.creditFromLedger(hash); err == nil || !strings.Contains(err.Error(), "not yet confirmed") {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		fail(stripErr(err))
		return
	}
	bytes := s.Quota.Remaining(hash)
	s.Quota.Consume(hash, bytes) // the block's value moves onto the circuit's tag; it cannot open a second circuit
	rem, ok := s.Quota.Add(c.tag, bytes-provisional)
	if !ok {
		fail("unknown circuit tag")
		return
	}
	confirmed = true
	c.mu.Lock()
	c.lowSent = false
	c.mu.Unlock()
	s.Metrics.Payments.Add(1)
	log.Printf("top-up accepted: +%d MiB on a circuit", bytes>>20)
	var out [8]byte
	binary.BigEndian.PutUint64(out[:], uint64(max64(rem, 0)))
	s.reply(c, wire.CmdQuota, sid, out[:])
}

func stripErr(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "http"); i >= 0 {
		msg = msg[:i]
	}
	return strings.TrimSpace(msg)
}

// creditFromLedger verifies on the Nano ledger that tag is a confirmed send to
// this relay and credits its quota: one blocks_info call.
func (s *Server) creditFromLedger(tag string) error {
	_, err := s.creditFromLedgerOwner(tag)
	return err
}

// creditFromLedgerOwner credits a confirmed send to this relay and returns
// the hex public key of the account that signed it (the tag's owner).
func (s *Server) creditFromLedgerOwner(tag string) (string, error) {
	owner, err := s.creditFromLedgerImpl(tag)
	return owner, err
}

func (s *Server) creditFromLedgerImpl(tag string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	infos, err := s.Nano.BlocksInfo(ctx, []string{strings.ToUpper(tag)})
	if err != nil {
		return "", fmt.Errorf("%w: %v", errTransient, err)
	}
	bi, ok := infos[strings.ToUpper(tag)]
	if !ok {
		return "", errors.New("payment block not found on the ledger")
	}
	if bi.Subtype != "send" {
		return "", errors.New("payment block is not a send")
	}
	to := bi.Contents.LinkAsAccount
	if to == "" {
		if pk, err := hex.DecodeString(bi.Contents.Link); err == nil && len(pk) == 32 {
			var p [32]byte
			copy(p[:], pk)
			to = nano.PubkeyToAddress(p)
		}
	}
	if to != s.Key.Address {
		return "", fmt.Errorf("payment went to %s, not this relay", short(to))
	}
	if _, isLevy := rewardEpoch(bi.Contents.Representative); isLevy {
		return "", errors.New("levy payouts do not buy service")
	}
	if bi.Confirmed != "true" { // an RPC that omits the field does not get to vouch for a payment
		return "", fmt.Errorf("%w: payment not yet confirmed", errTransient)
	}
	amt, ok := new(big.Int).SetString(bi.Amount, 10)
	if !ok || amt.Sign() <= 0 {
		return "", errors.New("bad amount")
	}
	n := BytesFor(amt, s.Quota.MinRate)
	if n <= 0 {
		return "", errors.New("amount below the minimum")
	}
	ownerPub, err := nano.AddressToPubkey(bi.BlockAccount)
	if err != nil {
		return "", errors.New("payment block has no account")
	}
	owner := hex.EncodeToString(ownerPub[:])
	s.Quota.Credit(tag, n, owner)
	log.Printf("payment accepted: %s XNO → %d bytes", formatXNO(amt), n)
	return owner, nil
}

func formatXNO(raw *big.Int) string {
	f := new(big.Float).SetInt(raw)
	f.Quo(f, new(big.Float).SetInt(rawPerXNO))
	return f.Text('f', 8)
}

var rawPerXNO = func() *big.Int { b, _ := new(big.Int).SetString("1000000000000000000000000000000", 10); return b }()

// poolTag returns the current pool tag for a downstream relay and whether it
// is usable now (static tag in test mode, or an on-chain transfer already made).
func (s *Server) poolTag(rel *RelayInfo) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pools == nil {
		s.pools = map[string]*pool{}
	}
	p := s.pools[rel.Account]
	if p == nil {
		p = &pool{tag: PoolTag(s.Key.Address, rel.Account)}
		s.pools[rel.Account] = p
		if s.PoolRaw == nil {
			return p.tag, true
		}
		return "", false
	}
	if s.PoolRaw == nil || p.credited-p.used >= 1<<20 {
		return p.tag, true
	}
	return "", false
}

type poolFile struct {
	Tag      string    `json:"tag"`
	LastTop  time.Time `json:"last_top"`
	Credited int64     `json:"credited"`
	Used     int64     `json:"used"`
}

// savePoolsLocked writes the pool table to PoolsFile (caller holds s.mu).
func (s *Server) savePoolsLocked() {
	if s.PoolsFile == "" {
		return
	}
	m := map[string]poolFile{}
	for acct, p := range s.pools {
		m[acct] = poolFile{p.tag, p.lastTop, p.credited, p.used}
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(s.PoolsFile, data, 0o600)
}

// LoadPools restores the pool table written by an earlier run.
func (s *Server) LoadPools() {
	if s.PoolsFile == "" {
		return
	}
	data, err := os.ReadFile(s.PoolsFile)
	if err != nil {
		return
	}
	var m map[string]poolFile
	if json.Unmarshal(data, &m) != nil {
		return
	}
	s.mu.Lock()
	if s.pools == nil {
		s.pools = map[string]*pool{}
	}
	for acct, p := range m {
		s.pools[acct] = &pool{tag: p.Tag, lastTop: p.LastTop, credited: p.Credited, used: p.Used}
	}
	s.mu.Unlock()
	log.Printf("%d prepaid pools restored", len(m))
}

// meterPool records bytes sent through a downstream pool so top-ups happen
// before the peer runs dry; the table is saved every 8 MiB.
func (s *Server) meterPool(acct string, n int64) {
	if acct == "" {
		return
	}
	s.mu.Lock()
	var refill *RelayInfo
	if p := s.pools[acct]; p != nil {
		before := p.used
		p.used += n
		if before/(8<<20) != p.used/(8<<20) {
			s.savePoolsLocked()
		}
		now := time.Now()
		if p.rateAt.IsZero() {
			p.rateAt = now
		}
		p.rateN += n
		if d := now.Sub(p.rateAt); d >= 2*time.Second {
			p.rate = p.rateN * int64(time.Second) / int64(d)
			p.rateN, p.rateAt = 0, now
		}
		// Refill before it runs dry, off the data path, and early enough in
		// time: a refill needs a Nano block, so it starts 60 s of the current
		// rate ahead (and never later than a quarter of the pool).
		if s.PoolRaw != nil && !p.topping && s.Registry != nil {
			if rel := s.Registry.Get(acct); rel != nil {
				size := BytesFor(s.PoolRaw, token.RateToRaw(rel.MinRate))
				if size >= 8<<20 && (p.credited-p.used < size/4 || p.credited-p.used < p.rate*60) {
					p.topping = true
					refill = rel
				}
			}
		}
	}
	s.mu.Unlock()
	if refill != nil {
		go func() {
			if _, err := s.topUpPool(refill); err != nil {
				log.Printf("pool top-up to %s: %v", short(refill.Account), err)
			}
		}()
	}
}

// topUpPool sends one PoolRaw to the peer and credits the pool.
func (s *Server) topUpPool(rel *RelayInfo) (string, error) {
	s.mu.Lock()
	p := s.pools[rel.Account]
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		p.topping = false
		s.mu.Unlock()
	}()
	acct := &nano.Account{Key: s.Key, Client: s.Nano}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute) // off the request path
	defer cancel()
	// Size the refill to five minutes at the pool's recent rate, between one
	// and twenty base pools, so a fast circuit is not refilled every few
	// seconds (each refill is a Nano block).
	s.mu.Lock()
	rate := p.rate
	s.mu.Unlock()
	rateRaw := token.RateToRaw(rel.MinRate)
	amount := new(big.Int).Set(s.PoolRaw)
	if rate > 0 && rateRaw.Sign() > 0 {
		want := new(big.Int).Mul(big.NewInt((rate*300+(1<<20)-1)/(1<<20)), rateRaw)
		if want.Cmp(amount) > 0 {
			amount = want
		}
		if max := new(big.Int).Mul(s.PoolRaw, big.NewInt(20)); amount.Cmp(max) > 0 {
			amount = max
		}
	}
	// A relay pays pools from what it holds: a new relay with only the
	// faucet's registration amount opens a small pool, earns from the hop
	// before it, and grows the pool on the next refill. Nothing is refused
	// for being poor; only an empty wallet waits.
	acct.ReceiveAll(ctx)
	if info, ok, err := s.Nano.AccountInfo(ctx, s.Key.Address); err == nil && ok {
		if bal, ok := new(big.Int).SetString(info.Balance, 10); ok {
			// Down to dust, deliberately: the float exists to prepay the next
			// hop, and a peer that is carrying our traffic must never be left
			// unpaid so that our own operator can be paid first. What the node
			// earns above the float is swept afterwards (RULES.md, rule 1: no
			// change may make another operator earn less for the same work).
			reserve := new(big.Int).Exp(big.NewInt(10), big.NewInt(25), nil) // 0.00001 XNO for the next block itself
			avail := new(big.Int).Sub(bal, reserve)
			if avail.Cmp(amount) < 0 {
				amount = avail
			}
			if amount.Sign() <= 0 {
				return p.tag, fmt.Errorf("no XNO for a pool yet (balance %s XNO); earnings will fund it", formatXNO(bal))
			}
		}
	}
	h, err := acct.Send(ctx, rel.Account, amount, nil)
	if err != nil {
		return p.tag, err
	}
	s.mu.Lock()
	p.tag = strings.ToUpper(h)
	p.credited += BytesFor(amount, rateRaw)
	s.savePoolsLocked()
	s.mu.Unlock()
	log.Printf("pool top-up to %s: %s XNO (%s)", short(rel.Account), formatXNO(amount), h[:8])
	time.Sleep(2 * time.Second)
	return p.tag, nil
}

// recentTopUp reports whether we prepaid this peer within the last minute.
func (s *Server) recentTopUp(rel *RelayInfo) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pools[rel.Account]
	return p != nil && time.Since(p.lastTop) < time.Minute
}

// poolExhausted forgets our estimate for a peer's pool (the peer just told
// us it is empty) and starts a top-up right away.
func (s *Server) poolExhausted(rel *RelayInfo) {
	s.mu.Lock()
	if p := s.pools[rel.Account]; p != nil {
		p.credited = p.used
		p.lastTop = time.Time{}
		s.savePoolsLocked()
	}
	s.mu.Unlock()
	go s.ensurePool(rel)
}

// WarmPools tops up pools for every known peer in the background, one at a time
// (a relay's chain is sequential), so extensions never wait for on-chain work.
func (s *Server) WarmPools() {
	if s.PoolRaw == nil {
		return
	}
	for _, rel := range s.Registry.All() {
		if rel.Account == s.Key.Address || s.Registry.Unpaid(rel.Account) || !s.Registry.Listed(rel.Account) {
			continue // never prepay an account that is not on the ledger (gossip can be forged cheaply)
		}
		// Prepay only relays that answer. A registration costs one raw to
		// publish, so anyone can list a hundred addresses they do not own and
		// collect a pool from every honest relay; and a pool paid to a relay
		// that never carries a cell is money given away. A recent gossip
		// record or an open TCP connection is proof enough, and both are
		// things an old relay produces too.
		if time.Since(s.Registry.LastSeen(rel.Account)) > 3*time.Hour && !reachable(rel, 4*time.Second) {
			continue
		}
		if _, ready := s.poolTag(rel); !ready {
			if _, err := s.ensurePool(rel); err != nil {
				log.Printf("pool to %s: %v", short(rel.Account), err)
			}
		}
	}
}

// reachable reports whether a TCP connection to the relay opens.
func reachable(rel *RelayInfo, timeout time.Duration) bool {
	c, err := (&net.Dialer{Timeout: timeout, Control: DialControl}).Dial("tcp", rel.Desc.Addr())
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// ensurePool makes sure we have prepaid quota at the downstream relay by
// transferring SAIL to it in bulk; the transfer hash is the pool tag.
func (s *Server) ensurePool(rel *RelayInfo) (string, error) {
	s.mu.Lock()
	if s.pools == nil {
		s.pools = map[string]*pool{}
	}
	p := s.pools[rel.Account]
	if p == nil {
		p = &pool{tag: PoolTag(s.Key.Address, rel.Account)}
		s.pools[rel.Account] = p
	}
	need := s.PoolRaw != nil && p.credited-p.used < 4<<20 && !p.topping && time.Since(p.lastTop) > 5*time.Minute
	if need && BytesFor(s.PoolRaw, token.RateToRaw(rel.MinRate)) < 8<<20 {
		need = false // this peer's price makes our pool size pointless: do not top up 288 times a day
	}
	if need {
		p.lastTop = time.Now()
		p.topping = true
	}
	s.mu.Unlock()
	if !need {
		return p.tag, nil
	}
	return s.topUpPool(rel)
}

// exitAllowed is the default exit policy: no mail relaying, no Windows file
// sharing, no UPnP; everything else on public addresses is fine.
func exitAllowed(target string) bool {
	t := strings.TrimPrefix(target, UDPPrefix)
	_, port, err := net.SplitHostPort(t)
	if err != nil {
		return false
	}
	switch port {
	case "25", "137", "138", "139", "445", "1900", "3389", "23":
		return false
	}
	return true
}

func (s *Server) handleBegin(c *circuit, sid uint16, target string, flow bool) {
	var conn net.Conn
	var err error
	if !exitAllowed(target) {
		s.reply(c, wire.CmdEnd, sid, []byte("port not allowed by exit policy"))
		return
	}
	c.mu.Lock()
	n := len(c.streams)
	c.mu.Unlock()
	if n >= 256 {
		s.reply(c, wire.CmdEnd, sid, []byte("too many streams on this circuit"))
		return
	}
	if strings.HasPrefix(target, UDPPrefix) {
		conn, err = dialUDP(target[len(UDPPrefix):], 15*time.Second)
	} else if s.AllowPrivate {
		conn, err = (&net.Dialer{Timeout: 15 * time.Second}).Dial("tcp", target)
	} else {
		conn, err = dialTCPPublic(target, 15*time.Second)
	}
	if err != nil {
		s.reply(c, wire.CmdEnd, sid, []byte(err.Error()))
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		conn.Close()
		return
	}
	st := newExitStream(conn, flow)
	c.streams[sid] = st
	early := c.pending[sid]
	delete(c.pending, sid)
	c.mu.Unlock()
	for _, d := range early { // flush data that arrived before the connect finished
		conn.Write(d)
	}
	if flow {
		go s.pumpUp(c, sid, st, len(early))
	}
	s.reply(c, wire.CmdConnected, sid, nil) // never reveal the exit's own address or ports
	s.Metrics.Streams.Add(1)
	// Read far more than one cell at a time and hand the whole chunk to the
	// writer as one batch: the records that leave the tunnel then carry many
	// cells each, instead of one cell per record.
	buf := make([]byte, 32*wire.MaxData)
	for {
		room := len(buf)
		if flow { // send only what the client has granted
			cells := st.takeCredit(32)
			if cells == 0 {
				break
			}
			room = cells * wire.MaxData
		}
		n, err := conn.Read(buf[:room])
		if n > 0 && flow { // return unused window (a short read used fewer cells)
			usedCells := (n + wire.MaxData - 1) / wire.MaxData
			if back := room/wire.MaxData - usedCells; back > 0 {
				st.addCredit(uint32(back))
			}
		}
		if n > 0 {
			s.Metrics.BytesExit.Add(int64(n))
			s.replyChunks(c, sid, buf[:n])
			if rem := s.Quota.Consume(c.tag, int64(n)); rem < 0 {
				s.reply(c, wire.CmdError, 0, []byte("quota exhausted"))
				break
			}
		}
		if err != nil {
			if err != io.EOF {
				_ = err
			}
			break
		}
	}
	s.reply(c, wire.CmdEnd, sid, nil)
	c.mu.Lock()
	delete(c.streams, sid)
	c.mu.Unlock()
	st.Close()
}

func (c *circuit) destroy() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if c.parkCond != nil {
		c.parkCond.Broadcast()
	}
	for _, st := range c.streams {
		st.Close()
	}
	if c.next != nil {
		c.next.write(&wire.Cell{CircID: c.nextID, Cmd: wire.CmdDestroy})
		c.next.drain(200 * time.Millisecond)
		c.next.stop()
		c.next.c.Close()
	}
}

// PoolTag is the static pool tag used when no SAIL top-ups are configured
// (tests / bootstrap): blake2b-256(ourAccount ‖ theirAccount) as 64 hex.
func PoolTag(from, to string) string {
	h, _ := blake2b.New256(nil)
	h.Write([]byte(from + to))
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil)))
}

func indexByte(b []byte, x byte) int {
	for i, v := range b {
		if v == x {
			return i
		}
	}
	return -1
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// DialRelay opens a TLS + token-gated tunnel to a relay and returns the raw conn.
// The ClientHello imitates a current Chrome (uTLS), the SNI is the relay's
// hostname (never a bare IP), and the certificate is pinned by the 6-byte
// fingerprint from the descriptor. Cell writes are then fragmented into
// random-sized TLS records so the wire shows no fixed 1024-byte rhythm.
// DialControl, if set, runs on every outbound socket before it connects. On
// Android the VPN app uses it to mark its own sockets so they bypass the tunnel.
var DialControl func(network, address string, c syscall.RawConn) error

func DialRelay(rel *RelayInfo, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout, Control: DialControl}
	raw, err := d.Dial("tcp", rel.Desc.Addr())
	if tc, ok := raw.(*net.TCPConn); ok && err == nil {
		// Socket buffers sized for a long path: 4 MB carries 10 MB/s at 400 ms.
		tc.SetReadBuffer(4 << 20)
		tc.SetWriteBuffer(4 << 20)
	}
	if err != nil {
		return nil, err
	}
	// When trace capture is on (the sailtrace tool only) the raw TCP
	// connection is tapped under TLS, so the recorded records are exactly
	// what a censor on the path would see. Nil in production.
	return DialRelayOver(rel, timeout, shape.WrapDial(raw, rel.Desc.Addr()))
}

// DialRelayOver runs the relay handshake over an already-open transport
// (a TCP connection, or a stream through a circuit when the caller's own
// address must stay hidden from the relay). Deadlines on transports that do
// not support them are enforced by closing the transport on timeout.
func DialRelayOver(rel *RelayInfo, timeout time.Duration, raw net.Conn) (net.Conn, error) {
	guard := time.AfterFunc(timeout, func() { raw.Close() })
	defer guard.Stop()
	sni := rel.Host
	if sni == "" {
		sni = rel.Desc.IP.String()
	}
	var leaf [32]byte
	cfg := &utls.Config{ServerName: sni}
	if rel.Desc.CertFP == ([6]byte{}) {
		// No pin: a real certificate for a real name, verified through the
		// system roots like any website; the ack binds the leaf anyway.
		cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) > 0 {
				leaf = sha256.Sum256(rawCerts[0])
			}
			return nil
		}
	} else {
		cfg.InsecureSkipVerify = true // pinned below via the on-ledger fingerprint, and bound into the ack
		cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("no cert")
			}
			if CertFP6(rawCerts[0]) != rel.Desc.CertFP {
				return errors.New("tls fingerprint does not match ledger descriptor")
			}
			leaf = sha256.Sum256(rawCerts[0])
			return nil
		}
	}
	uc := utls.UClient(raw, cfg, utls.HelloChrome_Auto)
	uc.SetDeadline(time.Now().Add(timeout))
	if err := uc.Handshake(); err != nil {
		raw.Close()
		return nil, err
	}
	var tc net.Conn = uc
	path, _ := TunnelPath(rel.Pub, rel.Secret, time.Now())
	var wsKey [16]byte
	rand.Read(wsKey[:])
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0 Safari/537.36\r\nAccept: */*\r\nAccept-Language: en-US,en;q=0.9\r\nOrigin: https://%s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", path, sni, sni, base64.StdEncoding.EncodeToString(wsKey[:]))
	if _, err := tc.Write([]byte(req)); err != nil {
		tc.Close()
		return nil, err
	}
	br := bufio.NewReader(tc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		tc.Close()
		return nil, err
	}
	if resp.StatusCode != 101 {
		tc.Close()
		return nil, fmt.Errorf("relay answered %s (not a sailnet relay, or wrong token)", resp.Status)
	}
	tc.SetDeadline(time.Time{})
	ws := wire.NewWSConn(tc, br, true) // client side masks its frames, as the RFC requires
	ws.Ping()                          // small first record after the handshake
	return &bufConn{Conn: ws, r: bufio.NewReaderSize(ws, 64<<10), leaf: leaf}, nil
}

// LeafHash returns the SHA-256 of the relay's TLS leaf certificate seen on a
// connection opened by DialRelay (zero if unknown).
func LeafHash(c net.Conn) [32]byte {
	if bc, ok := c.(*bufConn); ok {
		return bc.leaf
	}
	return [32]byte{}
}

// bufConn keeps bytes already buffered by the HTTP reader and fragments
// writes into random-sized pieces (each becomes its own TLS record).
type bufConn struct {
	net.Conn
	r    *bufio.Reader
	leaf [32]byte
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// Buffered lets the cell loops batch what is already in the reader.
func (b *bufConn) Buffered() int { return b.r.Buffered() }

// Write passes straight through: record boundaries are decided by the
// tunnel writer's shaper (relay/writer.go), not here. The random 1–3 way
// split that used to live here chopped every record the shaper produced;
// The measurement is reproducible with `sailtrace`.
func (b *bufConn) Write(p []byte) (int, error) { return b.Conn.Write(p) }

// wsAccept computes the RFC 6455 Sec-WebSocket-Accept value.
func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}

// leafHash is the SHA-256 of the certificate clients see right now.
func (s *Server) leafHash() [32]byte {
	if s.GetCertificate != nil {
		if c, err := s.GetCertificate(&tls.ClientHelloInfo{ServerName: s.Host}); err == nil && c != nil && len(c.Certificate) > 0 {
			return sha256.Sum256(c.Certificate[0])
		}
		return [32]byte{}
	}
	if len(s.TLS.Certificate) == 0 {
		return [32]byte{}
	}
	return sha256.Sum256(s.TLS.Certificate[0])
}

// watcher returns the relay's confirmation feed, starting it on first use.
func (s *Server) watcher() *Watcher {
	if w := s.watch.Load(); w != nil {
		return w
	}
	w := NewWatcher(WSUpstream, notifyCell)
	if !s.watch.CompareAndSwap(nil, w) {
		return s.watch.Load()
	}
	return w
}
