package relay

import (
	"net"
	"testing"
	"time"
)

func TestUDPFramingRoundTrip(t *testing.T) {
	// echo server
	pc, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer pc.Close()
	go func() {
		b := make([]byte, 2000)
		for {
			n, a, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			pc.WriteTo(b[:n], a)
		}
	}()
	c, err := net.DialUDP("udp", nil, pc.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	u := &udpConn{UDPConn: c}
	// two datagrams written in three odd chunks
	f := append(Frame([]byte("hello")), Frame([]byte("world!!"))...)
	u.Write(f[:3])
	u.Write(f[3:9])
	u.Write(f[9:])
	var d Deframer
	buf := make([]byte, 1024)
	u.UDPConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for i := 0; i < 2; i++ {
		n, err := u.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		d.Push(buf[:n])
	}
	if a, b := string(d.Next()), string(d.Next()); a != "hello" || b != "world!!" {
		t.Fatalf("got %q %q", a, b)
	}
	if _, err := dialUDP("127.0.0.1:53", time.Second); err == nil {
		t.Fatal("private destination accepted")
	}
}
