package relay

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dhyabi2/sail/nano"
)

// A faucet with both grants configured, no network behind it. Every test
// here is about who gets turned away, and a refusal is decided before any
// XNO moves, so the wallets never need to be funded or reachable.
func testFaucet(t *testing.T) *Faucet {
	t.Helper()
	key := func() *nano.Key {
		seed, err := nano.NewSeed()
		if err != nil {
			t.Fatal(err)
		}
		k, err := nano.DeriveKey(seed, 0)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	return &Faucet{
		Key: key(), Amount: big.NewInt(500), PerIP: 2,
		TrialKey: key(), TrialAmount: big.NewInt(1000), TrialPerIP: 2,
		File: filepath.Join(t.TempDir(), "faucet-state.json"),
	}
}

func claim(t *testing.T, f *Faucet, ip, account string, trial bool) (int, faucetReply) {
	t.Helper()
	body := `{"account":"` + account + `","kind":"registration"}`
	if trial {
		body = `{"account":"` + account + `","kind":"trial"}`
	}
	r := httptest.NewRequest(http.MethodPost, "/faucet", strings.NewReader(body))
	r.RemoteAddr = ip + ":40000"
	w := httptest.NewRecorder()
	f.ServeHTTP(w, r)
	var rep faucetReply
	json.Unmarshal(w.Body.Bytes(), &rep)
	return w.Code, rep
}

func someAccount(t *testing.T) string {
	t.Helper()
	seed, _ := nano.NewSeed()
	k, err := nano.DeriveKey(seed, 0)
	if err != nil {
		t.Fatal(err)
	}
	return k.Address
}

// The two faucets are independent. Somebody whose address has taken its node
// registrations must still be able to open an app and get a free trial: they
// are separate wallets, separate counters and separate limits, and one being
// spent is not a reason to refuse the other.
//
// It used to be: the first check compared the registration counter against
// the registration limit whatever was being asked for, so a busy address was
// refused a trial it had never claimed.
func TestTheTwoFaucetsDoNotThrottleEachOther(t *testing.T) {
	f := testFaucet(t)
	f.load()
	// An address that has spent its registration allowance for the day.
	f.st.Day = time.Now().UTC().Format("2006-01-02")
	f.st.IPs["10.0.0.7"] = f.PerIP

	if code, rep := claim(t, f, "10.0.0.7", someAccount(t), false); code != 429 {
		t.Fatalf("a registration claim past the limit should be refused, got HTTP %d (%s)", code, rep.Error)
	}
	// The trial has its own counter and has not been touched. It must get
	// past the limit checks; with no ledger behind it the send then fails,
	// which is a 503 — anything but a 429 proves it was not throttled.
	code, rep := claim(t, f, "10.0.0.7", someAccount(t), true)
	if code == 429 {
		t.Fatalf("the free trial was refused because of the registration counter: %s", rep.Error)
	}
}

// Each faucet still enforces its own per-address limit.
func TestEachFaucetKeepsItsOwnLimit(t *testing.T) {
	f := testFaucet(t)
	f.load()
	f.st.Day = time.Now().UTC().Format("2006-01-02")
	f.st.IPs["trial|10.0.0.8"] = f.TrialPerIP

	code, rep := claim(t, f, "10.0.0.8", someAccount(t), true)
	if code != 429 {
		t.Fatalf("a trial past the limit should be refused, got HTTP %d", code)
	}
	if !strings.Contains(rep.Error, "free trials") {
		t.Fatalf("the refusal should say what ran out, got %q", rep.Error)
	}
	// And it must say how much the wallet needs, so an app can tell its user.
	if rep.Amount == "" {
		t.Error("a refused trial should still report the amount a wallet needs")
	}
}

// One wallet, one grant per day, whichever faucet it asks.
func TestAWalletCannotClaimTwiceInADay(t *testing.T) {
	f := testFaucet(t)
	f.load()
	acct := someAccount(t)
	f.st.Day = time.Now().UTC().Format("2006-01-02")
	f.st.Accounts[acct] = time.Now().Unix()

	for _, trial := range []bool{false, true} {
		code, rep := claim(t, f, "10.0.0.9", acct, trial)
		if code != 429 {
			t.Fatalf("trial=%v: a wallet that already claimed should be refused, got HTTP %d", trial, code)
		}
		if rep.RetryAfterS <= 0 {
			t.Errorf("trial=%v: a refusal should say when to come back", trial)
		}
	}
}

// Nothing a caller sends should be taken as the address to pay.
func TestFaucetRejectsRubbishAccounts(t *testing.T) {
	f := testFaucet(t)
	for _, a := range []string{"", "   ", "nano_", "nano_notanaddress", "xrb_1111", "../etc/passwd", strings.Repeat("A", 200)} {
		if code, _ := claim(t, f, "10.0.0.10", a, true); code != 400 {
			t.Errorf("%q was not rejected: HTTP %d", a, code)
		}
	}
}

// A forwarded address is only believed when the forwarder proves itself.
// Without that, every user behind the proxy shares one counter, and the
// third person to open the app is told the network address is out of trials.
func TestForwardedAddressIsOnlyBelievedWithTheSecret(t *testing.T) {
	f := testFaucet(t)
	f.Secret = "shared-with-the-proxy"

	r := httptest.NewRequest(http.MethodPost, "/faucet", strings.NewReader(`{"account":"x"}`))
	r.RemoteAddr = "203.0.113.9:1234" // the proxy
	r.Header.Set("X-Forwarded-For", "198.51.100.4")
	if got := f.clientIP(r); got != "203.0.113.9" {
		t.Fatalf("an unproven forwarded address was believed: %s", got)
	}
	r.Header.Set("X-Faucet-Secret", "wrong")
	if got := f.clientIP(r); got != "203.0.113.9" {
		t.Fatalf("a wrong secret was accepted: %s", got)
	}
	r.Header.Set("X-Faucet-Secret", f.Secret)
	if got := f.clientIP(r); got != "198.51.100.4" {
		t.Fatalf("the proven forwarded address was ignored: %s", got)
	}
}

// The counters are what an operator reads instead of the log. A faucet
// nobody asked and a faucet turning everyone away look identical from the
// outside, so both numbers are kept, and both reset with the day.
func TestFaucetCountersReportGrantsAndRefusals(t *testing.T) {
	f := testFaucet(t)
	f.load()
	today := time.Now().UTC().Format("2006-01-02")
	f.st.Day = today
	f.st.IPs["trial|10.0.0.11"] = f.TrialPerIP // allowance already spent

	if paid, refusedCount, _ := f.Counters(); paid != 0 || refusedCount != 0 {
		t.Fatalf("a fresh faucet should report nothing: %d paid, %d refused", paid, refusedCount)
	}
	for i := 0; i < 3; i++ {
		if code, _ := claim(t, f, "10.0.0.11", someAccount(t), true); code != 429 {
			t.Fatalf("claim %d should have been refused, got HTTP %d", i, code)
		}
	}
	paid, refusedCount, lastPaid := f.Counters()
	if paid != 0 || refusedCount != 3 {
		t.Fatalf("want 0 paid and 3 refused, got %d and %d", paid, refusedCount)
	}
	if lastPaid != 0 {
		t.Errorf("nothing was paid, so there should be no last-paid time")
	}

	// Yesterday's numbers must not linger into a day that has not started.
	f.mu.Lock()
	f.st.Day = "2000-01-01"
	f.st.Paid, f.st.Refused = 9, 9
	f.mu.Unlock()
	if paid, refusedCount, _ := f.Counters(); paid != 0 || refusedCount != 0 {
		t.Fatalf("yesterday's counts leaked into today: %d paid, %d refused", paid, refusedCount)
	}
}
