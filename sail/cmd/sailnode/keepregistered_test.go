package main

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dhyabi2/sail/nano"
	"github.com/dhyabi2/sail/relay"
	"github.com/dhyabi2/sail/token"
)

const testTreasury = "treasury"

// fakeLedger stands in for the ledger and the registry sends, recording
// whether each send was allowed CPU proof-of-work.
type fakeLedger struct {
	mu    sync.Mutex
	st    *token.State
	addr  string
	cpu   []bool                  // AllowCPU of each send, in order
	fail  func(op token.Op) error // nil or an error to refuse this send with
	block bool                    // ledger reads wait for ctx to end
}

func newFakeLedger(t *testing.T, addr string) *fakeLedger {
	f := &fakeLedger{st: token.NewState(testTreasury), addr: addr}
	savedLedger, savedSend, savedRetry := ledgerRelay, sendOp, registerRetry
	t.Cleanup(func() { ledgerRelay, sendOp, registerRetry = savedLedger, savedSend, savedRetry })
	registerRetry = time.Millisecond
	ledgerRelay = func(ctx context.Context, _ *nano.Client, a string) (*token.Relay, error) {
		f.mu.Lock()
		block := f.block
		f.mu.Unlock()
		if block {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		r := f.st.Relays[a]
		if r == nil {
			return nil, nil
		}
		c := *r
		return &c, nil
	}
	sendOp = func(ctx context.Context, acct *nano.Account, rep *[32]byte) (string, error) {
		op, err := token.Decode(*rep)
		if err != nil {
			return "", err
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.cpu = append(f.cpu, acct.AllowCPU)
		if f.fail != nil {
			if err := f.fail(op); err != nil {
				return "", err
			}
		}
		return "hash", f.st.Apply(&token.Event{Op: op, Sender: acct.Key.Address, Recipient: testTreasury})
	}
	return f
}

// publish puts a record on the fake ledger directly (an earlier run).
func (f *fakeLedger) publish(t *testing.T, ops ...token.Op) {
	t.Helper()
	for _, op := range ops {
		if err := f.st.Apply(&token.Event{Op: op, Sender: f.addr, Recipient: testTreasury}); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *fakeLedger) sends() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.cpu...)
}

func regOp(cc string, asn, rate uint32, flags uint16) token.Op {
	return token.Op{Code: token.OpRegister, Aux: token.RegisterAux(cc, asn, rate, flags)}
}

func descOp(d relay.Descriptor) token.Op {
	return token.Op{Code: token.OpDescriptor, Aux: d.Encode()}
}

var (
	testKey   = &nano.Key{Address: "relay-under-test"}
	testDesc  = relay.Descriptor{IP: net.IPv4(192, 0, 2, 7), Port: 443, CertFP: [6]byte{1, 2, 3, 4, 5, 6}}
	testFlags = uint16(token.FlagPublic)
)

// runRegistration runs one keepRegistered to completion (bounded by the test).
func runRegistration(t *testing.T, listed *atomic.Bool, cc string, rate uint32, desc relay.Descriptor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	keepRegistered(ctx, listed, nil, testKey, cc, 64512, rate, testFlags, desc)
	if ctx.Err() != nil {
		t.Fatal("registration did not finish")
	}
}

func wantAllCPU(t *testing.T, f *fakeLedger, want bool) {
	t.Helper()
	got := f.sends()
	if len(got) == 0 {
		t.Fatal("no registration was sent")
	}
	for i, c := range got {
		if c != want {
			t.Fatalf("send %d: AllowCPU=%v, want %v (all sends: %v)", i, c, want, got)
		}
	}
}

func wantExact(t *testing.T, f *fakeLedger, cc string, rate uint32, desc relay.Descriptor) {
	t.Helper()
	r := f.st.Relays[testKey.Address]
	if r == nil || r.Country != cc || r.MinRate != rate || r.Descriptor != desc.Encode() {
		t.Fatalf("ledger record %+v is not the one published", r)
	}
}

// A fresh relay whose DESCRIPTOR send fails after REGISTER went out is on the
// ledger with no descriptor: it is not listed yet, and the retry must still
// be allowed CPU work or it never becomes reachable.
func TestRegistrationRetryAfterDescriptorFailsKeepsCPU(t *testing.T) {
	f := newFakeLedger(t, testKey.Address)
	failed := false
	f.fail = func(op token.Op) error {
		if op.Code == token.OpDescriptor && !failed {
			failed = true
			return errors.New("work timed out")
		}
		return nil
	}
	var listed atomic.Bool
	runRegistration(t, &listed, "DE", 1000, testDesc)
	if !failed {
		t.Fatal("the DESCRIPTOR failure never happened")
	}
	wantAllCPU(t, f, true)
	wantExact(t, f, "DE", 1000, testDesc)
}

// A reprice that cancelled a fresh relay's registration between REGISTER and
// DESCRIPTOR leaves a REGISTER alone on the ledger: still not listed.
func TestRegistrationRegisterOnlyRecordKeepsCPU(t *testing.T) {
	f := newFakeLedger(t, testKey.Address)
	f.publish(t, regOp("DE", 64512, 1000, testFlags))
	var listed atomic.Bool
	runRegistration(t, &listed, "DE", 2000, testDesc)
	wantAllCPU(t, f, true)
	wantExact(t, f, "DE", 2000, testDesc)
}

// A listed relay restarting with a changed descriptor or country must be
// allowed CPU work to correct its record, as before CPU work was bounded.
func TestRegistrationChangedRecordGetsCPU(t *testing.T) {
	newDesc := testDesc
	newDesc.CertFP = [6]byte{9, 9, 9, 9, 9, 9}
	for _, tc := range []struct {
		name string
		cc   string
		desc relay.Descriptor
	}{
		{"descriptor", "DE", newDesc},
		{"country", "FR", testDesc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeLedger(t, testKey.Address)
			f.publish(t, regOp("DE", 64512, 1000, testFlags), descOp(testDesc))
			var listed atomic.Bool
			runRegistration(t, &listed, tc.cc, 1000, tc.desc)
			wantAllCPU(t, f, true)
			wantExact(t, f, tc.cc, 1000, tc.desc)
		})
	}
}

// A listed relay whose record differs only in price keeps the strict rule:
// no CPU proof-of-work (keepRegistered must use cpuWorkForRegistration).
func TestRegistrationRepriceOfListedRelayNoCPU(t *testing.T) {
	f := newFakeLedger(t, testKey.Address)
	f.publish(t, regOp("DE", 64512, 1000, testFlags), descOp(testDesc))
	var listed atomic.Bool
	runRegistration(t, &listed, "DE", 2000, testDesc)
	wantAllCPU(t, f, false)
	if !listed.Load() {
		t.Fatal("a relay on the ledger at another price is listed")
	}
	if r := f.st.Relays[testKey.Address]; r.MinRate != 2000 {
		t.Fatalf("price on the ledger is %d, want 2000", r.MinRate)
	}
}

// A registration cancelled while it reads the ledger returns promptly, so
// the next one (a newer price) is not held up by the stale ledger read.
func TestRegistrationCancelledDuringLedgerRead(t *testing.T) {
	f := newFakeLedger(t, testKey.Address)
	f.block = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		var listed atomic.Bool
		keepRegistered(ctx, &listed, nil, testKey, "DE", 64512, 1000, testFlags, testDesc)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled registration still reading the ledger")
	}
	if n := len(f.sends()); n != 0 {
		t.Fatalf("cancelled registration sent %d blocks", n)
	}
}
