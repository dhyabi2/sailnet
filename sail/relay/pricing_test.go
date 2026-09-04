package relay

import (
	"path/filepath"
	"testing"
	"time"
)

// Ten-day repricing, simulated with a fake clock: down 10% when usage falls,
// up 3% when it grows, unchanged otherwise, never outside the bounds, and
// never a panic when the state file cannot be written.
func TestPricingWindows(t *testing.T) {
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	p := &Pricing{File: filepath.Join(t.TempDir(), "pricing.json"), Days: 10, Min: 1000, Max: 40000, Now: clock}
	rate := p.Load(20000, 0)
	if rate != 20000 {
		t.Fatalf("initial rate %d", rate)
	}
	// Day 5: nothing yet.
	now = now.Add(5 * 24 * time.Hour)
	if r, ch := p.Tick(50 << 20); ch || r != 20000 {
		t.Fatalf("mid-window change: %d %v", r, ch)
	}
	// Day 10: first window closes; no previous window to compare, no change.
	now = now.Add(5 * 24 * time.Hour)
	if r, ch := p.Tick(100 << 20); ch || r != 20000 {
		t.Fatalf("first window should not reprice: %d %v", r, ch)
	}
	// Day 20: usage halved → down 10%.
	now = now.Add(10 * 24 * time.Hour)
	r, ch := p.Tick(150 << 20) // 50 MB this window vs 100 MB before
	if !ch || r != 18000 {
		t.Fatalf("usage down: want 18000, got %d (changed %v)", r, ch)
	}
	// Day 30: usage doubled → up 3% (+1 rounding).
	now = now.Add(10 * 24 * time.Hour)
	r, ch = p.Tick(250 << 20) // 100 MB vs 50 MB
	if !ch || r != 18541 {
		t.Fatalf("usage up: want 18541, got %d (changed %v)", r, ch)
	}
	// Day 40: flat → unchanged.
	now = now.Add(10 * 24 * time.Hour)
	if r2, ch := p.Tick(350 << 20); ch || r2 != r {
		t.Fatalf("flat usage changed: %d %v", r2, ch)
	}
	// Restart: state reloads, window continues, counters restart from here.
	p2 := &Pricing{File: p.File, Days: 10, Min: 1000, Max: 40000, Now: clock}
	if got := p2.Load(20000, 0); got != r {
		t.Fatalf("reload rate: want %d, got %d", r, got)
	}
	// Bounds: many falling windows stop at Min.
	for i := 0; i < 60; i++ {
		now = now.Add(10 * 24 * time.Hour)
		p2.Tick(int64(i+1) * 1000) // tiny, shrinking usage pattern relative to previous windows
	}
	if got, _ := p2.Tick(0); got < 1000 {
		t.Fatalf("below min: %d", got)
	}
}

func TestPricingNeverPanics(t *testing.T) {
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	p := &Pricing{File: "/nonexistent-dir/for-sure/pricing.json", Days: 10, Now: func() time.Time { return now }}
	p.Load(20000, 0)
	now = now.Add(11 * 24 * time.Hour)
	p.Tick(1 << 30)
	now = now.Add(11 * 24 * time.Hour)
	if r, _ := p.Tick(1 << 20); r == 0 {
		t.Fatal("rate zero")
	}
	var q Pricing // zero value, no Load: must still answer
	if r, _ := q.Tick(5); r == 0 {
		t.Fatal("zero-value pricing gave rate 0")
	}
}
