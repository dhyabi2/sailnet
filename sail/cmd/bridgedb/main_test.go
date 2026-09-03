package main

import (
	"path/filepath"
	"testing"
	"time"
)

const l1 = "sail-bridge:nano_363r5j7hp1b8qisbqjkiwkpgbp4w3i9rzfikro5j1qeaw6gr6h3f3xbjqgu1:2.24.73.29:443:12f1d7d895f2:a.example:00112233445566778899aabbccddeeff"
const l2 = "sail-bridge:nano_136uituimqkb4un6q68cz7ggbdti61z1iwpb5bsw53j8ypqnhqjq7p3yumf5:72.61.148.4:8443:28455d4618d5:b.example:ffeeddccbbaa99887766554433221100"

func TestRedeemAndRetire(t *testing.T) {
	st := load(filepath.Join(t.TempDir(), "s.json"))
	for _, l := range []string{l1, l2} {
		st.Bridges[l[12:12+65]] = &Bridge{Line: l, Account: l[12 : 12+65], Added: time.Now()}
	}
	st.fill(1)
	codes := []string{"c1", "c2", "c3"}
	for _, c := range codes {
		st.Invites[c] = &Invite{Code: c, Bucket: 0, Uses: 5, Bridges: 1, Created: time.Now()}
	}
	got, err := st.redeem("c1")
	if err != nil || len(got) != 1 {
		t.Fatalf("redeem: %v %v", got, err)
	}
	acct := got[0][12 : 12+65]
	if _, err := st.redeem("c1"); err == nil {
		t.Fatal("second redeem within 10 s should be rate-limited")
	}
	if _, err := st.redeem("nope"); err == nil {
		t.Fatal("unknown code accepted")
	}
	// three distinct invites in bucket 0 report the bridge → retired, bucket refilled with the other bridge
	for _, c := range codes {
		if err := st.report(c, acct); err != nil {
			t.Fatal(err)
		}
	}
	if !st.Bridges[acct].Burned {
		t.Fatal("bridge should be retired after 3 reports")
	}
	st.Invites["c2"].LastUse = time.Time{}
	got2, err := st.redeem("c2")
	if err != nil || len(got2) != 1 || got2[0] == got[0] {
		t.Fatalf("bucket should be refilled with a different bridge: %v %v", got2, err)
	}
	// an invite from another bucket cannot report a bridge it never received
	st.Invites["x"] = &Invite{Code: "x", Bucket: 3, Uses: 1, Bridges: 1}
	st.Buckets[3] = nil
	if err := st.report("x", got2[0][12:12+65]); err == nil {
		t.Fatal("cross-bucket report should be refused")
	}
}
