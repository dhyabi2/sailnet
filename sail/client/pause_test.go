package client

import (
	"testing"
	"time"

	"github.com/dhyabi2/sail/relay"
)

// Out of sight, the app spends nothing: Pause closes the circuit and no
// build (and so no payment) happens until Resume. The anchor and the path
// are kept so Resume goes back through the same entry.

func TestPauseClosesTheCircuitAndBuildsNothing(t *testing.T) {
	m := mineManager(t, nil)
	path, err := m.choosePath()
	if err != nil {
		t.Fatal(err)
	}
	c := &relay.Circuit{Path: path}
	m.cur = c
	m.live.Store(c)
	m.tag = [32]byte{7}
	m.paidTo = path[0].Account

	m.Pause()
	if !c.Closed() {
		t.Fatal("the running circuit must close on Pause")
	}
	if _, err := m.circuit(); err != errPaused {
		t.Fatalf("a paused manager must not build; got %v", err)
	}
	if m.tag != ([32]byte{7}) {
		t.Fatal("the prepaid anchor tag must survive a pause")
	}
	if len(m.retryPath) != len(path) || m.retryPath[0].Account != path[0].Account {
		t.Fatal("Resume should try the same path, so the anchor at its entry is reused")
	}
	if !m.Paused() || m.Stage() != "Paused" {
		t.Fatalf("paused=%v stage=%q", m.Paused(), m.Stage())
	}
	m.Pause() // twice is harmless
}

func TestResumeLiftsThePause(t *testing.T) {
	m := mineManager(t, nil)
	m.Pause()
	m.lastFail = time.Now() // Resume's background build backs off instead of dialling anything
	m.Resume()
	if m.Paused() {
		t.Fatal("Resume must lift the pause")
	}
	m.Resume() // twice is harmless
}
