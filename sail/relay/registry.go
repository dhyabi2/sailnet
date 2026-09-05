package relay

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dhyabi2/sail/nano"
	"github.com/dhyabi2/sail/token"
)

// RelayInfo is a relay as read from the ledger.
type RelayInfo struct {
	Account string
	Pub     [32]byte
	Country string
	ASN     uint32
	MinRate uint32
	Flags   uint16
	Desc    Descriptor
	Host    string // SNI / certificate name (a domain-looking name, never a bare IP)
	// Unlisted marks a bridge: an entry relay that is not on the ledger, learned
	// from a bridge line passed out of band. A censor reading the ledger cannot
	// find its address to block it.
	Unlisted bool
	Secret   [16]byte // bridge secret (from the bridge line); zero for listed relays
}

// BridgeLine formats an unlisted relay for out-of-band sharing:
// sail-bridge:<account>:<ip>:<port>:<certfp-hex>:<host>
func (ri *RelayInfo) BridgeLine() string {
	s := fmt.Sprintf("sail-bridge:%s:%s:%d:%s:%s", ri.Account, ri.Desc.IP, ri.Desc.Port, hex.EncodeToString(ri.Desc.CertFP[:]), ri.Host)
	if ri.Secret != ([16]byte{}) {
		s += ":" + hex.EncodeToString(ri.Secret[:])
	}
	return s
}

// ParseBridgeLine is the inverse of BridgeLine. Flags: public entry, no exit
// (a bridge is used as the first hop only).
func ParseBridgeLine(line string) (*RelayInfo, error) {
	f := strings.Split(strings.TrimSpace(line), ":")
	if (len(f) != 6 && len(f) != 7) || f[0] != "sail-bridge" {
		return nil, errors.New("bridge line must be sail-bridge:<account>:<ip>:<port>:<certfp>:<host>[:<secret>]")
	}
	pub, err := nano.AddressToPubkey(f[1])
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(f[2])
	port, err := strconv.Atoi(f[3])
	if ip == nil || err != nil || port <= 0 || port > 65535 {
		return nil, errors.New("bridge line: bad ip or port")
	}
	fp, err := hex.DecodeString(f[4])
	if err != nil || len(fp) != 6 {
		return nil, errors.New("bridge line: bad certificate fingerprint")
	}
	ri := &RelayInfo{Account: f[1], Pub: pub, Country: "XX", Flags: token.FlagPublic, Desc: Descriptor{IP: ip, Port: uint16(port)}, Host: f[5], Unlisted: true}
	copy(ri.Desc.CertFP[:], fp)
	if len(f) == 7 {
		sec, err := hex.DecodeString(f[6])
		if err != nil || len(sec) != 16 {
			return nil, errors.New("bridge line: bad secret")
		}
		copy(ri.Secret[:], sec)
	}
	return ri, nil
}

// Registry caches the on-ledger relay list.
type Registry struct {
	Client    *nano.Client
	Treasury  string
	CacheFile string // if set, the last good relay list is kept here and loaded when the ledger is unreachable
	mu        sync.RWMutex
	relays    map[string]*RelayInfo
	bridges   map[string]*RelayInfo    // unlisted entries; survive Refresh
	gossip    map[string]*SignedRecord // signed records learned from peers; ledger and bridges win
	unpaid    map[string]bool          // relays that skipped the (optional) levy
	weights   map[string]float64       // legacy combined routing weight (rewards.go)
	ageTerm   map[string]float64       // Age/maxAge per relay, from the daily ledger table
	perfTerm  map[string]float64       // Perf/maxPerf per relay
	loaded    time.Time
}

// Refresh replays the ledger and rebuilds the list.
func (r *Registry) Refresh(ctx context.Context) error {
	st, err := token.NewIndexer(r.Client, r.Treasury).Run(ctx)
	if err != nil {
		return err
	}
	m := map[string]*RelayInfo{}
	for _, rel := range st.Relays {
		// Every registration stays listed, however old. A relay that never
		// upgrades publishes no heartbeat, and dropping it here would take
		// it out of the network (see COMPATIBILITY.md). Whether a relay is
		// actually there is decided by probes and gossip, not by block age.
		d, ok := DecodeDescriptor(rel.Descriptor)
		if !ok {
			continue
		}
		pub, err := nano.AddressToPubkey(rel.Account)
		if err != nil {
			continue
		}
		m[rel.Account] = &RelayInfo{Account: rel.Account, Pub: pub, Country: rel.Country, ASN: rel.ASN, MinRate: rel.MinRate, Flags: rel.Flags, Desc: d}
	}
	r.mu.Lock()
	r.relays, r.loaded = m, time.Now()
	r.mu.Unlock()
	r.saveCache()
	return nil
}

