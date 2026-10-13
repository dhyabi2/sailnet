//go:build unix

package nano

import (
	"context"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
)

func cpuTime(t *testing.T) time.Duration {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Skip("getrusage:", err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// A live relay may reach CPU work from several sends at once (REGISTER and
// DESCRIPTOR, a repricing that outlived the next one). Each used to take
// every core; together they must still take about one: over a window of
// wall time the process may spend little more than that window in CPU.
func TestCPUWorkTakesOneCoreProcessWide(t *testing.T) {
	if runtime.NumCPU() < 2 {
		t.Skip("one core: nothing to tell apart")
	}
	freshWorkCache(t)
	a := cpuOnlyAccount(t)

	const window = 1500 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		var root [32]byte
		root[0] = byte(0xb0 + i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.work(ctx, root, neverThreshold)
		}()
	}
	time.Sleep(200 * time.Millisecond) // past the work-service call, into the search
	before, start := cpuTime(t), time.Now()
	time.Sleep(window)
	used, wall := cpuTime(t)-before, time.Since(start)
	cancel()
	stopped := make(chan struct{})
	go func() { wg.Wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Error("CPU searches still running 5 s after their context ended")
	}
	if used > wall*3/2 {
		t.Fatalf("three concurrent CPU searches used %v of CPU in %v of wall time (%.1f cores); want at most about one",
			used, wall, float64(used)/float64(wall))
	}
	t.Logf("%v of CPU in %v of wall time", used, wall)
}
