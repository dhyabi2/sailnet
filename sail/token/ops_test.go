package token

import (
	"testing"
)

func TestEncodeDecode(t *testing.T) {
	op := Op{Code: OpRegister, Aux: RegisterAux("NL", 14061, 500_000, FlagPublic|FlagExit)}
	rep, err := Encode(op)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(rep)
	if err != nil {
		t.Fatal(err)
	}
	cc, asn, rate, flags := ParseRegisterAux(got.Aux)
	if cc != "NL" || asn != 14061 || rate != 500_000 || flags != FlagPublic|FlagExit {
		t.Fatalf("aux mismatch: %s %d %d %d", cc, asn, rate, flags)
	}
	var plain [32]byte
	if _, err := Decode(plain); err != ErrNotSail {
		t.Fatalf("expected ErrNotSail")
	}
}

func TestRegistry(t *testing.T) {
	s := NewState("treasury")
	reg := &Event{Op: Op{Code: OpRegister, Aux: RegisterAux("SG", 14061, 500_000, FlagPublic|FlagExit)}, Sender: "relay", Recipient: "treasury", SendHeight: 3}
	if err := s.Apply(reg); err != nil {
		t.Fatal(err)
	}
	var d [12]byte
	copy(d[:], "descriptor!!")
	desc := &Event{Op: Op{Code: OpDescriptor, Aux: d}, Sender: "relay", Recipient: "treasury", SendHeight: 4}
	if err := s.Apply(desc); err != nil {
		t.Fatal(err)
	}
	if s.Relays["relay"].Country != "SG" || s.Relays["relay"].Descriptor != d {
		t.Fatal("registry state wrong")
	}
	if s.Apply(&Event{Op: Op{Code: OpRegister}, Sender: "x", Recipient: "someone-else"}) == nil {
		t.Fatal("REGISTER to a non-anchor account must be rejected")
	}
	if s.Root() != s.Root() {
		t.Fatal("root unstable")
	}
}
