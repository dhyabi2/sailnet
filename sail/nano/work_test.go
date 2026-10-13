package nano

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
}

// cpuOnlyAccount returns an account allowed to fall back to CPU work whose
// work service answers with a value valid for no root of the tests.
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
	nc := &Client{URLs: []string{stub.URL}, HTTP: &http.Client{Timeout: 2 * time.Second}}
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

	nc := &Client{URLs: []string{stub.URL}, HTTP: &http.Client{Timeout: 2 * time.Second}}
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
