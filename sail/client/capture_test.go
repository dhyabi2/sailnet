package client

import (
	"bufio"
	"crypto/tls"
	"net"
	"testing"
	"time"
)

func TestSinkholeAnswer(t *testing.T) {
	// A query for example.com
	q := []byte{0x12, 0x34, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1}
	a := sinkholeAnswer(q)
	if a == nil || a[2]&0x80 == 0 || a[7] != 1 || string(a[len(a)-4:]) != string([]byte{127, 0, 0, 1}) {
		t.Fatalf("bad A answer: %x", a)
	}
	q[len(q)-3] = 28 // AAAA
	if a := sinkholeAnswer(q); a == nil || a[7] != 0 {
		t.Fatalf("bad AAAA answer: %x", a)
	}
	q[len(q)-3] = 15 // MX → not ours
	if sinkholeAnswer(q) != nil {
		t.Fatal("MX should be forwarded")
	}
}

func TestParseSNI(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, _ := net.Dial("tcp", ln.Addr().String())
		tls.Client(c, &tls.Config{ServerName: "www.wikipedia.org", InsecureSkipVerify: true}).Handshake()
		c.Close()
	}()
	c, _ := ln.Accept()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, host, err := readClientHello(bufio.NewReader(c))
	if err != nil || host != "www.wikipedia.org" {
		t.Fatalf("sni=%q err=%v", host, err)
	}
}
