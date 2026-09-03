package wire

import (
	"bufio"
	"bytes"
	"net"
	"testing"
)

func TestWSFramingRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	client := NewWSConn(a, nil, true)
	server := NewWSConn(b, nil, false)
	msg := bytes.Repeat([]byte("cell"), 300) // 1200 bytes: a 16-bit length frame
	go func() {
		client.Write(msg[:100])
		client.Write(msg[100:])
	}()
	got := make([]byte, len(msg))
	if _, err := readFull(server, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("client→server payload mismatch")
	}
	go server.Write(msg)
	got2 := make([]byte, len(msg))
	if _, err := readFull(client, got2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got2, msg) {
		t.Fatal("server→client payload mismatch")
	}
	// a ping from the client is answered with a pong and never surfaces as data
	go func() { client.Read(make([]byte, 16)) }() // drain the pong (net.Pipe is synchronous)
	go func() { client.writeFrame(0x9, []byte("hi")); client.Write([]byte("after")) }()
	buf := make([]byte, 5)
	if _, err := readFull(server, buf); err != nil || string(buf) != "after" {
		t.Fatalf("ping handling: %q %v", buf, err)
	}
	_ = bufio.NewReader
}

func readFull(c *WSConn, p []byte) (int, error) {
	n := 0
	for n < len(p) {
		m, err := c.Read(p[n:])
		if err != nil {
			return n, err
		}
		n += m
	}
	return n, nil
}
