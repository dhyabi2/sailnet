// Package wire defines Sailnet cells and per-hop cryptography.
//
// A circuit is a chain of hops. Every cell on the wire is a fixed 1024-byte
// unit: [circID:4][cmd:1][streamID:2][len:2][payload:1015]. Relay-bound
// cells (EXTEND, BEGIN, DATA, END, PING, QUOTA) are onion-encrypted: the
// client applies one ChaCha20-Poly1305 layer per hop, innermost first, and
// each hop peels one layer. Replies are encrypted by each hop in turn and
// peeled by the client. Because layers use AEAD, a hop that cannot
// authenticate a cell drops it, and a client can tell which hop's key
// produced a valid reply.
package wire

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"sync"

	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
)

const (
	CellSize    = 1024
	HeaderSize  = 9
	PayloadSize = CellSize - HeaderSize // 1015
	TagSize     = 16
	NonceSize   = 12
	// MaxData is the largest data payload that fits under 3 onion layers in
	// either direction: 3 AEAD layers (nonce+tag each), 2 relay markers, the
	// terminal marker, cmd and stream id, with a little slack.
	MaxData = PayloadSize - 3*(TagSize+NonceSize) - 8
)

// Commands.
const (
	CmdCreate    byte = 1 // client → first hop: X25519 pub (plaintext, over TLS)
	CmdCreated   byte = 2 // first hop → client: X25519 pub + signed ack
	CmdExtend    byte = 3 // via hop N: connect to hop N+1 and CREATE
	CmdExtended  byte = 4 // reply: CREATED from hop N+1, relayed back
	CmdBegin     byte = 5 // open stream: "host:port"
	CmdConnected byte = 6
	CmdData      byte = 7
	CmdEnd       byte = 8
	CmdPing      byte = 9
	CmdPong      byte = 10
	CmdQuota     byte = 11 // hop → client: remaining prepaid bytes (uint64)
	CmdError     byte = 12 // hop → client: reason string
	CmdDestroy   byte = 13
)

// Cell is a parsed cell.
type Cell struct {
	CircID   uint32
	Cmd      byte
	StreamID uint16
	Payload  []byte
}

// Marshal writes a fixed-size cell.
func (c *Cell) Marshal() []byte {
	buf := make([]byte, CellSize)
	binary.BigEndian.PutUint32(buf[0:4], c.CircID)
	buf[4] = c.Cmd
	binary.BigEndian.PutUint16(buf[5:7], c.StreamID)
	n := len(c.Payload)
	if n > PayloadSize {
		n = PayloadSize
	}
	binary.BigEndian.PutUint16(buf[7:9], uint16(n))
	copy(buf[9:], c.Payload[:n])
	if n < PayloadSize { // random padding so every cell looks alike
		rand.Read(buf[9+n:])
	}
	return buf
}

// ReadCell reads exactly one cell.
func ReadCell(r io.Reader) (*Cell, error) {
	buf := make([]byte, CellSize)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		if buf[4] == CmdPadding {
			continue // cover traffic never reaches the protocol
		}
		n := int(binary.BigEndian.Uint16(buf[7:9]))
		if n > PayloadSize {
			return nil, errors.New("bad cell length")
		}
		return &Cell{CircID: binary.BigEndian.Uint32(buf[0:4]), Cmd: buf[4], StreamID: binary.BigEndian.Uint16(buf[5:7]), Payload: append([]byte(nil), buf[9:9+n]...)}, nil
	}
}

// HopKeys is the symmetric key pair for one hop, derived from X25519.
type HopKeys struct {
	Forward  []byte // client → hop
	Backward []byte // hop → client

	// Replay protection: every layer's nonce is a per-direction sequence
	// number, and the receiver keeps a 64-cell anti-replay window (as IPsec
	// does), so a hop cannot replay a cell to duplicate stream data, burn
	// quota, or tag traffic for a colluding hop.
	mu             sync.Mutex
	fwdSeq, bwdSeq uint64
	fwdWin, bwdWin replayWindow
}

// replayWindow accepts each sequence number at most once, tolerating
// reordering within 64 positions.
type replayWindow struct {
	high uint64
	bits uint64
}

func (w *replayWindow) accept(seq uint64) bool {
	if seq == 0 {
		return false
	}
	if seq > w.high {
		shift := seq - w.high
		if shift >= 64 {
			w.bits = 0
		} else {
			w.bits <<= shift
		}
		w.bits |= 1
		w.high = seq
		return true
	}
	diff := w.high - seq
	if diff >= 64 || w.bits&(1<<diff) != 0 {
		return false
	}
	w.bits |= 1 << diff
	return true
}

