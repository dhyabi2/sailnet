package relay

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// setPairingUntil stamps the active pairing code with the time the access it
// was minted for ends, as `sailnode pair --until` writes it.
func setPairingUntil(t *testing.T, path string, until int64) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["owner_expires"] = until
	raw, _ = json.Marshal(m)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A wallet paired with a code that carries an end time rides free until
// then and not a second longer: the relay refuses its owner tag, empties
// what the tag was credited, and does so again after a restart. Owners
// written without an end time stay owners.
func TestPairedOwnerExpiresOnTheRelay(t *testing.T) {
	dir := t.TempDir()
	relayKey, phone, old := testKey(t, 1), testKey(t, 2), testKey(t, 3)
	s := ownerServer(t, relayKey, [32]byte{})
	s.OwnersFile, s.PairingFile = filepath.Join(dir, "owners.json"), filepath.Join(dir, "pairing.json")

	// owners.json as relays before expiry wrote it: a plain list, permanent.
	legacy, _ := json.Marshal(map[string][]string{"owners": {hex.EncodeToString(old.Public[:])}})
	if err := os.WriteFile(s.OwnersFile, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if s.LoadOwners() != 1 {
		t.Fatal("a legacy owners.json must load")
	}

	code, _, err := NewPairingCode(s.PairingFile)
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Unix() + 2
	setPairingUntil(t, s.PairingFile, until)
	if err := s.tryPair(hex.EncodeToString(phone.Public[:]), code); err != nil {
		t.Fatalf("pairing: %v", err)
	}
	tag := OwnerTag(relayKey.Public, phone.Public)
	clientPub, sig, tagHex := createFrom(phone, tag)
	if !s.ownerCreate(tagHex, tag, clientPub, sig) {
		t.Fatal("before its end time the paired wallet rides free")
	}

	for time.Now().Unix() < until {
		time.Sleep(100 * time.Millisecond)
	}
	if s.ownerCreate(tagHex, tag, clientPub, sig) {
		t.Fatal("after its end time the paired wallet must not ride free")
	}
	if rem := s.Quota.Remaining(tagHex); rem > 0 {
		t.Fatalf("an expired owner tag must keep nothing to spend, has %d bytes", rem)
	}
	again := ownerServer(t, relayKey, [32]byte{})
	again.OwnersFile = s.OwnersFile
	again.LoadOwners()
	if _, ok := again.ownerFor(tag); ok {
		t.Fatal("the end time must survive a restart")
	}

	oldTag := OwnerTag(relayKey.Public, old.Public)
	oc, osig, oHex := createFrom(old, oldTag)
	if !s.ownerCreate(oHex, oldTag, oc, osig) || !again.ownerCreate(oHex, oldTag, oc, osig) {
		t.Fatal("an owner written without an end time stays an owner")
	}
}

// Over the wire: once the end time passes, the owner tag no longer opens a
// circuit on the relay, whatever the app does.
func TestExpiredOwnerIsRefusedOverTheWire(t *testing.T) {
	reg := &Registry{}
	s, ri, _ := startRelay(t, reg, 5, true)
	dir := t.TempDir()
	s.OwnersFile, s.PairingFile = filepath.Join(dir, "owners.json"), filepath.Join(dir, "pairing.json")
	phone := testKey(t, 69)
	sign := func(pub, tg [32]byte) []byte { return SignCreate(phone, pub, tg) }

	code, _, _ := NewPairingCode(s.PairingFile)
	until := time.Now().Unix() + 3
	setPairingUntil(t, s.PairingFile, until)
	c, err := Build([]*RelayInfo{ri}, PairingTag(ri.Pub, phone.Public), 10*time.Second, PairingCreate(phone.Public), sign)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Pair(code, 5*time.Second); err != nil {
		t.Fatalf("pairing failed: %v", err)
	}
	c.Close()
	tag := OwnerTag(ri.Pub, phone.Public)
	c2, err := Build([]*RelayInfo{ri}, tag, 10*time.Second, nil, sign)
	if err != nil {
		t.Fatalf("before its end time the paired phone rides free: %v", err)
	}
	c2.Close()

	for time.Now().Unix() < until {
		time.Sleep(100 * time.Millisecond)
	}
	if c3, err := Build([]*RelayInfo{ri}, tag, 5*time.Second, nil, sign); err == nil {
		c3.Close()
		t.Fatal("after its end time the owner tag must be refused")
	}
}

// `sailnode unpair` (RemoveOwner) and the relay's sweep end a wallet's free
// access at once, a circuit already open on its owner tag included; an end
// time passing does the same. Other owners keep riding.
func TestUnpairedOrExpiredOwnerLosesOpenCircuits(t *testing.T) {
	reg := &Registry{}
	s, ri, _ := startRelay(t, reg, 5, true)
	dir := t.TempDir()
	s.OwnersFile, s.PairingFile = filepath.Join(dir, "owners.json"), filepath.Join(dir, "pairing.json")
	pair := func(k int, until time.Time) func(pub, tg [32]byte) []byte {
		phone := testKey(t, byte(k))
		sign := func(pub, tg [32]byte) []byte { return SignCreate(phone, pub, tg) }
		code, _, err := NewPairingCodeUntil(s.PairingFile, until)
		if err != nil {
			t.Fatal(err)
		}
		c, err := Build([]*RelayInfo{ri}, PairingTag(ri.Pub, phone.Public), 10*time.Second, PairingCreate(phone.Public), sign)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Pair(code, 5*time.Second); err != nil {
			t.Fatalf("pairing failed: %v", err)
		}
		c.Close()
		return sign
	}
	open := func(k int, sign func(pub, tg [32]byte) []byte) *Circuit {
		c, err := Build([]*RelayInfo{ri}, OwnerTag(ri.Pub, testKey(t, byte(k)).Public), 10*time.Second, nil, sign)
		if err != nil {
			t.Fatalf("owner %d should ride free: %v", k, err)
		}
		t.Cleanup(c.Close)
		if c.Ping(5*time.Second) != -1 {
			t.Fatalf("owner %d: ping failed", k)
		}
		return c
	}
	until := time.Now().Add(3 * time.Second)
	signA, signB, signC := pair(70, time.Time{}), pair(71, time.Time{}), pair(72, until)
	a, b, c := open(70, signA), open(71, signB), open(72, signC)

	if ok, err := RemoveOwner(s.OwnersFile, testKey(t, 70).Public); !ok || err != nil {
		t.Fatalf("RemoveOwner: %v %v", ok, err)
	}
	s.SweepOwners()
	if a.Ping(3*time.Second) == -1 {
		t.Fatal("an unpaired owner's open circuit must stop")
	}
	if b.Ping(5*time.Second) != -1 || c.Ping(5*time.Second) != -1 {
		t.Fatal("the other owners keep riding")
	}
	if x, err := Build([]*RelayInfo{ri}, OwnerTag(ri.Pub, testKey(t, 70).Public), 5*time.Second, nil, signA); err == nil {
		x.Close()
		t.Fatal("an unpaired owner's tag must be refused")
	}

	for time.Now().Before(until) {
		time.Sleep(100 * time.Millisecond)
	}
	s.SweepOwners()
	if c.Ping(3*time.Second) == -1 {
		t.Fatal("an expired owner's open circuit must stop")
	}
	if b.Ping(5*time.Second) != -1 {
		t.Fatal("a permanent owner keeps riding")
	}
	m, err := readOwners(s.OwnersFile)
	if err != nil || len(m) != 1 {
		t.Fatalf("owners.json should hold only the permanent owner left: %v %v", m, err)
	}
}

// A code minted for one wallet (`sailnode pair --wallet`, as the Lite gate
// mints them for the wallet its token is bound to) pairs that wallet and no
// other: handing the six digits to another phone gets that phone nothing,
// and does not use the code up.
func TestPairingCodeBoundToAWalletPairsOnlyThatWallet(t *testing.T) {
	dir := t.TempDir()
	relayKey, phone, stranger := testKey(t, 1), testKey(t, 2), testKey(t, 3)
	s := ownerServer(t, relayKey, [32]byte{})
	s.OwnersFile, s.PairingFile = filepath.Join(dir, "owners.json"), filepath.Join(dir, "pairing.json")
	code, _, err := NewPairingCode(s.PairingFile)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(s.PairingFile)
	var m map[string]any
	json.Unmarshal(raw, &m)
	m["wallet"] = hex.EncodeToString(phone.Public[:])
	raw, _ = json.Marshal(m)
	os.WriteFile(s.PairingFile, raw, 0o600)

	if err := s.tryPair(hex.EncodeToString(stranger.Public[:]), code); err == nil {
		t.Fatal("a code minted for one wallet must not pair another")
	}
	if _, ok := s.ownerFor(OwnerTag(relayKey.Public, stranger.Public)); ok {
		t.Fatal("the other wallet must not be an owner")
	}
	if err := s.tryPair(hex.EncodeToString(phone.Public[:]), code); err != nil {
		t.Fatalf("the wallet the code was minted for pairs: %v", err)
	}
	if _, ok := s.ownerFor(OwnerTag(relayKey.Public, phone.Public)); !ok {
		t.Fatal("the bound wallet should now be an owner")
	}
}
