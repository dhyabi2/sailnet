package relay

import (
	"crypto/rand"
	"golang.org/x/crypto/blake2b"

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
//
// A code can carry an end time (`sailnode pair --until`, which the Lite gate
// sets to the end of the access it sold): the wallet it pairs is an owner
// until then, and the relay itself stops honouring its owner tag after it,
// whatever the app does. `sailnode unpair` ends it sooner. Owners written
// without an end time, as every relay before this wrote them, stay owners.

const (
	PairingTTL      = 5 * time.Minute
	pairingAttempts = 3
)

type pairingFile struct {
	Salt     string `json:"salt"`
	Hash     string `json:"hash"`
	Expires  int64  `json:"expires"`
	Attempts int    `json:"attempts"`
	// OwnerExpires is when the free access this code grants ends (Unix
	// seconds); 0 means never.
	OwnerExpires int64 `json:"owner_expires,omitempty"`
}

func pairingHash(salt []byte, code string) string {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(code))
	return hex.EncodeToString(h.Sum(nil))
}

// NewPairingCode mints a code, stores its hash at path, and returns the code.
// The wallet it pairs is an owner for good.
func NewPairingCode(path string) (string, time.Time, error) {
	return NewPairingCodeUntil(path, time.Time{})
}

// NewPairingCodeUntil mints a code whose wallet is an owner only until until
// (the zero time: for good).
func NewPairingCodeUntil(path string, until time.Time) (string, time.Time, error) {
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
	pf := pairingFile{Salt: hex.EncodeToString(salt), Hash: pairingHash(salt, code), Expires: exp.Unix()}
	if !until.IsZero() {
		pf.OwnerExpires = until.Unix()
	}
	data, _ := json.Marshal(pf)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", time.Time{}, err
	}
	return code, exp, nil
}

