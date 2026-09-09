package relay

import (
	"crypto/rand"

	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dhyabi2/sail/wire"
	"log"
	"os"
	"strings"
	"time"
)

// Pairing: the operator runs `sailnode pair` on the relay, reads six digits
// off the terminal, and types them into the app. The app opens an ordinary
// paid circuit to the relay and sends the code inside the tunnel; the relay
// already knows which wallet paid that circuit, so a matching code makes
// that wallet an owner (owners.json) and the app can ride the relay free
// (owner.go). Nothing is copied by a human but six digits, and nothing
// touches the ledger beyond the anchor the app would have paid anyway.
//
// A code lives five minutes, works once, and is void after three wrong
// guesses. Only its salted hash is on disk. The running relay reads the
// file when a code arrives, so `sailnode pair` needs no restart.

const (
	PairingTTL      = 5 * time.Minute
	pairingAttempts = 3
)

type pairingFile struct {
	Salt     string `json:"salt"`
	Hash     string `json:"hash"`
	Expires  int64  `json:"expires"`
	Attempts int    `json:"attempts"`
}

func pairingHash(salt []byte, code string) string {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(code))
	return hex.EncodeToString(h.Sum(nil))
}

// NewPairingCode mints a code, stores its hash at path, and returns the code.
func NewPairingCode(path string) (string, time.Time, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", time.Time{}, err
	}
	n := (uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])) % 1000000
	code := fmt.Sprintf("%06d", n)
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", time.Time{}, err
	}
	exp := time.Now().Add(PairingTTL)
	data, _ := json.Marshal(pairingFile{Salt: hex.EncodeToString(salt), Hash: pairingHash(salt, code), Expires: exp.Unix()})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", time.Time{}, err
	}
	return code, exp, nil
}

// tryPair checks a code against the pairing file and, on success, makes
// ownerHex (the hex public key of the wallet that paid the circuit) an owner.
func (s *Server) tryPair(ownerHex, code string) error {
	if s.PairingFile == "" {
		return errors.New("pairing is not set up on this relay")
	}
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	raw, err := os.ReadFile(s.PairingFile)
	if err != nil {
		return errors.New("no pairing code is active: run `sailnode pair` on the relay")
	}
	var pf pairingFile
	if json.Unmarshal(raw, &pf) != nil || time.Now().Unix() >= pf.Expires {
		os.Remove(s.PairingFile)
		return errors.New("the pairing code has expired: run `sailnode pair` on the relay for a new one")
	}
	if pf.Attempts >= pairingAttempts {
		os.Remove(s.PairingFile)
		return errors.New("too many wrong codes: run `sailnode pair` on the relay for a new one")
	}
	salt, _ := hex.DecodeString(pf.Salt)
	if subtle.ConstantTimeCompare([]byte(pairingHash(salt, code)), []byte(pf.Hash)) != 1 {
		pf.Attempts++
		if data, err := json.Marshal(pf); err == nil {
			os.WriteFile(s.PairingFile, data, 0o600)
		}
		return fmt.Errorf("wrong code (%d of %d tries)", pf.Attempts, pairingAttempts)
	}
	os.Remove(s.PairingFile) // one use
	pub, err := hex.DecodeString(ownerHex)
	if err != nil || len(pub) != 32 {
		return errors.New("this circuit was not paid by a wallet that can be paired")
	}
	var p [32]byte
	copy(p[:], pub)
	s.addOwner(p)
	log.Printf("paired: a new owner rides this relay free (%s…)", ownerHex[:8])
	return nil
}

// addOwner records an owner and persists the list.
func (s *Server) addOwner(pub [32]byte) {
	s.ownersMu.Lock()
	defer s.ownersMu.Unlock()
	if s.Owners == nil {
		s.Owners = map[[32]byte]bool{}
	}
	s.Owners[pub] = true
	if s.OwnersFile == "" {
		return
	}
	var list []string
	for k := range s.Owners {
		list = append(list, hex.EncodeToString(k[:]))
	}
	data, _ := json.Marshal(map[string][]string{"owners": list})
	os.WriteFile(s.OwnersFile, data, 0o600)
}

// LoadOwners reads owners.json, if any, into s.Owners.
func (s *Server) LoadOwners() int {
	if s.OwnersFile == "" {
		return 0
	}
	raw, err := os.ReadFile(s.OwnersFile)
	if err != nil {
		return 0
	}
	var f struct {
		Owners []string `json:"owners"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return 0
	}
	s.ownersMu.Lock()
	defer s.ownersMu.Unlock()
	if s.Owners == nil {
		s.Owners = map[[32]byte]bool{}
	}
	for _, h := range f.Owners {
		b, err := hex.DecodeString(h)
		if err != nil || len(b) != 32 {
			continue
		}
		var p [32]byte
		copy(p[:], b)
		s.Owners[p] = true
	}
	return len(s.Owners)
}

// ownerFor returns the owner whose tag this is, if any: the --owner/--payout
// wallet, or one paired later.
func (s *Server) ownerFor(tagB [32]byte) ([32]byte, bool) {
	if s.Key == nil {
		return [32]byte{}, false
	}
	if s.Owner != ([32]byte{}) && tagB == OwnerTag(s.Key.Public, s.Owner) {
		return s.Owner, true
	}
	s.ownersMu.Lock()
	defer s.ownersMu.Unlock()
	for o := range s.Owners {
		if tagB == OwnerTag(s.Key.Public, o) {
			return o, true
		}
	}
	return [32]byte{}, false
}

// handlePair answers CmdPair on a circuit.
func (s *Server) handlePair(c *circuit, sid uint16, data []byte) {
	owner := s.Quota.Owner(c.tag)
	if owner == "" {
		s.reply(c, wire.CmdError, sid, []byte("pairing needs a circuit paid by your wallet"))
		return
	}
	if err := s.tryPair(owner, string(data)); err != nil {
		s.reply(c, wire.CmdError, sid, []byte(err.Error()))
		return
	}
	s.reply(c, wire.CmdPaired, sid, []byte("paired"))
}