type cachedRelay struct {
	Account string `json:"account"`
	Country string `json:"cc"`
	ASN     uint32 `json:"asn"`
	MinRate uint32 `json:"rate"`
	Flags   uint16 `json:"flags"`
	Desc    string `json:"desc"`
	Host    string `json:"host,omitempty"`
}

func (r *Registry) saveCache() {
	if r.CacheFile == "" {
		return
	}
	var list []cachedRelay
	for _, ri := range r.All() {
		d := ri.Desc.Encode()
		list = append(list, cachedRelay{ri.Account, ri.Country, ri.ASN, ri.MinRate, ri.Flags, hex.EncodeToString(d[:]), ri.Host})
	}
	data, _ := json.MarshalIndent(list, "", "  ")
	os.WriteFile(r.CacheFile, data, 0o600)
}

// LoadCache fills the registry from CacheFile without touching the network.
// Returns the number of relays loaded.
func (r *Registry) LoadCache() int {
	if r.CacheFile == "" {
		return 0
	}
	data, err := os.ReadFile(r.CacheFile)
	if err != nil {
		return 0
	}
	var list []cachedRelay
	if json.Unmarshal(data, &list) != nil {
		return 0
	}
	m := map[string]*RelayInfo{}
	for _, c := range list {
		db, err := hex.DecodeString(c.Desc)
		if err != nil || len(db) != 12 {
			continue
		}
		var a [12]byte
		copy(a[:], db)
		d, ok := DecodeDescriptor(a)
		if !ok {
			continue
		}
		pub, err := nano.AddressToPubkey(c.Account)
		if err != nil {
			continue
		}
		m[c.Account] = &RelayInfo{Account: c.Account, Pub: pub, Country: c.Country, ASN: c.ASN, MinRate: c.MinRate, Flags: c.Flags, Desc: d, Host: c.Host}
	}
	r.mu.Lock()
	if r.relays == nil {
		r.relays = m
	}
	r.mu.Unlock()
	return len(m)
}

// Get returns a relay by account (nil if unknown).
func (r *Registry) Get(acct string) *RelayInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if b := r.bridges[acct]; b != nil {
		if v := r.relays[acct]; v != nil {
			c := *v
			c.Unlisted, c.Desc, c.Secret = true, b.Desc, b.Secret
			if b.Host != "" {
				c.Host = b.Host
			}
			return &c
		}
		return r.bridgeWithRecordLocked(b)
	}
	if v := r.relays[acct]; v != nil {
		return v
	}
	if rec := r.gossip[acct]; rec != nil {
		if ri, err := rec.Verify(time.Now()); err == nil {
			return ri
		}
	}
	return nil
}

// All returns relays (ledger and bridges) sorted by account.
func (r *Registry) All() []*RelayInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*RelayInfo, 0, len(r.relays)+len(r.bridges))
	for _, v := range r.relays {
		if b := r.bridges[v.Account]; b != nil {
			// listed relay handed out as a bridge too: keep the ledger record
			// (flags, country, rate), take the bridge's marker and TLS name
			c := *v
			c.Unlisted, c.Desc, c.Secret = true, b.Desc, b.Secret
			if b.Host != "" {
				c.Host = b.Host
			}
			out = append(out, &c)
			continue
		}
		out = append(out, v)
	}
	for _, v := range r.bridges {
		if r.relays[v.Account] == nil {
			out = append(out, r.bridgeWithRecordLocked(v))
		}
	}
	out = append(out, r.gossipRelaysLocked()...)
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

// Add injects a relay: a bridge (Unlisted) is kept across Refresh; anything
// else is a static bootstrap entry (tests).
// LastSeen is when a relay last signed a gossip record that reached this
// registry (relays sign a fresh one every time they answer a gossip request),
// or zero when no record is known. A recent value is proof of life.
func (r *Registry) LastSeen(acct string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec := r.gossip[acct]; rec != nil {
		return time.Unix(rec.Time, 0)
	}
	return time.Time{}
}

