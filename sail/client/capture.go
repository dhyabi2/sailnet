package client

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Whole-device capture without a root daemon: a DNS
// sinkhole on :53 answers every name with 127.0.0.1, so every application's
// TCP flow to port 80 or 443 lands on our listeners; the Host header or the
// TLS SNI names the real destination and the flow goes out through the
// circuit. Names an app resolves never reach any resolver on the local
// network. Binding ports below 1024 needs administrator rights.

// serveSinkholeDNS answers A queries with 127.0.0.1, AAAA with an empty
// answer, and forwards anything else through the circuit resolver.
func (m *manager) serveSinkholeDNS(addr, upstream string) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Printf("capture: cannot listen on %s: %v (administrator rights needed for :53)", addr, err)
		return
	}
	log.Printf("capture: DNS sinkhole on %s (every name → 127.0.0.1)", addr)
	buf := make([]byte, 4096)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			continue
		}
		q := append([]byte(nil), buf[:n]...)
		go func() {
			if ans := sinkholeAnswer(q); ans != nil {
				pc.WriteTo(ans, from)
				return
			}
			if ans, err := m.resolveViaCircuit(q, upstream); err == nil {
				pc.WriteTo(ans, from)
			}
		}()
	}
}

// sinkholeAnswer builds a response for A/AAAA questions; nil for other types.
func sinkholeAnswer(q []byte) []byte {
	if len(q) < 12 || binary.BigEndian.Uint16(q[4:6]) != 1 {
		return nil
	}
	i := 12
	for i < len(q) && q[i] != 0 { // skip the name
		i += int(q[i]) + 1
	}
	if i+5 > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[i+1 : i+3])
	qend := i + 5
	if qtype != 1 && qtype != 28 {
		return nil
	}
	resp := append([]byte(nil), q[:qend]...)
	resp[2] = 0x81 // response, recursion desired
	resp[3] = 0x80 // recursion available, NOERROR
	binary.BigEndian.PutUint16(resp[6:8], 0)
	binary.BigEndian.PutUint16(resp[8:10], 0)
	binary.BigEndian.PutUint16(resp[10:12], 0)
	if qtype == 1 {
		binary.BigEndian.PutUint16(resp[6:8], 1)
		resp = append(resp, 0xC0, 0x0C, 0, 1, 0, 1, 0, 0, 0, 30, 0, 4, 127, 0, 0, 1)
	}
	return resp
}

// serveCapture accepts flows on a local port and forwards each to the host
// named inside it (Host header on 80, SNI on 443), through the circuit.
func (m *manager) serveCapture(addr string, tls bool) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("capture: cannot listen on %s: %v (administrator rights needed)", addr, err)
		return
	}
	log.Printf("capture: listening on %s", addr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go m.captureFlow(conn, tls)
	}
}

func (m *manager) captureFlow(conn net.Conn, tls bool) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	br := bufio.NewReaderSize(conn, 64<<10)
	var head []byte
	var host string
	var err error
	if tls {
		head, host, err = readClientHello(br)
	} else {
		head, host, err = readHTTPHead(br)
	}
	conn.SetReadDeadline(time.Time{})
	if err != nil || host == "" {
		return
	}
	port := "80"
	if tls {
		port = "443"
	}
	if h, p, e := net.SplitHostPort(host); e == nil {
		host, port = h, p
	}
	c, err := m.circuit()
	if err != nil {
		log.Printf("capture: flow failed: %v", err) // never the hostname
		return
	}
	st, err := c.OpenOptimistic(net.JoinHostPort(host, port))
	if err != nil {
		return
	}
	defer st.Close()
	if _, err := st.Write(head); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(Up(st), br); st.Close(); done <- struct{}{} }()
	go func() { io.Copy(Down(conn), st); done <- struct{}{} }()
	<-done
}

// readHTTPHead reads the request head and returns it with the Host value.
func readHTTPHead(br *bufio.Reader) ([]byte, string, error) {
	var head []byte
	host := ""
	for {
		line, err := br.ReadBytes('\n')
		head = append(head, line...)
		if err != nil {
			return head, host, err
		}
		if l := strings.ToLower(string(bytes.TrimSpace(line))); strings.HasPrefix(l, "host:") {
			host = strings.TrimSpace(string(bytes.TrimSpace(line))[5:])
		}
		if len(bytes.TrimSpace(line)) == 0 || len(head) > 64<<10 {
			return head, host, nil
		}
	}
}

