package client

import (
	"bufio"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// Sharing: other devices on this phone's hotspot reach the circuit through
// one proxy port that speaks both HTTP CONNECT (what a phone's or laptop's
// Wi-Fi proxy setting sends) and SOCKS5. Unlike the local SOCKS port, a
// guest never reaches anything directly: private, loopback and link-local
// destinations (this phone, its LAN, the router) are refused, and so is
// plain HTTP. Every byte a guest moves is paid from this wallet.

// ShareBytes counts what guests moved through the circuit, both directions.
var ShareBytes atomic.Int64

// ServeShare answers one guest connection.
func (m *manager) ServeShare(conn net.Conn) {
	serveShare(conn, func(target string) (net.Conn, error) {
		c, err := m.circuit()
		if err != nil {
			return nil, err
		}
		st, err := c.OpenOptimistic(target)
		if err != nil {
			return nil, err
		}
		return streamConn{st}, nil
	})
}

// shareRefused reports why a guest may not reach target, or "" if it may.
func shareRefused(host string, port int) string {
	if port == 80 {
		return "plain HTTP"
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast()) {
		return "local address"
	}
	if host == "localhost" {
		return "local address"
	}
	return ""
}

func serveShare(conn net.Conn, open func(target string) (net.Conn, error)) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(conn)
	first, err := br.Peek(1)
	if err != nil {
		return
	}
	if first[0] == 5 {
		shareSocks(conn, br, open)
		return
	}
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		// A proxied plain-HTTP request: the exit and every network after it
		// could read it, so it is answered here and never sent.
		io.WriteString(conn, "HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain\r\nConnection: close\r\nContent-Length: 30\r\n\r\nSailnet carries HTTPS only.\r\n\r\n")
		return
	}
	host, ps, err := net.SplitHostPort(req.Host)
	port, _ := strconv.Atoi(ps)
	if err != nil || port <= 0 || port > 65535 {
		io.WriteString(conn, "HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n")
		return
	}
	if why := shareRefused(host, port); why != "" {
		io.WriteString(conn, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return
	}
	up, err := open(net.JoinHostPort(host, ps))
	if err != nil {
		io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
		return
	}
	defer up.Close()
	io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
	conn.SetReadDeadline(time.Time{})
	pipeShare(conn, br, up)
}

// shareSocks is SOCKS5 CONNECT with no authentication, as serveSocks, but
// with the guest rules applied.
func shareSocks(conn net.Conn, br *bufio.Reader, open func(string) (net.Conn, error)) {
	buf := make([]byte, 262)
	if _, err := io.ReadFull(br, buf[:2]); err != nil {
		return
	}
	if _, err := io.ReadFull(br, buf[:int(buf[1])]); err != nil {
		return
	}
	conn.Write([]byte{5, 0})
	if _, err := io.ReadFull(br, buf[:4]); err != nil || buf[1] != 1 {
		conn.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0}) // command not supported
		return
	}
	var host string
	switch buf[3] {
	case 1:
		if _, err := io.ReadFull(br, buf[:4]); err != nil {
			return
		}
		host = net.IP(buf[:4]).String()
	case 3:
		if _, err := io.ReadFull(br, buf[:1]); err != nil {
			return
		}
		l := int(buf[0])
		if _, err := io.ReadFull(br, buf[:l]); err != nil {
			return
		}
		host = string(buf[:l])
	case 4:
		if _, err := io.ReadFull(br, buf[:16]); err != nil {
			return
		}
		host = net.IP(buf[:16]).String()
	default:
		return
	}
	if _, err := io.ReadFull(br, buf[:2]); err != nil {
		return
	}
	port := int(buf[0])<<8 | int(buf[1])
	if shareRefused(host, port) != "" {
		conn.Write([]byte{5, 2, 0, 1, 0, 0, 0, 0, 0, 0}) // not allowed by ruleset
		return
	}
	up, err := open(net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		conn.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	conn.SetReadDeadline(time.Time{})
	pipeShare(conn, br, up)
}

// pipeShare copies both ways (from br, which may hold bytes already read)
// and counts them.
func pipeShare(conn net.Conn, br *bufio.Reader, up net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		n, _ := io.Copy(Up(up), br)
		ShareBytes.Add(n)
		up.Close()
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(Down(conn), up)
		ShareBytes.Add(n)
		done <- struct{}{}
	}()
	<-done
}

// ShareListener accepts guests on addr until closed. serve is looked up per
// connection, so a reconnect (a new manager) is picked up without
// listening again; nil means no tunnel, and the guest is turned away.
func ShareListener(addr string, serve func() func(net.Conn)) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return
			}
			if s := serve(); s != nil {
				go s(conn)
			} else {
				conn.Close()
			}
		}
	}()
	log.Printf("sharing on %s", redactAddr(addr))
	return ln, nil
}

// redactAddr keeps only the port: the hotspot address is this device's.
func redactAddr(addr string) string {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "a local port"
	}
	return "port " + p
}
