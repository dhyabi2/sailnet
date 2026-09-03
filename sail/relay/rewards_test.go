package relay

import (
	"math/big"
	"testing"

	"github.com/dhyabi2/sail/nano"
)

func raw(xno float64) *big.Int {
	f := new(big.Float).Mul(big.NewFloat(xno), new(big.Float).SetInt(rawPerXNO))
	i, _ := f.Int(nil)
	return i
}

func row(acct string, earned float64, age int, perf float64) *EpochRelay {
	r := &EpochRelay{Account: acct, Earned: raw(earned), LevyOwed: raw(earned * LevyPercent / 100), Age: age, Perf: perf, Paid: new(big.Int), Payers: map[string]*big.Int{}, PaidTo: map[string]*big.Int{}, Eligible: true}
	return r
}

func received(t *EpochTable, to string) *big.Int {
	got := new(big.Int)
	for _, m := range t.Payouts {
		if v := m[to]; v != nil {
			got.Add(got, v)
		}
	}
	return got
}

func TestPayoutSplitAndCap(t *testing.T) {
	// A: big earner. B: old, little traffic. C: new, busy.
	tb := &EpochTable{Epoch: 5, Relays: map[string]*EpochRelay{}, Payouts: map[string]map[string]*big.Int{}}
	tb.Relays["A"] = row("A", 1.0, 10, 1)
	tb.Relays["B"] = row("B", 0.5, 100, 0)
	tb.Relays["C"] = row("C", 0.5, 1, 3)
	tb.computePayouts()
	if _, self := tb.Payouts["A"]["A"]; self {
		t.Fatal("paid itself")
	}
	// Nobody receives more than 80 % of their own levy.
	for acct, r := range tb.Relays {
		capRaw := new(big.Int).Mul(r.LevyOwed, big.NewInt(RewardCapPercent))
		capRaw.Div(capRaw, big.NewInt(100))
		if got := received(tb, acct); got.Cmp(capRaw) > 0 {
			t.Fatalf("%s received %.6f > cap %.6f", acct, xno(got), xno(capRaw))
		}
	}
	// B is capped at 0.04 (80 % of its 0.05 levy) even though its age share is larger.
	if got := xno(received(tb, "B")); got < 0.0399 || got > 0.0401 {
		t.Fatalf("B got %.6f, want 0.04", got)
	}
	// C (age 1, perf 3) would get more than its cap from A and B together; it is scaled to exactly its cap.
	if got := xno(received(tb, "C")); got < 0.0399 || got > 0.0401 {
		t.Fatalf("C received %.6f, want 0.04", got)
	}

	// Compliance is matched per recipient: a levy send to an unrelated wallet counts for nothing.
	a := tb.Relays["A"]
	a.PaidTo["X"] = raw(1)
	tb.computePayouts()
	if tb.Compliant("A") {
		t.Fatal("paying a stranger should not count as levy")
	}
	// Paying the owed shares does.
	for to, v := range tb.Payouts["A"] {
		a.PaidTo[to] = new(big.Int).Set(v)
	}
	tb.computePayouts()
	if !tb.Compliant("A") {
		t.Fatalf("A paid every share and is still non-compliant: paid %.6f", xno(a.Paid))
	}

	// Reward op round trip.
	rep := RewardOp(12345)
	if e, ok := rewardEpoch(nano.PubkeyToAddress(rep)); !ok || e != 12345 {
		t.Fatalf("reward op round trip: %v %d", ok, e)
	}
}

func TestWashTrafficLosesMoney(t *testing.T) {
	// An attacker owns relays P and Q and washes 10 XNO through them; honest H earns 1 XNO.
	// Whatever the split, the attacker's relays can receive at most 80 % of their own levy
	// plus what honest levies send them, and the honest levy is bounded by H's earnings.
	tb := &EpochTable{Epoch: 9, Relays: map[string]*EpochRelay{}, Payouts: map[string]map[string]*big.Int{}}
	tb.Relays["P"] = row("P", 10, 180, 100)
	tb.Relays["Q"] = row("Q", 10, 180, 100)
	tb.Relays["H"] = row("H", 1, 5, 1)
	tb.computePayouts()
	paidOut := new(big.Int).Add(tb.Relays["P"].LevyOwed, tb.Relays["Q"].LevyOwed)
	gotBack := new(big.Int).Add(received(tb, "P"), received(tb, "Q"))
	// received ≤ 80 % of own levy each → attacker nets at most −20 % of its levy plus H's tiny levy
	maxBack := new(big.Int).Mul(paidOut, big.NewInt(RewardCapPercent))
	maxBack.Div(maxBack, big.NewInt(100))
	if gotBack.Cmp(maxBack) > 0 {
		t.Fatalf("attacker got back %.6f > %.6f", xno(gotBack), xno(maxBack))
	}
	if xno(gotBack) >= xno(paidOut) {
		t.Fatalf("wash traffic must lose money: paid %.6f got %.6f", xno(paidOut), xno(gotBack))
	}
}
