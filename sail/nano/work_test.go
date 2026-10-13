package nano

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

// testThreshold keeps CPU work in tests to a few thousand hashes; production
// thresholds are untouched.
const testThreshold uint64 = 0xfff0000000000000

// neverThreshold is met by no nonce in any useful time: a search for it runs
// until its context ends.
const neverThreshold uint64 = 0xffffffffffffffff

// freshWorkCache points the work cache at an empty file of the test's own,
// so a value remembered by an earlier run (or -count=2) is not found again.
func freshWorkCache(t *testing.T) {
	t.Helper()
	t.Setenv("SAIL_HOME", t.TempDir())
	workCache.Lock()
	workCache.loaded = false
	workCache.m = map[string]string{}
	workCache.Unlock()
	workProgress.Lock()
	workProgress.m = map[workProgressKey]uint64{}
	workProgress.Unlock()
}

// setAllowCPUWork sets the process-wide CPU work mode for one test: true is a
// client, false a relay.
func setAllowCPUWork(t *testing.T, v bool) {
	t.Helper()
	saved := AllowCPUWork
	AllowCPUWork = v
	t.Cleanup(func() { AllowCPUWork = saved })
}

// cpuOnlyAccount returns an account allowed to fall back to CPU work whose
// work service answers (with a rate budget of its own, not DefaultBudget) with a value valid for no root of the tests.
func cpuOnlyAccount(t *testing.T) *Account {
	t.Helper()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"work":"0000000000000000"}`))
	}))
	t.Cleanup(stub.Close)
	key, err := DeriveKey(make([]byte, 32), 0)
	if err != nil {
		t.Fatal(err)
	}
	nc := &Client{URLs: []string{stub.URL}, HTTP: &http.Client{Timeout: 2 * time.Second}, Budget: NewBudget(1000, 1000)}
	return &Account{Key: key, Client: nc, AllowCPU: true}
}

// A relay turns the process-wide CPU fallback off (AllowCPUWork=false): a
// saturated core would stall every circuit through it, and its sends retry.
// Registration is the one send that may not wait forever: a relay that cannot
// register is invisible. An Account with AllowCPU set finds CPU work when no
// work service answers; one without it keeps the strict rule.
//
// The stub answers work_generate with a value that is valid for no root, so
// WorkGenerate reports success while work() must reject it and fall through
// to the CPU question — no live network is involved.
func TestRegistrationAccountMayFallBackToCPUWork(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"work":"0000000000000000"}`))
	}))
	defer stub.Close()

	savedCPU := AllowCPUWork
	freshWorkCache(t)
	AllowCPUWork = false
	defer func() { AllowCPUWork = savedCPU }()

	nc := &Client{URLs: []string{stub.URL}, HTTP: &http.Client{Timeout: 2 * time.Second}, Budget: NewBudget(1000, 1000)}
	key, err := DeriveKey(make([]byte, 32), 0)
	if err != nil {
		t.Fatal(err)
	}
	var root [32]byte
	for i := range root {
		root[i] = byte(i + 1)
	}
	if ValidWork(root, "0000000000000000", testThreshold) {
		t.Fatal("the stub's work happens to be valid for the root; pick another root")
	}

	strict := &Account{Key: key, Client: nc}
	if _, err := strict.work(context.Background(), root, testThreshold); err == nil || !strings.Contains(err.Error(), "proof-of-work service unavailable") {
		t.Fatalf("strict account: want the work-service error, got %v", err)
	}

	allowed := &Account{Key: key, Client: nc, AllowCPU: true}
	w, err := allowed.work(context.Background(), root, testThreshold)
	if err != nil {
		t.Fatalf("allowed account: %v", err)
	}
	if !ValidWork(root, w, testThreshold) {
		t.Fatal("allowed account: CPU work is not valid for the root")
	}
	// The value is cached, so the next send finds the work ready and does
	// not brute-force again.
	if _, ok := CachedWork(root, testThreshold); !ok {
		t.Fatal("CPU work was not remembered in the cache")
	}
}

// The registration send runs under a 20-minute context; CPU work must end with
// it. A context already over starts no search, and one that ends mid-search
// stops it — either way with the context's error, never with work.
func TestCPUWorkEndsWithItsContext(t *testing.T) {
	for _, client := range []bool{false, true} {
		t.Run(map[bool]string{false: "relay", true: "client"}[client], func(t *testing.T) {
			setAllowCPUWork(t, client)
			testCPUWorkEndsWithItsContext(t)
		})
	}
}

