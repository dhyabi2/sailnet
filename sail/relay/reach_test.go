package relay

import (
	"net"
	"testing"
	"time"
)

// The stats page counts a relay as alive when a TCP connection to it opens,
// not only when its gossip happens to reach us. On 2026-09-09 gossip alone
// reported 5 of 12 reachable relays.

func listenerRelay(t *testing.T, acct string) (*RelayInfo, net.Listener) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	return &RelayInfo{Account: acct, MinRate: 1, Desc: Descriptor{IP: net.ParseIP("127.0.0.1"), Port: uint16(port)}}, l
}

func TestSweepReachabilityRecordsWhoAnswers(t *testing.T) {
	up, l := listenerRelay(t, "nano_up")
	defer l.Close()
	down, l2 := listenerRelay(t, "nano_down")
	l2.Close() // registered, nobody home
	reg := &Registry{relays: map[string]*RelayInfo{up.Account: up, down.Account: down}}
	s := &Server{Registry: reg}

	s.sweepReachability()

	if !s.reachedWithin("nano_up", time.Minute) {
		t.Fatal("a relay that accepted a connection should count as reached")
	}
	if s.reachedWithin("nano_down", time.Minute) {
		t.Fatal("a relay nobody could connect to must not count")
	}
	if s.reachedWithin("nano_never", time.Minute) {
		t.Fatal("an account never probed must not count")
	}
}

func TestReachedWithinExpires(t *testing.T) {
	s := &Server{reached: map[string]time.Time{"nano_old": time.Now().Add(-4 * time.Hour)}}
	if s.reachedWithin("nano_old", 3*time.Hour) {
		t.Fatal("a connection four hours ago is not proof of presence now")
	}
}
