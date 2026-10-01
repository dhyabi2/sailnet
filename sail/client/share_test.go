package client

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
)

// A guest on the hotspot reaches public HTTPS hosts through the circuit and
// nothing else: not this phone, not its LAN, not plain HTTP.

// fakeOpen stands in for the circuit: it records the target and echoes.
func fakeOpen(got *[]string) func(string) (net.Conn, error) {
	return func(target string) (net.Conn, error) {
		*got = append(*got, target)
		a, b := net.Pipe()
		go func() { io.Copy(b, b) }() // echo
		return a, nil
	}
}

func guest(t *testing.T, open func(string) (net.Conn, error)) net.Conn {
	t.Helper()
	g, s := net.Pipe()
	go serveShare(s, open)
	return g
}

func TestShareConnectReachesAPublicHost(t *testing.T) {
	var got []string
	g := guest(t, fakeOpen(&got))
	defer g.Close()
	io.WriteString(g, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\nhello")
	br := bufio.NewReader(g)
	line, _ := br.ReadString('\n')
	if !strings.Contains(line, "200") {
		t.Fatalf("CONNECT answered %q", line)
	}
	br.ReadString('\n') // blank line
	buf := make([]byte, 5)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("bytes sent with the CONNECT must reach the host: %q %v", buf, err)
	}
	if len(got) != 1 || got[0] != "example.com:443" {
		t.Fatalf("opened %v", got)
	}
}

func TestShareRefusesLocalAndPlainHTTP(t *testing.T) {
	for _, req := range []string{
		"CONNECT 192.168.1.1:443 HTTP/1.1\r\nHost: 192.168.1.1:443\r\n\r\n",
		"CONNECT 127.0.0.1:8080 HTTP/1.1\r\nHost: 127.0.0.1:8080\r\n\r\n",
		"CONNECT localhost:443 HTTP/1.1\r\nHost: localhost:443\r\n\r\n",
		"CONNECT 10.0.0.5:22 HTTP/1.1\r\nHost: 10.0.0.5:22\r\n\r\n",
		"CONNECT example.com:80 HTTP/1.1\r\nHost: example.com:80\r\n\r\n",
		"GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n",
	} {
		var got []string
		g := guest(t, fakeOpen(&got))
		io.WriteString(g, req)
		line, _ := bufio.NewReader(g).ReadString('\n')
		g.Close()
		if !strings.Contains(line, "403") || len(got) != 0 {
			t.Fatalf("%q: answered %q, opened %v", strings.SplitN(req, "\r", 2)[0], line, got)
		}
	}
}

func socks(t *testing.T, g net.Conn, atyp byte, addr []byte, port int) byte {
	t.Helper()
	g.Write([]byte{5, 1, 0})
	r := make([]byte, 2)
	io.ReadFull(g, r)
	req := []byte{5, 1, 0, atyp}
	if atyp == 3 {
		req = append(req, byte(len(addr)))
	}
	req = append(req, addr...)
	req = append(req, byte(port>>8), byte(port))
	g.Write(req)
	rep := make([]byte, 10)
	io.ReadFull(g, rep)
	return rep[1]
}

func TestShareSocks(t *testing.T) {
	var got []string
	g := guest(t, fakeOpen(&got))
	if code := socks(t, g, 3, []byte("example.com"), 443); code != 0 || len(got) != 1 || got[0] != "example.com:443" {
		t.Fatalf("SOCKS to a public host: code %d, opened %v", code, got)
	}
	g.Close()
	for _, c := range []struct {
		atyp byte
		addr []byte
		port int
	}{{1, []byte{127, 0, 0, 1}, 443}, {1, []byte{192, 168, 43, 1}, 8080}, {3, []byte("example.com"), 80}} {
		got = nil
		g := guest(t, fakeOpen(&got))
		if code := socks(t, g, c.atyp, c.addr, c.port); code != 2 || len(got) != 0 {
			t.Fatalf("SOCKS %v:%d must be refused, got code %d, opened %v", c.addr, c.port, code, got)
		}
		g.Close()
	}
}
