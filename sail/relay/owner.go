package relay

import (
	"encoding/hex"

	"golang.org/x/crypto/blake2b"
)

// The operator's own wallet rides the operator's own relay for free.
//
// A relay names an owner: the account its earnings go to (--payout), or one
// given explicitly (--owner). A CREATE whose tag is this relay's owner tag
// and whose signature verifies against the owner's key is admitted without
// a payment and without a ledger lookup. Nobody else pays for it: the next
// hop is prepaid from the float exactly as for any circuit, so the owner's
// traffic is paid for out of what this relay earned — the owner's money
// already — and a one-hop circuit through the owner's own exit costs
// nothing at all. Every other operator earns exactly what they would have.
//
// The proof is a signature, not a claim. The owner tag is bound to this
// relay's key, so a tag accepted here means nothing anywhere else, and the
// owner's key never touches the wire — only a signature over the same bytes
// every paid CREATE already signs. To everyone but this relay the circuit
// looks like any other.

// OwnerBytes is one grant; the tag is topped up on every CREATE, so in
// practice the owner is not metered.
const OwnerBytes int64 = 1 << 40

// OwnerTag is the payment tag an owner presents to its own relay: bound to
// the relay so it cannot be replayed elsewhere, and to the owner so a relay
// with a different owner computes a different tag and refuses it.
func OwnerTag(relayPub, ownerPub [32]byte) [32]byte {
	h, _ := blake2b.New256(nil)
	h.Write([]byte("sailnet-owner"))
	h.Write(relayPub[:])
	h.Write(ownerPub[:])
	var t [32]byte
	copy(t[:], h.Sum(nil))
	return t
}

// ownerCreate admits a CREATE from this relay's owner. It reports true when
// the tag is the owner tag for this relay and the signature is the owner's;
// the tag is credited, or topped up, so the circuit proceeds as if paid.
// Anything else — no owner, another tag, a bad signature — is false, and
// the CREATE takes the ordinary paid path, which will refuse it.
func (s *Server) ownerCreate(tag string, tagB, clientPub [32]byte, sig []byte) bool {
	if s.Owner == ([32]byte{}) || s.Key == nil || s.Quota == nil {
		return false
	}
	if tagB != OwnerTag(s.Key.Public, s.Owner) {
		return false
	}
	if !VerifyCreate(s.Owner, clientPub, tagB, sig) {
		return false
	}
	if !s.Quota.Credit(tag, OwnerBytes, hex.EncodeToString(s.Owner[:])) && s.Quota.Remaining(tag) < OwnerBytes/2 {
		s.Quota.Add(tag, OwnerBytes)
	}
	return true
}
