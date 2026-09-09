package relay

import (
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/dhyabi2/sail/nano"
)

// Run a relay, ride it free: the operator's own wallet opens circuits on
// the operator's own relay without paying. Proof is a signature over the
// owner tag, which is bound to this relay's key.

func testKey(t *testing.T, b byte) *nano.Key {
	t.Helper()
	seed := make([]byte, 32)
	seed[0] = b
	k, err := nano.DeriveKey(seed, 0)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func ownerServer(t *testing.T, relayKey *nano.Key, ownerPub [32]byte) *Server {
	t.Helper()
	q, err := NewQuota(filepath.Join(t.TempDir(), "quota.wal"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Key: relayKey, Owner: ownerPub, Quota: q}
}

func createFrom(k *nano.Key, tag [32]byte) (clientPub [32]byte, sig []byte, tagHex string) {
	clientPub = [32]byte{7, 7, 7}
	return clientPub, SignCreate(k, clientPub, tag), hex.EncodeToString(tag[:])
}

func TestOwnerRidesItsOwnRelayFree(t *testing.T) {
	relayKey, owner := testKey(t, 1), testKey(t, 2)
	s := ownerServer(t, relayKey, owner.Public)
	tag := OwnerTag(relayKey.Public, owner.Public)
	clientPub, sig, tagHex := createFrom(owner, tag)

	if !s.ownerCreate(tagHex, tag, clientPub, sig) {
		t.Fatal("the owner's signed CREATE should be admitted")
	}
	if !s.Quota.Known(tagHex) || s.Quota.Remaining(tagHex) != OwnerBytes {
		t.Fatalf("owner tag should be credited %d bytes, has %d", OwnerBytes, s.Quota.Remaining(tagHex))
	}
	// The tag is owned by the owner's key, so the ordinary CREATE check
	// (VerifyCreate against Quota.Owner) keeps working for later circuits.
	if got := s.Quota.Owner(tagHex); got != hex.EncodeToString(owner.Public[:]) {
		t.Fatalf("tag owner = %s, want the owner's key", got)
	}
}

func TestOwnerTagIsToppedUpNotMetered(t *testing.T) {
	relayKey, owner := testKey(t, 1), testKey(t, 2)
	s := ownerServer(t, relayKey, owner.Public)
	tag := OwnerTag(relayKey.Public, owner.Public)
	clientPub, sig, tagHex := createFrom(owner, tag)
	s.ownerCreate(tagHex, tag, clientPub, sig)
	s.Quota.Consume(tagHex, OwnerBytes-1) // nearly spent
	if !s.ownerCreate(tagHex, tag, clientPub, sig) {
		t.Fatal("a later CREATE from the owner should still be admitted")
	}
	if rem := s.Quota.Remaining(tagHex); rem < OwnerBytes {
		t.Fatalf("the owner tag should have been topped up, remaining %d", rem)
	}
}

func TestStrangersAndOtherRelaysGetNothing(t *testing.T) {
	relayKey, owner, stranger := testKey(t, 1), testKey(t, 2), testKey(t, 3)
	s := ownerServer(t, relayKey, owner.Public)
	tag := OwnerTag(relayKey.Public, owner.Public)

	// Right tag, wrong key: a stranger who learned the tag cannot sign for it.
	clientPub, sig, tagHex := createFrom(stranger, tag)
	if s.ownerCreate(tagHex, tag, clientPub, sig) || s.Quota.Known(tagHex) {
		t.Fatal("a stranger's signature over the owner tag must be refused, and nothing credited")
	}

	// The owner's own signature over the tag for a DIFFERENT relay: refused
	// here, because the tag is bound to the relay it was made for.
	other := OwnerTag(testKey(t, 9).Public, owner.Public)
	clientPub, sig, otherHex := createFrom(owner, other)
	if s.ownerCreate(otherHex, other, clientPub, sig) {
		t.Fatal("an owner tag for another relay must not be honoured here")
	}

	// A relay with no owner honours nobody.
	none := ownerServer(t, relayKey, [32]byte{})
	clientPub, sig, tagHex = createFrom(owner, tag)
	if none.ownerCreate(tagHex, tag, clientPub, sig) {
		t.Fatal("a relay with no owner must not admit anyone free")
	}
}
