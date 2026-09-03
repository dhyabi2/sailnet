package wire

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
)

// WebSocket framing (RFC 6455, binary frames only). After the HTTP upgrade
// the tunnel carries cells inside real WebSocket frames, so an inspector
// that follows the handshake sees a well-formed WebSocket stream, and a CDN
// or reverse proxy that speaks WebSocket can sit in front of a bridge.
// Client-to-server frames are masked as the RFC requires. Read concatenates
// frame payloads into a byte stream, so callers keep reading cells as before.

type WSConn struct {
	net.Conn
	r      *bufio.Reader
	client bool // mask outgoing frames
	rest   []byte
	wmu    sync.Mutex
}

// NewWSConn wraps an upgraded connection. r may already hold buffered bytes.
func NewWSConn(c net.Conn, r *bufio.Reader, client bool) *WSConn {
	if r == nil {
		r = bufio.NewReader(c)
	}
	return &WSConn{Conn: c, r: r, client: client}
}

var errWSClosed = errors.New("websocket closed")

// readFrame returns the payload of the next data frame; control frames are
// answered or skipped. Fragments are appended to the stream as they arrive.
func (w *WSConn) readFrame() ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(w.r, hdr[:]); err != nil {
		return nil, err
	}
	op := hdr[0] & 0x0f
	masked := hdr[1]&0x80 != 0
	n := uint64(hdr[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(w.r, b[:]); err != nil {
			return nil, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(w.r, b[:]); err != nil {
			return nil, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > 1<<20 {
		return nil, errors.New("websocket frame too large")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(w.r, mask[:]); err != nil {
			return nil, err
		}
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(w.r, p); err != nil {
		return nil, err
	}
	if masked {
		for i := range p {
			p[i] ^= mask[i%4]
		}
	}
	switch op {
	case 0x8: // close
		return nil, errWSClosed
	case 0x9: // ping → pong
		w.writeFrame(0xA, p)
		return nil, nil
	case 0xA: // pong
		return nil, nil
	}
	return p, nil
}

func (w *WSConn) Read(p []byte) (int, error) {
	for len(w.rest) == 0 {
		f, err := w.readFrame()
		if err != nil {
			return 0, err
		}
		w.rest = f
	}
	n := copy(p, w.rest)
	w.rest = w.rest[n:]
	return n, nil
}

// writeFrame sends one frame with the given opcode.
func (w *WSConn) writeFrame(op byte, p []byte) error {
	hdr := make([]byte, 2, 14)
	hdr[0] = 0x80 | op
	switch {
	case len(p) < 126:
		hdr[1] = byte(len(p))
	case len(p) < 1<<16:
		hdr[1] = 126
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(len(p)))
	default:
		hdr[1] = 127
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(len(p)))
	}
	body := p
	if w.client {
		var mask [4]byte
		rand.Read(mask[:])
		hdr[1] |= 0x80
		hdr = append(hdr, mask[:]...)
		body = make([]byte, len(p))
		for i := range p {
			body[i] = p[i] ^ mask[i%4]
		}
	}
	// Header and payload go out in one Write, so the frame becomes a single
	// TLS record. Writing them separately put a tiny record in front of every
	// data record: a fingerprint no real WebSocket stack produces.
	frame := append(hdr, body...)
	w.wmu.Lock()
	defer w.wmu.Unlock()
	_, err := w.Conn.Write(frame)
	return err
}

// HeaderLen is the framing overhead this side adds to a payload of n bytes.
// The shaper needs it to hit an exact size on the wire.
func (w *WSConn) HeaderLen(n int) int {
	h := 2
	switch {
	case n < 126:
	case n < 1<<16:
		h += 2
	default:
		h += 8
	}
	if w.client {
		h += 4
	}
	return h
}

// Write sends p as one binary frame.
func (w *WSConn) Write(p []byte) (int, error) {
	if err := w.writeFrame(0x2, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Ping sends a WebSocket ping with a random payload of 20 to 120 bytes: a
// small first record after the handshake, as real WebSocket sessions have.
func (w *WSConn) Ping() error {
	var n [1]byte
	rand.Read(n[:])
	p := make([]byte, 20+int(n[0])%100)
	rand.Read(p)
	return w.writeFrame(0x9, p)
}
