package client

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
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
	// Wait on the servers themselves, not by binding their ports: a probe
	// that binds while a server is still binding takes the port from it.
	waitUntil(t, "the servers bind their ports", func() bool { return serversReady(m) })

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
	waitUntil(t, "a fresh manager takes the freed ports", func() bool { return serversReady(m2) })
	m2.Shutdown()
}

func TestServersStartedAfterShutdownOwnNothing(t *testing.T) {
	tcp, udp := freeTCPPort(t), freeUDPPort(t)
	m := mineManager(t, nil)
	m.Shutdown() // first: the manager is already gone when the servers start
	// Both return at once on a stopped manager, so no goroutine and no sleep:
	// the ports are probed only after the servers are done with them.
	m.ServeStatus(fmt.Sprintf("127.0.0.1:%d", tcp))
	m.serveDNS(fmt.Sprintf("127.0.0.1:%d", udp), "1.1.1.1:53")
	if tcpTaken(tcp) || udpTaken(udp) {
		t.Fatal("a server started after Shutdown must not hold its port")
	}
}

// A browser extension holds its connection to the status endpoint open
// (keep-alive). Closing the listener alone left that connection served by
// the dead manager: GET /status still said running=true, and POST /rebuild
// started a circuit build — and paid for it — on a manager nobody owned.
func TestShutdownCutsKeepAliveStatusConnections(t *testing.T) {
	m := mineManager(t, nil)
	m.lastFail = time.Now() // were a build started, it stops before any network
	addr := fmt.Sprintf("127.0.0.1:%d", freeTCPPort(t))
	go m.ServeStatus(addr)
	waitUntil(t, "the status endpoint is ready", func() bool { return statusReady(m) })

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	ask := func(method, path string) (*http.Response, error) {
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: x\r\nX-Sail: 1\r\nContent-Length: 0\r\n\r\n", method, path); err != nil {
			return nil, err
		}
		resp, err := http.ReadResponse(br, nil)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		return resp, err
	}
	if resp, err := ask("GET", "/status"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("before Shutdown the endpoint should answer: %v %v", resp, err)
	}

	m.Shutdown()

	if resp, err := ask("POST", "/rebuild"); err == nil {
		t.Fatalf("a kept-alive connection was still served after Shutdown: POST /rebuild → %s", resp.Status)
	}
}

// A circuit is never built for a manager that has been shut down, whoever asks.
func TestNoCircuitAfterShutdown(t *testing.T) {
	m := mineManager(t, nil)
	m.lastFail = time.Now() // without the stop check this returns "retrying shortly", offline
	m.Shutdown()
	if _, err := m.circuit(); err != errStopped {
		t.Fatalf("circuit() after Shutdown returned %v, want errStopped", err)
	}
}

func statusReady(m *manager) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusSrv != nil
}

func serversReady(m *manager) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusSrv != nil && m.dnsPC != nil
}
