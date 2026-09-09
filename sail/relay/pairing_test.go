package relay

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dhyabi2/sail/nano"
	"github.com/dhyabi2/sail/wire"
)

func TestPairingCodeIsSingleUseShortLivedAndGuessResistant(t *testing.T) {
	dir := t.TempDir()
	relayKey, phone := testKey(t, 1), testKey(t, 2)
	s := ownerServer(t, relayKey, [32]byte{})
	s.OwnersFile, s.PairingFile = filepath.Join(dir, "owners.json"), filepath.Join(dir, "pairing.json")
	phoneHex := hex.EncodeToString(phone.Public[:])

	if err := s.tryPair(phoneHex, "123456"); err == nil {
		t.Fatal("no code minted: must refuse")
	}
	code, _, err := NewPairingCode(s.PairingFile)
	if err != nil || len(code) != 6 {
		t.Fatalf("code %q err %v", code, err)
	}
	for i := 0; i < 2; i++ {
		if err := s.tryPair(phoneHex, "000000"); err == nil && code != "000000" {
			t.Fatal("a wrong code must be refused")
		}
	}
	if err := s.tryPair(phoneHex, code[:3]+" "+code[3:]); err != nil { // spaces as printed are fine
		t.Fatalf("the right code must pair: %v", err)
	}
	if err := s.tryPair(phoneHex, code); err == nil {
		t.Fatal("a code works once")
	}
	if _, ok := s.ownerFor(OwnerTag(relayKey.Public, phone.Public)); !ok {
		t.Fatal("the paying wallet should now be an owner")
	}
	// Persisted: a fresh Server reads it back.
	again := &Server{Key: relayKey, OwnersFile: s.OwnersFile}
	if again.LoadOwners() != 1 {
		t.Fatal("owners.json should hold the paired wallet")
	}
	// Three wrong guesses void a code.
	code, _, _ = NewPairingCode(s.PairingFile)
	for i := 0; i < 3; i++ {
		s.tryPair(phoneHex, "999999")
	}
	if err := s.tryPair(phoneHex, code); err == nil {
		t.Fatal("after three wrong guesses even the right code must be void")
	}
}

func TestPairOverTheWireThenRideFree(t *testing.T) {
	reg := &Registry{}
	s, ri, _ := startRelay(t, reg, 5, true)
	dir := t.TempDir()
	s.OwnersFile, s.PairingFile = filepath.Join(dir, "owners.json"), filepath.Join(dir, "pairing.json")
	seed := make([]byte, 32)
	seed[0] = 66
	phone, _ := nano.DeriveKey(seed, 0)

	// The phone pays an ordinary anchor: the relay credits it to the phone's key.
	var anchor [32]byte
	copy(anchor[:], "phone-paid-this-anchor-normally!")
	s.Quota.Credit(hex.EncodeToString(anchor[:]), 5<<20, hex.EncodeToString(phone.Public[:]))
	code, _, _ := NewPairingCode(s.PairingFile)

	c, err := Build([]*RelayInfo{ri}, anchor, 10*time.Second, nil, func(pub, tg [32]byte) []byte { return SignCreate(phone, pub, tg) })
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Pair("000000", 5*time.Second); err == nil && code != "000000" {
		t.Fatal("a wrong code must be refused over the wire")
	}
	if err := c.Pair(code, 5*time.Second); err != nil {
		t.Fatalf("pairing with the right code failed: %v", err)
	}
	c.Close()

	// Now the owner tag opens a circuit with nothing credited: Direct, 1 hop, free.
	tag := OwnerTag(ri.Pub, phone.Public)
	c2, err := Build([]*RelayInfo{ri}, tag, 10*time.Second, nil, func(pub, tg [32]byte) []byte { return SignCreate(phone, pub, tg) })
	if err != nil {
		t.Fatalf("the paired phone should ride free: %v", err)
	}
	defer c2.Close()
	if bad := c2.Ping(5 * time.Second); bad != -1 {
		t.Fatal("ping failed on the free circuit")
	}
	_ = wire.CmdPair
}

// A phone with nothing paid opens a pairing circuit while a code is active,
// hands over the code, and rides free from then on. Without a code the same
// tag is refused, and the tag of another wallet never opens anything.
func TestPairWithoutPayingAnAnchor(t *testing.T) {
	reg := &Registry{}
	s, ri, _ := startRelay(t, reg, 5, true)
	dir := t.TempDir()
	s.OwnersFile, s.PairingFile = filepath.Join(dir, "owners.json"), filepath.Join(dir, "pairing.json")
	seed := make([]byte, 32)
	seed[0] = 67
	phone, _ := nano.DeriveKey(seed, 0)
	sign := func(pub, tg [32]byte) []byte { return SignCreate(phone, pub, tg) }
	ptag := PairingTag(ri.Pub, phone.Public)

	if c, err := Build([]*RelayInfo{ri}, ptag, 5*time.Second, PairingCreate(phone.Public), sign); err == nil {
		c.Close()
		t.Fatal("no code is active: the pairing tag must be refused")
	}
	code, _, _ := NewPairingCode(s.PairingFile)
	seed[0] = 68
	other, _ := nano.DeriveKey(seed, 0)
	if c, err := Build([]*RelayInfo{ri}, ptag, 5*time.Second, PairingCreate(phone.Public), func(pub, tg [32]byte) []byte { return SignCreate(other, pub, tg) }); err == nil {
		c.Close()
		t.Fatal("another wallet signing the phone's pairing tag must be refused")
	}
	c, err := Build([]*RelayInfo{ri}, ptag, 10*time.Second, PairingCreate(phone.Public), sign)
	if err != nil {
		t.Fatalf("the pairing circuit should open free while a code is active: %v", err)
	}
	if err := c.Pair(code, 5*time.Second); err != nil {
		t.Fatalf("pairing over the free circuit failed: %v", err)
	}
	c.Close()
	if _, err := os.Stat(s.PairingFile); err == nil {
		t.Fatal("a used code must be gone")
	}
	c2, err := Build([]*RelayInfo{ri}, OwnerTag(ri.Pub, phone.Public), 10*time.Second, nil, sign)
	if err != nil {
		t.Fatalf("the paired phone should ride free: %v", err)
	}
	defer c2.Close()
	if bad := c2.Ping(5 * time.Second); bad != -1 {
		t.Fatal("ping failed on the free circuit")
	}
}
