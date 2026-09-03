package token

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
)

// Relay is an on-ledger relay registration (REGISTER + DESCRIPTOR blocks sent
// as 1-raw sends to the treasury/anchor account). SAIL balances and stakes live
// in HodlGame's state machine, not here.
type Relay struct {
	Account    string
	Country    string
	ASN        uint32
	MinRate    uint32 // SAIL units per MiB
	Flags      uint16
	Descriptor [12]byte
	Height     uint64
}

// State is the deterministic Sailnet registry.
type State struct {
	Treasury string
	Relays   map[string]*Relay
	Events   int
}

// NewState creates an empty registry.
func NewState(treasury string) *State {
	return &State{Treasury: treasury, Relays: map[string]*Relay{}}
}

// Event is one decoded op with its ledger context.
type Event struct {
	Op         Op
	Sender     string
	Recipient  string
	SendHash   string
	SendHeight uint64
}

// Apply folds one registry op.
func (s *State) Apply(e *Event) error {
	s.Events++
	switch e.Op.Code {
	case OpRegister:
		if e.Recipient != s.Treasury {
			return fmt.Errorf("REGISTER must target the anchor account")
		}
		cc, asn, rate, flags := ParseRegisterAux(e.Op.Aux)
		r := s.Relays[e.Sender]
		if r == nil {
			r = &Relay{Account: e.Sender}
			s.Relays[e.Sender] = r
		}
		r.Country, r.ASN, r.MinRate, r.Flags, r.Height = cc, asn, rate, flags, e.SendHeight
	case OpDescriptor:
		r := s.Relays[e.Sender]
		if r == nil {
			return fmt.Errorf("DESCRIPTOR before REGISTER")
		}
		r.Descriptor = e.Op.Aux
	case OpNop:
	default:
		return fmt.Errorf("op %d is not a registry op", e.Op.Code)
	}
	return nil
}

// Root is the canonical registry fingerprint.
func (s *State) Root() string {
	h := sha256.New()
	keys := make([]string, 0, len(s.Relays))
	for k := range s.Relays {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r := s.Relays[k]
		var b [12]byte
		copy(b[:], r.Country)
		binary.BigEndian.PutUint32(b[2:], r.ASN)
		binary.BigEndian.PutUint32(b[6:], r.MinRate)
		binary.BigEndian.PutUint16(b[10:], r.Flags)
		h.Write([]byte(k))
		h.Write(b[:])
		h.Write(r.Descriptor[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}
