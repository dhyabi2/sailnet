package relay

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"log"

	"github.com/dhyabi2/sail/nano"
	"github.com/dhyabi2/sail/token"
	"github.com/dhyabi2/sail/wire"
	"golang.org/x/crypto/blake2b"
)

// SignedRecord is a relay's self-description signed by its own key. Relays
// hand these to each other and to clients over the tunnel, so the network
// can be learned from any one relay with no ledger in reach. A forged record
// needs the relay's private key, which is exactly what it would need to
// register on the ledger, so gossip adds no new trust.
type SignedRecord struct {
	Account string `json:"a"`
	Country string `json:"cc"`
	ASN     uint32 `json:"asn"`
	MinRate uint32 `json:"rate"`
	Flags   uint16 `json:"flags"`
	Desc    string `json:"desc"` // 12-byte descriptor, hex
	Host    string `json:"host,omitempty"`
	Time    int64  `json:"t"`   // unix seconds; records older than RecordTTL are dropped
	Sig     string `json:"sig"` // ed25519 over the fields, hex
}

// RecordTTL is how long a gossiped record stays usable without renewal.
const RecordTTL = 48 * time.Hour

func (r *SignedRecord) digest() []byte {
	h, _ := blake2b.New256(nil)
	h.Write([]byte("sailnet-record"))
	h.Write([]byte(r.Account))
	h.Write([]byte(r.Country))
	var b [8]byte
	binary.BigEndian.PutUint32(b[:4], r.ASN)
	binary.BigEndian.PutUint32(b[4:], r.MinRate)
	h.Write(b[:])
	binary.BigEndian.PutUint16(b[:2], r.Flags)
	h.Write(b[:2])
	h.Write([]byte(r.Desc))
	h.Write([]byte(r.Host))
	binary.BigEndian.PutUint64(b[:], uint64(r.Time))
	h.Write(b[:])
	return h.Sum(nil)
}

// NewSignedRecord signs ri with key (key must be ri's own).
func NewSignedRecord(key *nano.Key, ri *RelayInfo) *SignedRecord {
	d := ri.Desc.Encode()
	r := &SignedRecord{Account: ri.Account, Country: ri.Country, ASN: ri.ASN, MinRate: ri.MinRate, Flags: ri.Flags, Desc: hex.EncodeToString(d[:]), Host: ri.Host, Time: time.Now().Unix()}
	r.Sig = hex.EncodeToString(key.Sign(r.digest()))
	return r
}

// Verify checks the signature and age and returns the relay it describes.
func (r *SignedRecord) Verify(now time.Time) (*RelayInfo, error) {
	pub, err := nano.AddressToPubkey(r.Account)
	if err != nil {
		return nil, err
	}
	sig, err := hex.DecodeString(r.Sig)
	if err != nil || !nano.Verify(pub, r.digest(), sig) {
		return nil, errors.New("bad record signature")
	}
	age := now.Sub(time.Unix(r.Time, 0))
	if age > RecordTTL || age < -10*time.Minute {
		return nil, errors.New("record expired")
	}
	db, err := hex.DecodeString(r.Desc)
	if err != nil || len(db) != 12 {
		return nil, errors.New("bad descriptor")
	}
	var a [12]byte
	copy(a[:], db)
	d, ok := DecodeDescriptor(a)
	if !ok {
		return nil, errors.New("empty descriptor")
	}
	return &RelayInfo{Account: r.Account, Pub: pub, Country: r.Country, ASN: r.ASN, MinRate: r.MinRate, Flags: r.Flags, Desc: d, Host: r.Host}, nil
}

// AddGossip verifies and stores a record. Ledger and bridge entries for the
// same account take precedence in All/Get. Returns true if stored.
func (r *Registry) AddGossip(rec *SignedRecord) bool {
	ri, err := rec.Verify(time.Now())
	if err != nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gossip == nil {
		r.gossip = map[string]*SignedRecord{}
	}
	if old := r.gossip[rec.Account]; old != nil && old.Time >= rec.Time {
		return false
	}
	if r.gossip[rec.Account] == nil && len(r.gossip) >= 500 {
		return false // cap: forged records must not grow memory or the reply beyond one exchange
	}
	if ri.Desc.IP.IsPrivate() || ri.Desc.IP.IsLoopback() || ri.Desc.IP.IsUnspecified() {
		return false
	}
	r.gossip[rec.Account] = rec
	_ = ri
	return true
}

