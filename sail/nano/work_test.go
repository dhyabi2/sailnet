package nano

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
	t.Setenv("SAIL_HOME", t.TempDir())
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

	strict := &Account{Key: key, Client: nc}
	if _, err := strict.work(context.Background(), root, SendThreshold); err == nil || !strings.Contains(err.Error(), "proof-of-work service unavailable") {
		t.Fatalf("strict account: want the work-service error, got %v", err)
	}

	allowed := &Account{Key: key, Client: nc, AllowCPU: true}
	w, err := allowed.work(context.Background(), root, SendThreshold)
	if err != nil {
		t.Fatalf("allowed account: %v", err)
	}
	if !ValidWork(root, w, SendThreshold) {
		t.Fatal("allowed account: CPU work is not valid for the root")
	}
	// The value is cached, so the next send finds the work ready and does
	// not brute-force again.
	if _, ok := CachedWork(root, SendThreshold); !ok {
		t.Fatal("CPU work was not remembered in the cache")
	}
}
