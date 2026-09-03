package relay

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"time"
)

// UDP through the circuit. A stream opened with target "udp:host:port" is a
// datagram channel: every datagram travels as a 2-byte big-endian length
// followed by its bytes, in both directions. The exit keeps one connected
// UDP socket per stream, so games, calls and QUIC work like any other
// internet connection instead of being dropped.

const UDPPrefix = "udp:"

// MaxDatagram is the largest datagram carried (TUN MTU minus IP/UDP headers).
const MaxDatagram = 1472

// udpConn adapts a UDP socket to the byte-stream shape the exit's stream
// loop expects: Read returns framed datagrams, Write accepts framed bytes in
// any chunking and sends each complete datagram.
type udpConn struct {
	*net.UDPConn
	pending []byte
}

func dialUDP(target string, timeout time.Duration) (net.Conn, error) {
	addr, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		return nil, err
	}
	if addr.IP.IsLoopback() || addr.IP.IsPrivate() || addr.IP.IsLinkLocalUnicast() {
		return nil, errors.New("udp: private destination refused")
	}
	c, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return nil, err
	}
	return &udpConn{UDPConn: c}, nil
}

// Read returns one datagram framed with its length.
func (u *udpConn) Read(p []byte) (int, error) {
	buf := make([]byte, MaxDatagram)
	u.UDPConn.SetReadDeadline(time.Now().Add(120 * time.Second)) // idle flows end
	n, err := u.UDPConn.Read(buf)
	if err != nil {
		return 0, err
	}
	if n+2 > len(p) {
		n = len(p) - 2 // never happens with cell-sized buffers; keep the frame valid
	}
	binary.BigEndian.PutUint16(p, uint16(n))
	copy(p[2:], buf[:n])
	return n + 2, nil
}

// Write consumes framed bytes and sends every complete datagram.
func (u *udpConn) Write(p []byte) (int, error) {
	u.pending = append(u.pending, p...)
	for len(u.pending) >= 2 {
		n := int(binary.BigEndian.Uint16(u.pending))
		if n > MaxDatagram {
			return 0, errors.New("udp: bad frame")
		}
		if len(u.pending) < 2+n {
			break
		}
		if _, err := u.UDPConn.Write(u.pending[2 : 2+n]); err != nil {
			return 0, err
		}
		u.pending = u.pending[2+n:]
	}
	return len(p), nil
}

// Frame prefixes a datagram with its length for sending on a udp: stream.
func Frame(d []byte) []byte {
	out := make([]byte, 2+len(d))
	binary.BigEndian.PutUint16(out, uint16(len(d)))
	copy(out[2:], d)
	return out
}

// Deframer turns the byte stream coming back from a udp: stream into datagrams.
type Deframer struct{ buf []byte }

// Push adds bytes; Next returns the next complete datagram or nil.
func (d *Deframer) Push(p []byte) { d.buf = append(d.buf, p...) }
func (d *Deframer) Next() []byte {
	if len(d.buf) < 2 {
		return nil
	}
	n := int(binary.BigEndian.Uint16(d.buf))
	if len(d.buf) < 2+n {
		return nil
	}
	out := append([]byte(nil), d.buf[2:2+n]...)
	d.buf = d.buf[2+n:]
	return out
}

// dialTCPPublic connects to target only if every resolved address is public:
// an exit must not be a door into its operator's LAN, loopback or cloud
// metadata service, nor into another user's home network.
func dialTCPPublic(target string, timeout time.Duration) (net.Conn, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil {
		return nil, err
	}
	var last error
	for _, ip := range ips {
		if ip.IP.IsLoopback() || ip.IP.IsPrivate() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsUnspecified() || ip.IP.IsLinkLocalMulticast() || ip.IP.IsMulticast() {
			return nil, errors.New("private destination refused")
		}
	}
	for _, ip := range ips {
		c, err := (&net.Dialer{Timeout: timeout}).Dial("tcp", net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}