// tryPair checks a code against the pairing file and, on success, makes
// ownerHex (the hex public key of the wallet that paid the circuit) an owner.
// PairingTag is the tag a wallet uses to open the short free circuit that
// carries its pairing code: it can only be formed by that wallet for this
// relay, and the relay honours it only while a code is active (tryPair).
func PairingTag(relayPub, clientPub [32]byte) [32]byte {
	h, _ := blake2b.New256(nil)
	h.Write([]byte("sailnet-pairing"))
	h.Write(relayPub[:])
	h.Write(clientPub[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// PairingBytes is what a pairing circuit may carry: the code and the answer,
// nothing a stranger could use, and only for the five minutes a code lives.
const PairingBytes = 256 << 10

// PairingCreate is what a pairing CREATE carries after the signature instead
// of a payment: the wallet's public key, so the relay can recompute the tag
// and check the signature (a CREATE otherwise names only the circuit's
// ephemeral key). A relay from before pairing reads it as a payment it
// cannot parse and refuses, which is the right answer there.
func PairingCreate(walletPub [32]byte) []byte {
	return append([]byte("pair:"), walletPub[:]...)
}

// pairingCreate admits a CREATE whose tag is the pairing tag of the signing
// wallet, without payment, while a code is active. Before this the app had
// to pay the relay an anchor just to hand over the code, which meant proof
// of work, a reachable RPC and a ledger wait on a phone — a minute of "…"
// that often ended in a timeout — and it overwrote the payment the running
// circuit was made with.
func (s *Server) pairingCreate(tag string, tagB, clientPub [32]byte, sig, extra []byte) bool {
	if s.Quota == nil || s.Key == nil || s.PairingFile == "" || len(extra) != 5+32 || string(extra[:5]) != "pair:" {
		return false
	}
	var wallet [32]byte
	copy(wallet[:], extra[5:])
	if tagB != PairingTag(s.Key.Public, wallet) || !VerifyCreate(wallet, clientPub, tagB, sig) {
		return false
	}
	raw, err := os.ReadFile(s.PairingFile)
	if err != nil {
		return false
	}
	var pf pairingFile
	if json.Unmarshal(raw, &pf) != nil || time.Now().Unix() >= pf.Expires || pf.Attempts >= pairingAttempts {
		return false
	}
	if !s.Quota.Known(tag) {
		s.Quota.Credit(tag, PairingBytes, hex.EncodeToString(wallet[:]))
		log.Printf("pairing: a wallet opened a circuit to send its code")
	}
	return true
}

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
	if pf.OwnerExpires != 0 && time.Now().Unix() >= pf.OwnerExpires {
		return errors.New("the access this code was minted for has already ended")
	}
	var p [32]byte
	copy(p[:], pub)
	s.addOwner(p, pf.OwnerExpires)
	if pf.OwnerExpires != 0 {
		log.Printf("paired: a new owner rides this relay free until %s (%s…)", time.Unix(pf.OwnerExpires, 0).UTC().Format(time.RFC3339), ownerHex[:8])
	} else {
		log.Printf("paired: a new owner rides this relay free (%s…)", ownerHex[:8])
	}
	return nil
}

// owners.json: {"owners": [hex, ...], "expires": {hex: unix}}. An owner
// missing from "expires" rides free for good, which is what every file
// written before end times existed means.
type ownersFile struct {
	Owners  []string         `json:"owners"`
	Expires map[string]int64 `json:"expires,omitempty"`
}

func readOwners(path string) (map[[32]byte]int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f ownersFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	m := map[[32]byte]int64{}
	for _, h := range f.Owners {
		b, err := hex.DecodeString(h)
		if err != nil || len(b) != 32 {
			continue
		}
		var p [32]byte
		copy(p[:], b)
		m[p] = f.Expires[h]
	}
	return m, nil
}

func writeOwners(path string, m map[[32]byte]int64) error {
	f := ownersFile{Owners: []string{}}
	for k, until := range m {
		h := hex.EncodeToString(k[:])
		f.Owners = append(f.Owners, h)
		if until != 0 {
			if f.Expires == nil {
				f.Expires = map[string]int64{}
			}
			f.Expires[h] = until
		}
	}
	data, _ := json.Marshal(f)
	return os.WriteFile(path, data, 0o600)
}

// RemoveOwner takes a paired wallet out of owners.json (`sailnode unpair`).
// A running relay notices within a minute (RunOwners) and ends the wallet's
// free access, open circuits included. It reports whether the wallet was
// there.
func RemoveOwner(path string, pub [32]byte) (bool, error) {
	m, err := readOwners(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if _, ok := m[pub]; !ok {
		return false, nil
	}
	delete(m, pub)
	return true, writeOwners(path, m)
}

// addOwner records an owner, free until until (0: for good), and persists
// the list. The file is read first, so a wallet `sailnode unpair` removed
// since the last sweep is not written back.
func (s *Server) addOwner(pub [32]byte, until int64) {
	s.ownersMu.Lock()
	defer s.ownersMu.Unlock()
	if s.OwnersFile != "" {
		if m, err := readOwners(s.OwnersFile); err == nil {
			s.Owners = m
		}
	}
	if s.Owners == nil {
		s.Owners = map[[32]byte]int64{}
	}
	s.Owners[pub] = until
	if s.OwnersFile == "" {
		return
	}
	writeOwners(s.OwnersFile, s.Owners)
	if st, err := os.Stat(s.OwnersFile); err == nil {
		s.ownersMod = st.ModTime()
	}
}

// LoadOwners reads owners.json, if any, into s.Owners.
func (s *Server) LoadOwners() int {
	if s.OwnersFile == "" {
		return 0
	}
	m, err := readOwners(s.OwnersFile)
	if err != nil {
		return 0
	}
	s.ownersMu.Lock()
	defer s.ownersMu.Unlock()
	if s.Owners == nil {
		s.Owners = map[[32]byte]int64{}
	}
	for p, until := range m {
		s.Owners[p] = until
	}
	if st, err := os.Stat(s.OwnersFile); err == nil {
		s.ownersMod = st.ModTime()
	}
	return len(s.Owners)
}

// ownerFor returns the owner whose tag this is, if any: the --owner/--payout
// wallet, or one paired later whose end time, if it has one, has not passed.
func (s *Server) ownerFor(tagB [32]byte) ([32]byte, bool) {
	if s.Key == nil {
		return [32]byte{}, false
	}
	if s.Owner != ([32]byte{}) && tagB == OwnerTag(s.Key.Public, s.Owner) {
		return s.Owner, true
	}
	now := time.Now().Unix()
	s.ownersMu.Lock()
	defer s.ownersMu.Unlock()
	for o, until := range s.Owners {
		if tagB == OwnerTag(s.Key.Public, o) {
			if until != 0 && now >= until {
				return [32]byte{}, false
			}
			return o, true
		}
	}
	return [32]byte{}, false
}

// endOwnerGrant spends what a wallet's owner tag still holds, so a circuit
// open on it stops at its next cell and a new CREATE on it finds nothing to
// spend on the paid path. Re-pairing tops the tag up again (ownerCreate).
func (s *Server) endOwnerGrant(pub [32]byte) {
	if s.Quota == nil || s.Key == nil {
		return
	}
	t := OwnerTag(s.Key.Public, pub)
	tag := hex.EncodeToString(t[:])
	if rem := s.Quota.Remaining(tag); rem > 0 {
		s.Quota.Consume(tag, rem)
	}
}

// SweepOwners ends the free access of owners whose end time has passed and
// of those removed from owners.json since it was last read (`sailnode
// unpair`). Expired owners are dropped from the file.
func (s *Server) SweepOwners() {
	var gone [][32]byte
	now := time.Now().Unix()
	s.ownersMu.Lock()
	if s.OwnersFile != "" {
		if st, err := os.Stat(s.OwnersFile); err == nil && !st.ModTime().Equal(s.ownersMod) {
			if m, err := readOwners(s.OwnersFile); err == nil {
				for p := range s.Owners {
					if _, ok := m[p]; !ok {
						gone = append(gone, p)
					}
				}
				s.Owners = m
				s.ownersMod = st.ModTime()
			}
		}
	}
	expired := false
	for p, until := range s.Owners {
		if until != 0 && now >= until {
			gone = append(gone, p)
			delete(s.Owners, p)
			expired = true
		}
	}
	if expired && s.OwnersFile != "" {
		writeOwners(s.OwnersFile, s.Owners)
		if st, err := os.Stat(s.OwnersFile); err == nil {
			s.ownersMod = st.ModTime()
		}
	}
	s.ownersMu.Unlock()
	for _, p := range gone {
		s.endOwnerGrant(p)
	}
	if len(gone) > 0 {
		log.Printf("owners: free access ended for %d wallet(s) (expired or unpaired)", len(gone))
	}
}

// RunOwners sweeps the owner list on a timer; run it on a relay with
// paired owners.
func (s *Server) RunOwners(every time.Duration) {
	for {
		time.Sleep(every)
		s.SweepOwners()
	}
}

// handlePair answers CmdPair on a circuit.
func (s *Server) handlePair(c *circuit, sid uint16, data []byte) {
	owner := s.Quota.Owner(c.tag)
	if owner == "" {
		s.reply(c, wire.CmdError, sid, []byte("pairing needs a circuit paid by your wallet"))
		return
	}
	if err := s.tryPair(owner, string(data)); err != nil {
		log.Printf("pairing: %v", err)
		s.reply(c, wire.CmdError, sid, []byte(err.Error()))
		return
	}
	s.reply(c, wire.CmdPaired, sid, []byte("paired"))
}