func testCPUWorkEndsWithItsContext(t *testing.T) {
	freshWorkCache(t)
	a := cpuOnlyAccount(t)
	var root [32]byte
	root[0] = 0xaa

	cases := []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{"cancelled before", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}, context.Canceled},
		{"deadline mid-search", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 300*time.Millisecond)
		}, context.DeadlineExceeded},
	}
	for _, c := range cases {
		ctx, cancel := c.ctx()
		type result struct {
			w   string
			err error
		}
		done := make(chan result, 1)
		go func() {
			w, err := a.work(ctx, root, neverThreshold)
			done <- result{w, err}
		}()
		select {
		case r := <-done:
			if !errors.Is(r.err, c.want) || r.w != "" {
				t.Errorf("%s: got work %q, err %v; want no work and %v", c.name, r.w, r.err, c.want)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s: CPU work still running 5 s after its context ended", c.name)
		}
		cancel()
	}
}

func progressOf(root [32]byte, threshold uint64) uint64 {
	return loadWorkProgress(workProgressKey{root, threshold})
}

// A search cut off by its context is not wasted: the next call for the same
// root and threshold starts where it stopped, so a root whose solution lies
// further than one deadline's worth of hashing is still found by retrying.
func TestCPUWorkResumesAfterItsDeadline(t *testing.T) {
	for _, client := range []bool{false, true} {
		t.Run(map[bool]string{false: "relay", true: "client"}[client], func(t *testing.T) {
			setAllowCPUWork(t, client)
			freshWorkCache(t)
			a := cpuOnlyAccount(t)
			var root [32]byte
			root[0] = 0xcc

			call := func() {
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				if _, err := a.work(ctx, root, neverThreshold); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("want the deadline, got %v", err)
				}
			}
			call()
			first := progressOf(root, neverThreshold)
			if first <= workStart {
				t.Fatalf("first search recorded no progress (%#x)", first)
			}
			call()
			second := progressOf(root, neverThreshold)
			// Had the second call rescanned from the start, it would have
			// stopped about where the first did; resuming, it got about as
			// far again.
			if second <= first+(first-workStart)/4 {
				t.Fatalf("second search did not resume: progress %#x after the first, %#x after the second", first, second)
			}
			if p := progressOf(root, testThreshold); p != workStart {
				t.Fatalf("progress leaked to another threshold: %#x", p)
			}
		})
	}
}

// A resumed search starts at the recorded position (not at the beginning),
// and finding work drops the position and caches the work.
func TestCPUWorkStartsAtTheRecordedPosition(t *testing.T) {
	setAllowCPUWork(t, false)
	freshWorkCache(t)
	a := cpuOnlyAccount(t)
	var root [32]byte
	root[0] = 0xdd

	// The first solution from the start, which a rescan would return.
	w0, err := GenerateWorkCPU(context.Background(), root, testThreshold)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := hex.DecodeString(w0)
	first := binary.BigEndian.Uint64(b)
	saveWorkProgress(workProgressKey{root, testThreshold}, first+1)

	w, err := a.work(context.Background(), root, testThreshold)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = hex.DecodeString(w)
	if n := binary.BigEndian.Uint64(b); n <= first || !ValidWork(root, w, testThreshold) {
		t.Fatalf("got nonce %#x (valid %v); want valid work past the recorded position %#x", n, ValidWork(root, w, testThreshold), first+1)
	}
	if p := progressOf(root, testThreshold); p != workStart {
		t.Fatalf("position not dropped once work was found: %#x", p)
	}
	if got, ok := CachedWork(root, testThreshold); !ok || got != w {
		t.Fatal("found work was not remembered in the cache")
	}
}

// Clients keep main's speed: their CPU work uses every core and does not wait
// for the relay's one-search slot.
func TestClientCPUWorkIsNotSerialised(t *testing.T) {
	setAllowCPUWork(t, true)
	freshWorkCache(t)
	a := cpuOnlyAccount(t)
	if got := cpuWorkers(); got != runtime.NumCPU() {
		t.Fatalf("client CPU work uses %d workers, want %d", got, runtime.NumCPU())
	}

	cpuWorkSlot <- struct{}{} // a relay-mode search holds the slot
	defer func() { <-cpuWorkSlot }()
	var root [32]byte
	root[0] = 0xee
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w, err := a.work(ctx, root, testThreshold)
	if err != nil || !ValidWork(root, w, testThreshold) {
		t.Fatalf("client CPU work queued behind the relay slot: work %q, err %v", w, err)
	}
}

func TestRelayCPUWorkUsesOneWorker(t *testing.T) {
	setAllowCPUWork(t, false)
	if got := cpuWorkers(); got != 1 {
		t.Fatalf("relay CPU work uses %d workers, want 1", got)
	}
}
