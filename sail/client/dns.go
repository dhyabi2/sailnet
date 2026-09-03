package client

import (
	"encoding/binary"
	"io"
	"log"
	"net"
	"time"
)

// serveDNS answers UDP DNS queries by forwarding each one, over TCP, through
// the live circuit to a public resolver at the exit. The local network sees
// no DNS traffic at all, so DNS-based blocking and logging on that network
// cannot see or touch the names the user resolves. Point the system resolver
// (or an application) at this address.
func (m *manager) serveDNS(addr, upstream string) {
	if h, _, err := net.SplitHostPort(upstream); err == nil {
		KeepIP(h) // a public resolver is not the user's device
	}
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Printf("dns: cannot listen on %s: %v", addr, err)
		return
	}
	log.Printf("DNS through the circuit on %s (upstream %s at the exit)", addr, upstream)
	buf := make([]byte, 4096)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			continue
		}
		q := append([]byte(nil), buf[:n]...)
		go func() {
			ans, err := m.resolveViaCircuit(q, upstream)
			if err != nil {
				log.Printf("dns: %v", err)
				// SERVFAIL with the query's id so the client retries later
				if len(q) >= 12 {
					fail := append([]byte(nil), q[:12]...)
					fail[2] |= 0x80 // response
					fail[3] = (fail[3] & 0xf0) | 2
					fail[6], fail[7], fail[8], fail[9], fail[10], fail[11] = 0, 0, 0, 0, 0, 0
					pc.WriteTo(fail, from)
				}
				return
			}
			pc.WriteTo(ans, from)
		}()
	}
}

// resolveViaCircuit sends one DNS message (TCP framing) to upstream through
// the circuit's exit and returns the answer.
func (m *manager) resolveViaCircuit(q []byte, upstream string) ([]byte, error) {
	c, err := m.circuit()
	if err != nil {
		return nil, err
	}
	st, err := c.Open(upstream, 15*time.Second)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	msg := make([]byte, 2+len(q))
	binary.BigEndian.PutUint16(msg, uint16(len(q)))
	copy(msg[2:], q)
	if _, err := st.Write(msg); err != nil {
		return nil, err
	}
	var hdr [2]byte
	done := make(chan error, 1)
	var ans []byte
	go func() {
		if _, err := io.ReadFull(st, hdr[:]); err != nil {
			done <- err
			return
		}
		ans = make([]byte, binary.BigEndian.Uint16(hdr[:]))
		_, err := io.ReadFull(st, ans)
		done <- err
	}()
	select {
	case err := <-done:
		return ans, err
	case <-time.After(15 * time.Second):
		return nil, errors("dns: timeout through the circuit")
	}
}