// readClientHello reads one TLS record and returns it with the SNI it carries.
func readClientHello(br *bufio.Reader) ([]byte, string, error) {
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return nil, "", err
	}
	if hdr[0] != 0x16 {
		return nil, "", fmt.Errorf("not a TLS handshake")
	}
	n := int(binary.BigEndian.Uint16(hdr[3:5]))
	if n > 16384 {
		return nil, "", fmt.Errorf("record too large")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, "", err
	}
	rec := append(hdr, body...)
	return rec, parseSNI(body), nil
}

// parseSNI walks a ClientHello and returns the server_name, or "".
func parseSNI(b []byte) string {
	if len(b) < 4 || b[0] != 1 {
		return ""
	}
	p := 4 + 2 + 32 // handshake header, version, random
	if len(b) < p+1 {
		return ""
	}
	p += 1 + int(b[p]) // session id
	if len(b) < p+2 {
		return ""
	}
	p += 2 + int(binary.BigEndian.Uint16(b[p:])) // cipher suites
	if len(b) < p+1 {
		return ""
	}
	p += 1 + int(b[p]) // compression
	if len(b) < p+2 {
		return ""
	}
	extLen := int(binary.BigEndian.Uint16(b[p:]))
	p += 2
	end := p + extLen
	if end > len(b) {
		return ""
	}
	for p+4 <= end {
		typ := binary.BigEndian.Uint16(b[p:])
		l := int(binary.BigEndian.Uint16(b[p+2:]))
		p += 4
		if p+l > end {
			return ""
		}
		if typ == 0 && l >= 5 { // server_name
			q := p + 2 // list length
			if b[q] == 0 {
				nl := int(binary.BigEndian.Uint16(b[q+1:]))
				if q+3+nl <= p+l {
					return string(b[q+3 : q+3+nl])
				}
			}
			return ""
		}
		p += l
	}
	return ""
}

// ---------------------------------------------------------------- OS resolver

// subvertDNS points the operating system's resolver at 127.0.0.1 and keeps a
// backup so revertDNS can restore it. macOS: every network service via
// networksetup. Linux: /etc/resolv.conf.
func subvertDNS() error {
	backup := filepath.Join(dataDir(), "dns-backup.json")
	switch runtime.GOOS {
	case "darwin":
		out, err := command("networksetup", "-listallnetworkservices").Output()
		if err != nil {
			return err
		}
		saved := map[string][]string{}
		for _, svc := range strings.Split(string(out), "\n") {
			svc = strings.TrimSpace(svc)
			if svc == "" || strings.HasPrefix(svc, "*") || strings.Contains(svc, "asterisk") {
				continue
			}
			cur, _ := command("networksetup", "-getdnsservers", svc).Output()
			var list []string
			for _, l := range strings.Split(string(cur), "\n") {
				if ip := net.ParseIP(strings.TrimSpace(l)); ip != nil {
					list = append(list, ip.String())
				}
			}
			saved[svc] = list
			if err := command("networksetup", "-setdnsservers", svc, "127.0.0.1").Run(); err != nil {
				return fmt.Errorf("networksetup %s: %v (run with sudo)", svc, err)
			}
		}
		data, _ := json.MarshalIndent(saved, "", "  ")
		os.WriteFile(backup, data, 0o600)
		command("dscacheutil", "-flushcache").Run()
		command("killall", "-HUP", "mDNSResponder").Run()
		log.Printf("capture: system DNS now 127.0.0.1 on %d service(s); backup in %s", len(saved), backup)
		return nil
	case "linux":
		old, err := os.ReadFile("/etc/resolv.conf")
		if err != nil {
			return err
		}
		os.WriteFile(backup, old, 0o600)
		if err := os.WriteFile("/etc/resolv.conf", []byte("nameserver 127.0.0.1\n"), 0o644); err != nil {
			return fmt.Errorf("%v (run as root)", err)
		}
		log.Printf("capture: /etc/resolv.conf now 127.0.0.1; backup in %s", backup)
		return nil
	}
	return fmt.Errorf("dns subversion not implemented on %s", runtime.GOOS)
}

// revertDNS restores what subvertDNS saved.
func revertDNS() {
	backup := filepath.Join(dataDir(), "dns-backup.json")
	data, err := os.ReadFile(backup)
	if err != nil {
		return
	}
	switch runtime.GOOS {
	case "darwin":
		var saved map[string][]string
		if json.Unmarshal(data, &saved) != nil {
			return
		}
		for svc, list := range saved {
			args := append([]string{"-setdnsservers", svc}, list...)
			if len(list) == 0 {
				args = append(args, "Empty")
			}
			command("networksetup", args...).Run()
		}
		command("dscacheutil", "-flushcache").Run()
		command("killall", "-HUP", "mDNSResponder").Run()
	case "linux":
		os.WriteFile("/etc/resolv.conf", data, 0o644)
	}
	os.Remove(backup)
	log.Printf("capture: system DNS restored")
}
