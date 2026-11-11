package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A reprice cancels the registration for the old price, and the new one
// starts only after the old one has returned: never two searches at once.
func TestRegistrarCancelsStaleRegistration(t *testing.T) {
	var g registrar
	var running, maxRunning atomic.Int32
	firstStopped := make(chan struct{})
	enter := func() {
		n := running.Add(1)
		for {
			m := maxRunning.Load()
			if n <= m || maxRunning.CompareAndSwap(m, n) {
				break
			}
		}
	}
	g.start(func(ctx context.Context) { // the old price: blocks until cancelled
		enter()
		defer running.Add(-1)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // a search that takes a moment to notice
		close(firstStopped)
	})
	time.Sleep(20 * time.Millisecond)
	secondRan := make(chan struct{})
	g.start(func(ctx context.Context) {
		enter()
		defer running.Add(-1)
		select {
		case <-firstStopped:
		default:
			t.Error("new registration began before the stale one returned")
		}
		close(secondRan)
	})
	select {
	case <-secondRan:
	case <-time.After(2 * time.Second):
		t.Fatal("stale registration was never cancelled")
	}
	if m := maxRunning.Load(); m != 1 {
		t.Fatalf("%d registrations ran at once, want 1", m)
	}
}

// Three reprices in a row: the middle one, replaced before it began, never runs.
func TestRegistrarSkipsReplacedBeforeStart(t *testing.T) {
	var g registrar
	release := make(chan struct{})
	var ran atomic.Int32
	g.start(func(ctx context.Context) { <-ctx.Done(); <-release })
	g.start(func(ctx context.Context) { ran.Add(1) })
	last := make(chan struct{})
	g.start(func(ctx context.Context) { close(last) })
	close(release)
	select {
	case <-last:
	case <-time.After(2 * time.Second):
		t.Fatal("latest registration never ran")
	}
	if ran.Load() != 0 {
		t.Fatal("a registration replaced before it began still ran")
	}
}

func TestCPUWorkOnlyBeforeListed(t *testing.T) {
	if !cpuWorkForRegistration(false) {
		t.Fatal("an unlisted relay must be allowed CPU work to get on the ledger")
	}
	if cpuWorkForRegistration(true) {
		t.Fatal("a listed relay repricing must not brute-force proof-of-work")
	}
}