func (k *HopKeys) nextFwd() uint64 { k.mu.Lock(); defer k.mu.Unlock(); k.fwdSeq++; return k.fwdSeq }
func (k *HopKeys) nextBwd() uint64 { k.mu.Lock(); defer k.mu.Unlock(); k.bwdSeq++; return k.bwdSeq }
func (k *HopKeys) acceptFwd(seq uint64) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.fwdWin.accept(seq)
}
func (k *HopKeys) acceptBwd(seq uint64) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.bwdWin.accept(seq)
}

// ErrReplay is returned for a cell whose sequence number was already seen.
var ErrReplay = errors.New("replayed cell")

// sealSeq encrypts with the sequence number as the nonce (8 bytes big-endian
// followed by 4 zero bytes). Wire format is unchanged: nonce ‖ ciphertext.
func sealSeq(key []byte, seq uint64, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, NonceSize)
	binary.BigEndian.PutUint64(nonce[:8], seq)
	return append(nonce, aead.Seal(nil, nonce, plaintext, nil)...), nil
}

// openSeq decrypts and returns the sequence number carried in the nonce.
func openSeq(key, box []byte) (uint64, []byte, error) {
	if len(box) < NonceSize+TagSize {
		return 0, nil, errors.New("short box")
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return 0, nil, err
	}
	pt, err := aead.Open(nil, box[:NonceSize], box[NonceSize:], nil)
	if err != nil {
		return 0, nil, err
	}
	return binary.BigEndian.Uint64(box[:8]), pt, nil
}

// GenX25519 returns (private, public).
func GenX25519() (priv, pub [32]byte, err error) {
	if _, err = rand.Read(priv[:]); err != nil {
		return
	}
	p, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return
	}
	copy(pub[:], p)
	return
}

// DeriveHopKeys computes shared keys; both sides get the same result.
func DeriveHopKeys(priv, peerPub [32]byte, clientPub, hopPub [32]byte) (*HopKeys, error) {
	shared, err := curve25519.X25519(priv[:], peerPub[:])
	if err != nil {
		return nil, err
	}
	kdf := func(label string) []byte {
		h, _ := blake2b.New256(nil)
		h.Write([]byte("sailnet-v1-" + label))
		h.Write(shared)
		h.Write(clientPub[:])
		h.Write(hopPub[:])
		return h.Sum(nil)
	}
	return &HopKeys{Forward: kdf("fwd"), Backward: kdf("bwd")}, nil
}

// Seal encrypts payload with key: nonce(12) || ciphertext || tag.
func Seal(key, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, NonceSize)
	rand.Read(nonce)
	return append(nonce, aead.Seal(nil, nonce, plaintext, nil)...), nil
}

// Open decrypts a Seal output.
func Open(key, box []byte) ([]byte, error) {
	if len(box) < NonceSize+TagSize {
		return nil, errors.New("short box")
	}
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, box[:NonceSize], box[NonceSize:], nil)
}

// OnionSeal wraps payload for a route: the last hop's layer is applied first.
// The innermost layer carries [cmd:1][streamID:2][data]; outer layers carry the
// previous box. Each hop opens with its forward key: if the result parses as a
// terminal cell (see PeelForward) the cell is for this hop, otherwise forward.
func OnionSeal(hops []*HopKeys, cmd byte, streamID uint16, data []byte) ([]byte, error) {
	inner := make([]byte, 3+len(data))
	inner[0] = cmd
	binary.BigEndian.PutUint16(inner[1:3], streamID)
	copy(inner[3:], data)
	cur := append([]byte{0}, inner...) // 0 = terminal marker
	var err error
	for i := len(hops) - 1; i >= 0; i-- {
		cur, err = sealSeq(hops[i].Forward, hops[i].nextFwd(), cur)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			cur = append([]byte{1}, cur...) // 1 = relay onward
		}
	}
	return cur, nil
}

// PeelForward removes one layer. terminal=true means the cell is for this hop.
func PeelForward(k *HopKeys, box []byte) (terminal bool, cmd byte, streamID uint16, data []byte, err error) {
	seq, pt, err := openSeq(k.Forward, box)
	if err != nil {
		return
	}
	if !k.acceptFwd(seq) {
		err = ErrReplay
		return
	}
	if len(pt) < 1 {
		err = errors.New("empty layer")
		return
	}
	if pt[0] == 0 {
		if len(pt) < 4 {
			err = errors.New("short terminal")
			return
		}
		return true, pt[1], binary.BigEndian.Uint16(pt[2:4]), pt[4:], nil
	}
	return false, 0, 0, pt[1:], nil // onward box for next hop
}

// SealBackward is used by a hop to encrypt a reply toward the client.
func SealBackward(k *HopKeys, cmd byte, streamID uint16, data []byte) ([]byte, error) {
	inner := make([]byte, 3+len(data))
	inner[0] = cmd
	binary.BigEndian.PutUint16(inner[1:3], streamID)
	copy(inner[3:], data)
	return sealSeq(k.Backward, k.nextBwd(), inner)
}