func (r *Registry) Add(ri *RelayInfo) {
	r.mu.Lock()
	if ri.Unlisted {
		if r.bridges == nil {
			r.bridges = map[string]*RelayInfo{}
		}
		r.bridges[ri.Account] = ri
	} else {
		if r.relays == nil {
			r.relays = map[string]*RelayInfo{}
		}
		r.relays[ri.Account] = ri
	}
	r.mu.Unlock()
}

// LoadBridges reads bridge lines (one per line, # comments) from path.
func (r *Registry) LoadBridges(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ri, err := ParseBridgeLine(line)
		if err != nil {
			return n, err
		}
		r.Add(ri)
		n++
	}
	return n, nil
}

func short(a string) string {
	if len(a) > 16 {
		return a[:16] + "…"
	}
	return a
}

// Harbour returns the public relay whose endpoint a home node's descriptor points at.
func (r *Registry) Harbour(home *RelayInfo) *RelayInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, v := range r.relays {
		if v.Flags&token.FlagHome == 0 && v.Desc.Addr() == home.Desc.Addr() {
			return v
		}
	}
	return nil
}

// SetUnpaid replaces the set of levy-delinquent relays.
func (r *Registry) SetUnpaid(u map[string]bool) {
	r.mu.Lock()
	r.unpaid = u
	r.mu.Unlock()
}

// Unpaid reports whether a relay is excluded for not paying its levy.
func (r *Registry) Unpaid(acct string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.unpaid[acct]
}

// RefreshCompliance recomputes the last settled epoch's table and updates the
// unpaid set. Call once a day; every node reaches the same answer.
func (r *Registry) RefreshCompliance(ctx context.Context) error {
	e := CurrentEpoch() - 2 // e's payouts were due in e+1, which has closed
	t, err := ComputeEpoch(ctx, r.Client, r.Treasury, e)
	if err != nil {
		return err
	}
	u := map[string]bool{}
	for acct := range t.Relays {
		if !t.Compliant(acct) {
			u[acct] = true
		}
	}
	r.SetUnpaid(u)
	return nil
}

// Listed reports whether an account is registered on the ledger (as opposed
// to known only from gossip or a bridge line). Money only flows to listed relays.
func (r *Registry) Listed(acct string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.relays[acct] != nil
}

// SetWeights replaces the routing weights derived from the ledger.
func (r *Registry) SetWeights(w map[string]float64) {
	r.mu.Lock()
	r.weights = w
	r.mu.Unlock()
}

// RewardWeight is the seniority/performance multiplier a client applies when
// choosing relays: 60 % age, 40 % performance, floor 0.25 so new relays still
// receive enough traffic to build a record. Rewards flow as real traffic and
// real payments, which cannot be farmed without serving real users.
func (r *Registry) RewardWeight(acct string) float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if w, ok := r.weights[acct]; ok {
		return w
	}
	return 0.25
}

// RefreshScores recomputes yesterday's ledger table and turns it into
// routing weights. Every node reaches the same answer; nothing is paid.
func (r *Registry) RefreshScores(ctx context.Context) error {
	e := CurrentEpoch() - 1
	t, err := ComputeEpoch(ctx, r.Client, r.Treasury, e)
	if err != nil {
		return err
	}
	var maxAge, maxPerf float64
	for _, row := range t.Relays {
		maxAge = math.Max(maxAge, float64(row.Age))
		maxPerf = math.Max(maxPerf, row.Perf)
	}
	w := map[string]float64{}
	ages, perfs := map[string]float64{}, map[string]float64{}
	for acct, row := range t.Relays {
		v := 0.25
		if maxAge > 0 {
			v += 0.6 * float64(row.Age) / maxAge
			ages[acct] = float64(row.Age) / maxAge
		}
		if maxPerf > 0 {
			v += 0.4 * row.Perf / maxPerf
			perfs[acct] = row.Perf / maxPerf
		}
		w[acct] = v
	}
	r.SetWeights(w)
	r.mu.Lock()
	r.ageTerm, r.perfTerm = ages, perfs
	r.mu.Unlock()
	return nil
}

// RewardTerm is the lottery weight of a relay in one of the two draws:
// "age" (seniority) or "perf" (performance), normalised to the best relay,
// with a floor so new relays are drawn often enough to build a record.
// Clients hold the 60/40 draw.
func (r *Registry) RewardTerm(acct, mode string) float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var v float64
	if mode == "age" {
		v = r.ageTerm[acct]
	} else {
		v = r.perfTerm[acct]
	}
	return 0.25 + v
}