// Records returns every signed record this registry can vouch for: its own
// (if self is set) and the gossip it has verified, minus expired ones.
func (r *Registry) Records(self *SignedRecord) []*SignedRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []*SignedRecord{}
	if self != nil {
		out = append(out, self)
	}
	now := time.Now()
	for acct, rec := range r.gossip {
		if now.Sub(time.Unix(rec.Time, 0)) > RecordTTL {
			delete(r.gossip, acct)
			continue
		}
		if self != nil && acct == self.Account {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// gossipRelays returns verified gossip entries not shadowed by ledger or bridges (caller holds r.mu).
func (r *Registry) gossipRelaysLocked() []*RelayInfo {
	var out []*RelayInfo
	now := time.Now()
	for acct, rec := range r.gossip {
		if r.relays[acct] != nil || r.bridges[acct] != nil {
			continue
		}
		if ri, err := rec.Verify(now); err == nil {
			out = append(out, ri)
		}
	}
	return out
}

// FetchRelays dials a relay and asks for the records it knows.
func FetchRelays(rel *RelayInfo, timeout time.Duration) ([]*SignedRecord, error) {
	conn, err := DialRelay(rel, timeout)
	if err != nil {
		return nil, err
	}
	return FetchRelaysOver(conn, timeout)
}

// FetchRelaysOver runs the gossip exchange on an open tunnel connection and closes it.
func FetchRelaysOver(conn net.Conn, timeout time.Duration) ([]*SignedRecord, error) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	w := newConnWriter(conn, true)
	defer w.stop()
	if err := w.write(&wire.Cell{Cmd: wire.CmdRelays}); err != nil {
		return nil, err
	}
	r := bufio.NewReader(conn)
	var body []byte
	for i := 0; i < 64; i++ { // at most 64 KiB of records
		cell, err := wire.ReadCell(r)
		if err != nil {
			return nil, err
		}
		if cell.Cmd == wire.CmdError {
			return nil, fmt.Errorf("relay: %s", cell.Payload)
		}
		if cell.Cmd != wire.CmdRelaysReply {
			continue
		}
		if len(cell.Payload) == 0 {
			break
		}
		body = append(body, cell.Payload...)
	}
	var recs []*SignedRecord
	if err := json.Unmarshal(body, &recs); err != nil {
		return nil, fmt.Errorf("gossip: bad records: %v", err)
	}
	return recs, nil
}

// sendRelays answers a CmdRelays request on a tunnel connection.
func (s *Server) sendRelays(in *connWriter) {
	var self *SignedRecord
	if s.Self != nil {
		self = NewSignedRecord(s.Key, s.Self)
	}
	body, _ := json.Marshal(s.Registry.Records(self))
	seq := uint16(0)
	for len(body) > 0 {
		n := len(body)
		if n > 1000 {
			n = 1000
		}
		if in.write(&wire.Cell{Cmd: wire.CmdRelaysReply, StreamID: seq, Payload: body[:n]}) != nil {
			return
		}
		body = body[n:]
		seq++
	}
	in.write(&wire.Cell{Cmd: wire.CmdRelaysReply, StreamID: seq})
}

// Gossip asks up to n random-ish peers for their records and merges them.
// Called by relays periodically; the network then knows itself without the
// ledger, and a client that reaches any one relay can learn the rest.
func (s *Server) Gossip(n int) {
	peers := s.Registry.All()
	asked := 0
	for _, p := range peers {
		if asked >= n {
			break
		}
		if p.Account == s.Key.Address || p.Flags&token.FlagHome != 0 {
			continue
		}
		recs, err := FetchRelays(p, 15*time.Second)
		if err != nil {
			continue
		}
		asked++
		added := 0
		for _, rec := range recs {
			if s.Registry.AddGossip(rec) {
				added++
			}
		}
		if added > 0 {
			log.Printf("gossip: %d record(s) from %s", added, strings.ToLower(short(p.Account)))
		}
	}
}

// bridgeWithRecordLocked fills a bridge's unknown fields (exit flag, country,
// rate) from the relay's own signed record when gossip has brought one; the
// bridge line's address stays authoritative. Caller holds r.mu.
func (r *Registry) bridgeWithRecordLocked(b *RelayInfo) *RelayInfo {
	rec := r.gossip[b.Account]
	if rec == nil {
		return b
	}
	ri, err := rec.Verify(time.Now())
	if err != nil {
		return b
	}
	c := *b
	c.Flags, c.Country, c.ASN, c.MinRate = ri.Flags, ri.Country, ri.ASN, ri.MinRate
	if c.Host == "" {
		c.Host = ri.Host
	}
	return &c
}
