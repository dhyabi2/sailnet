// Package token holds the Sailnet on-ledger registry ops (REGISTER, DESCRIPTOR,
// NOP), encoded in the 32-byte representative field of an ordinary 1-raw send
// to the anchor account, plus XNO amount helpers. Payments are plain XNO sends.
package token

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

const (
	Magic0  = 0x53 // 'S'
	Magic1  = 0x41 // 'A'
	Version = 0x01

	OpBuy        = 0x01
	OpTransfer   = 0x02
	OpAnchor     = 0x03
	OpRegister   = 0x04
	OpDescriptor = 0x05
	OpClaim      = 0x06
	OpNop        = 0x07
	OpReward     = 0x08 // levy payout for an epoch: Aux[0:4] = epoch number (UTC day)
	OpAlive      = 0x09 // relay heartbeat: "still serving"; a relay without one for AliveTTL is retired from the registry

	Decimals = 6
	Unit     = 1_000_000 // legacy: micro units in aux fields
)

// RateUnitRaw is the unit of relay prices in REGISTER aux: 1e20 raw = 1e-10 XNO.
// A uint32 rate therefore spans 1e-10 .. 0.43 XNO per MiB.
var RateUnitRaw = func() *big.Int { b, _ := new(big.Int).SetString("100000000000000000000", 10); return b }()

// RawPerXNO is 10^30.
var RawPerXNO = func() *big.Int { b, _ := new(big.Int).SetString("1000000000000000000000000000000", 10); return b }()

// RateToRaw converts a REGISTER rate (units of RateUnitRaw per MiB) to raw per MiB.
func RateToRaw(rate uint32) *big.Int {
	return new(big.Int).Mul(big.NewInt(int64(rate)), RateUnitRaw)
}

// RateFromXNO converts "0.00002" XNO per MiB to a uint32 rate.
func RateFromXNO(s string) (uint32, error) {
	raw, err := ParseXNO(s)
	if err != nil {
		return 0, err
	}
	r := new(big.Int).Quo(raw, RateUnitRaw)
	if !r.IsUint64() || r.Uint64() > 1<<32-1 {
		return 0, fmt.Errorf("rate out of range")
	}
	return uint32(r.Uint64()), nil
}

// ParseXNO parses a decimal XNO amount into raw.
func ParseXNO(s string) (*big.Int, error) {
	f, ok := new(big.Float).SetPrec(256).SetString(s)
	if !ok {
		return nil, fmt.Errorf("bad XNO amount %q", s)
	}
	f.Mul(f, new(big.Float).SetInt(RawPerXNO))
	r, _ := f.Int(nil)
	return r, nil
}

// FormatXNO renders raw as a decimal XNO string (trimmed).
func FormatXNO(raw *big.Int) string {
	if raw == nil {
		return "0"
	}
	q, r := new(big.Int).QuoRem(raw, RawPerXNO, new(big.Int))
	frac := fmt.Sprintf("%030s", r.String())
	frac = strings.TrimRight(frac, "0")
	if frac == "" {
		return q.String()
	}
	return q.String() + "." + frac
}

// Op is a decoded SAIL operation.
type Op struct {
	Code   byte
	Amount *big.Int // SAIL units (uint128)
	Aux    [12]byte
}

var ErrNotSail = errors.New("not a SAIL op")

// Encode packs an op into a 32-byte representative public key.
func Encode(op Op) ([32]byte, error) {
	var out [32]byte
	out[0], out[1], out[2], out[3] = Magic0, Magic1, Version, op.Code
	amt := op.Amount
	if amt == nil {
		amt = new(big.Int)
	}
	if amt.Sign() < 0 || amt.BitLen() > 128 {
		return out, errors.New("amount out of range")
	}
	amt.FillBytes(out[4:20])
	copy(out[20:32], op.Aux[:])
	return out, nil
}

// Decode parses a representative public key. Returns ErrNotSail for ordinary
// representatives so callers can cheaply skip normal Nano blocks.
func Decode(rep [32]byte) (Op, error) {
	if rep[0] != Magic0 || rep[1] != Magic1 || rep[2] != Version {
		return Op{}, ErrNotSail
	}
	op := Op{Code: rep[3], Amount: new(big.Int).SetBytes(rep[4:20])}
	copy(op.Aux[:], rep[20:32])
	if op.Code < OpBuy || op.Code > OpReward {
		return Op{}, errors.New("unknown SAIL opcode")
	}
	return op, nil
}

// AnchorAux packs circuitTag (8 bytes) and ratePpm (units per MiB, uint32).
func AnchorAux(circuitTag [8]byte, rate uint32) [12]byte {
	var a [12]byte
	copy(a[:8], circuitTag[:])
	binary.BigEndian.PutUint32(a[8:], rate)
	return a
}

// ParseAnchorAux is the inverse of AnchorAux.
func ParseAnchorAux(a [12]byte) (tag [8]byte, rate uint32) {
	copy(tag[:], a[:8])
	return tag, binary.BigEndian.Uint32(a[8:])
}

// RegisterAux packs country code (2 ASCII bytes), ASN, minRate (units/MiB), flags.
func RegisterAux(cc string, asn uint32, minRate uint32, flags uint16) [12]byte {
	var a [12]byte
	if len(cc) >= 2 {
		a[0], a[1] = cc[0], cc[1]
	}
	binary.BigEndian.PutUint32(a[2:6], asn)
	binary.BigEndian.PutUint32(a[6:10], minRate)
	binary.BigEndian.PutUint16(a[10:12], flags)
	return a
}

// ParseRegisterAux is the inverse of RegisterAux.
func ParseRegisterAux(a [12]byte) (cc string, asn, minRate uint32, flags uint16) {
	return string(a[0:2]), binary.BigEndian.Uint32(a[2:6]), binary.BigEndian.Uint32(a[6:10]), binary.BigEndian.Uint16(a[10:12])
}

// Relay flags.
const (
	FlagPublic = 1 << 0 // reachable on HTTPS :443
	FlagExit   = 1 << 1 // offers exit service
	FlagHome   = 1 << 2 // outbound-only, reached via reverse tunnel
	FlagFlow   = 1 << 3 // exit understands BEGIN2 / CREDIT (stream flow control)
)

// FormatSAIL renders units as a decimal SAIL string.
func FormatSAIL(units *big.Int) string {
	if units == nil {
		return "0"
	}
	q, r := new(big.Int).QuoRem(units, big.NewInt(Unit), new(big.Int))
	return fmt.Sprintf("%s.%06d", q, r.Int64())
}

// ParseSAIL parses "12.5" into units.
func ParseSAIL(s string) (*big.Int, error) {
	f, ok := new(big.Float).SetPrec(128).SetString(s)
	if !ok {
		return nil, fmt.Errorf("bad amount %q", s)
	}
	f.Mul(f, big.NewFloat(Unit))
	u, _ := f.Int(nil)
	return u, nil
}
