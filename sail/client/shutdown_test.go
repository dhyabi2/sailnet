package client

import (
	"fmt"
	"net"
	"testing"
	"time"
)

// A disconnected app must leave nothing behind. Before Shutdown closed the
// DNS and status servers, a stop() left both listening on a dead manager —
// the next connect collided with them (no DNS of its own, an extension
// reading the corpse's status), and /status kept answering running=true for a
// tunnel that was gone.

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()
	return p
}

func tcpTaken(port int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return true
	}
	l.Close()
	return false
}

func udpTaken(port int) bool {
	pc, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return true
	}
	pc.Close()
	return false
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never happened", what)
}

func TestShutdownClosesTheDNSAndStatusServers(t *testing.T) {
	tcp, udp := freeTCPPort(t), freeUDPPort(t)
	m := mineManager(t, nil)
	go m.serveDNS(fmt.Sprintf("127.0.0.1:%d", udp), "1.1.1.1:53")
	go m.ServeStatus(fmt.Sprintf("127.0.0.1:%d", tcp))
	waitUntil(t, "the servers bind their ports", func() bool { return tcpTaken(tcp) && udpTaken(udp) })

	m.Shutdown()

	if tcpTaken(tcp) {
		t.Fatal("the status endpoint outlived the manager")
	}
	if udpTaken(udp) {
		t.Fatal("the DNS resolver outlived the manager")
	}

	// The next manager must be able to take the very same ports.
	m2 := mineManager(t, nil)
	go m2.ServeStatus(fmt.Sprintf("127.0.0.1:%d", tcp))
	go m2.serveDNS(fmt.Sprintf("127.0.0.1:%d", udp), "1.1.1.1:53")
	waitUntil(t, "a fresh manager takes the freed ports", func() bool { return tcpTaken(tcp) && udpTaken(udp) })
}

func TestServersStartedAfterShutdownOwnNothing(t *testing.T) {
	tcp, udp := freeTCPPort(t), freeUDPPort(t)
	m := mineManager(t, nil)
	m.Shutdown() // first: the manager is already gone when the server goroutines run
	go m.ServeStatus(fmt.Sprintf("127.0.0.1:%d", tcp))
	go m.serveDNS(fmt.Sprintf("127.0.0.1:%d", udp), "1.1.1.1:53")
	time.Sleep(200 * time.Millisecond)
	if tcpTaken(tcp) || udpTaken(udp) {
		t.Fatal("a server started after Shutdown must not hold its port")
	}
}