// WrapBackward adds this hop's backward layer over a reply arriving from the next hop.
func WrapBackward(k *HopKeys, box []byte) ([]byte, error) {
	return sealSeq(k.Backward, k.nextBwd(), append([]byte{1}, box...))
}

// PeelBackward is used by the client: tries hops in order and returns which
// hop produced the reply (proof-of-route: only that hop's key opens it).
func PeelBackward(hops []*HopKeys, box []byte) (hop int, cmd byte, streamID uint16, data []byte, err error) {
	cur := box
	for i, k := range hops {
		seq, pt, e := openSeq(k.Backward, cur)
		if e != nil {
			err = e
			return
		}
		if !k.acceptBwd(seq) {
			err = ErrReplay
			return
		}
		if len(pt) >= 1 && pt[0] == 1 {
			cur = pt[1:]
			continue
		}
		if len(pt) < 3 {
			err = errors.New("short reply")
			return
		}
		return i, pt[0], binary.BigEndian.Uint16(pt[1:3]), pt[3:], nil
	}
	err = errors.New("reply had no terminal layer")
	return
}

// Home-node tunnel commands.
const (
	CmdHomeHello byte = 14 // home node → harbour: account ‖ sig ‖ poolTag: "reach me through this connection"
	CmdHomeOK    byte = 15
)

// Gossip commands (circuit 0, over any tunnel connection): a peer or client
// asks a relay for the signed relay records it knows; the relay answers with
// JSON chunks (StreamID = sequence) and an empty final chunk.
const (
	CmdRelays      byte = 16
	CmdRelaysReply byte = 17
	// CmdPadding cells carry nothing and are dropped by the receiver. They are
	// mixed into bursts so a new stream's record-size sequence (an inner TLS
	// handshake, say) does not look like a textbook TLS-in-TLS exchange.
	CmdPadding byte = 18
	// CmdCover (circuit 0, client → entry): switch this connection to cadence
	// mode. Payload: cadence in ms (uint16) ‖ max cells per tick (uint8). Both
	// sides then send at least one cell per tick, padding when idle, so an
	// observer of the entry link sees a steady rhythm instead of the shape of
	// what the user does.
	CmdCover byte = 19
	// CmdRPC (circuit 0, client → entry): a Nano RPC request (JSON) for the
	// relay to forward to its own ledger source, so a fresh client reads the
	// ledger without ever connecting to anything but its entry. StreamID is
	// the request id; CmdRPCReply cells carry the answer in order, ending with
	// an empty payload. Rate-limited and action-filtered by the relay.
	CmdRPC      byte = 20
	CmdRPCReply byte = 21
	// CmdWatch (circuit 0, client → entry): payload is an account; the entry
	// subscribes to its confirmations upstream and pushes each one back as a
	// CmdNotify cell (JSON: account, amount, hash, subtype, link). A client
	// waiting for funds learns of them the second they confirm.
	CmdWatch  byte = 22
	CmdNotify byte = 23

	// Stream flow control (inside the onion, stream-scoped). A stream opened
	// with BEGIN2 is windowed in both directions: each side may have at most
	// StreamWindow cells outstanding and the receiver returns CREDIT cells
	// (uint32 count) as it consumes. A slow consumer then holds back only
	// its own stream instead of the whole circuit.
	CmdBegin2 byte = 24 // like CmdBegin, with flow control
	CmdCredit byte = 25 // payload: uint32 cells consumed, add to the sender's window
	// CmdTopUp (client → entry, inside the onion): payload is a signed XNO send
	// block to the entry; the entry publishes it and adds its value to the
	// circuit's existing quota, answering with CmdQuota. A circuit therefore
	// never dies at a payment boundary: the client tops it up in place.
	CmdTopUp byte = 26
	// QuotaLowStream is the stream id of an unsolicited CmdQuota from the
	// entry: the quota is under a quarter, top up now.
	QuotaLowStream uint16 = 2
)

const (
	// StreamWindow is the initial per-stream window in cells (about 2 MB).
	// The receiver owns the buffer, so it may grow the window by granting
	// more credit than it consumed, up to MaxStreamWindow (16 MB): a 600 ms
	// circuit then carries 25 MB/s on one stream. It grows only while the
	// consumer keeps draining the buffer, so a slow reader stays small.
	StreamWindow    = 2048
	MaxStreamWindow = 16384
	// CreditEvery is how many consumed cells trigger a CREDIT at the initial
	// window; it scales with the window (a quarter of it).
	CreditEvery = StreamWindow / 4
)

// PaddingCell returns a marshalled padding cell (random payload).
func PaddingCell() []byte { return (&Cell{Cmd: CmdPadding}).Marshal() }
