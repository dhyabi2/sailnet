// Package nano is a minimal Nano (XNO) client: keys, state blocks, PoW, RPC.
package nano

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"

	"github.com/hectorchu/gonano/util"
	"github.com/hectorchu/gonano/wallet/ed25519"
	"golang.org/x/crypto/blake2b"
)

// Key is a Nano account keypair.
type Key struct {
	Private ed25519.PrivateKey // 64 bytes (seed||pub)
	Public  [32]byte
	Address string
}

// NewSeed returns 32 random bytes.
func NewSeed() ([]byte, error) {
	s := make([]byte, 32)
	_, err := rand.Read(s)
	return s, err
}

// DeriveKey derives account `index` from a 32-byte seed (standard Nano scheme:
// blake2b-256(seed || index_be32)).
func DeriveKey(seed []byte, index uint32) (*Key, error) {
	if len(seed) != 32 {
		return nil, errors.New("seed must be 32 bytes")
	}
	h, _ := blake2b.New256(nil)
	h.Write(seed)
	var idx [4]byte
	binary.BigEndian.PutUint32(idx[:], index)
	h.Write(idx[:])
	priv := ed25519.NewKeyFromSeed(h.Sum(nil))
	k := &Key{Private: priv}
	copy(k.Public[:], priv[32:])
	addr, err := util.PubkeyToAddress(k.Public[:])
	if err != nil {
		return nil, err
	}
	k.Address = addr
	return k, nil
}

// Sign signs a 32-byte block hash.
func (k *Key) Sign(hash []byte) []byte { return ed25519.Sign(k.Private, hash) }

// Verify checks an ed25519-blake2b signature by a public key.
func Verify(pub [32]byte, hash, sig []byte) bool {
	return ed25519.Verify(ed25519.PublicKey(pub[:]), hash, sig)
}

// AddressToPubkey decodes a nano_ address.
func AddressToPubkey(addr string) ([32]byte, error) {
	var out [32]byte
	pk, err := util.AddressToPubkey(addr)
	if err != nil {
		return out, err
	}
	copy(out[:], pk)
	return out, nil
}

// PubkeyToAddress encodes a public key as a nano_ address.
func PubkeyToAddress(pk [32]byte) string {
	a, _ := util.PubkeyToAddress(pk[:])
	return a
}

// HexToPubkey parses a 64-hex-char key.
func HexToPubkey(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return out, errors.New("bad 32-byte hex")
	}
	copy(out[:], b)
	return out, nil
}
