package client

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	stdErrors "errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	mathrand "math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dhyabi2/sail/nano"
	"github.com/dhyabi2/sail/relay"
	"github.com/dhyabi2/sail/shape"
	"github.com/dhyabi2/sail/token"
)

const Treasury = "nano_1cexacexqrr51coik9eeqzebmfej7g6chrd1eygmzm9ad86qek84pnhpda1t"

func dataDir() string {
	if d := os.Getenv("SAIL_HOME"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".sail")
}

func loadKey() *nano.Key {
	p := os.Getenv("SAIL_WALLET")
	if p == "" {
		p = filepath.Join(dataDir(), "wallet.json")
	}
	var wf struct {
		Seed  string `json:"seed"`
		Index uint32 `json:"index"`
	}
	data, err := os.ReadFile(p)
	if err != nil {
		log.Fatalf("no wallet at %s (run: sail wallet new)", p)
	}
	if err := json.Unmarshal(data, &wf); err != nil {
		log.Fatal(err)
	}
	seed, _ := hex.DecodeString(wf.Seed)
	k, err := nano.DeriveKey(seed, wf.Index)
	if err != nil {
		log.Fatal(err)
	}
	return k
}

// chainState is the wallet's cached frontier/balance (see nano.ChainState).
func chainState(k *nano.Key) *nano.ChainState {
	return nano.LoadChainState(filepath.Join(dataDir(), "chain-"+k.Address[len(k.Address)-8:]+".json"))
}

// bootstrapResolver answers name lookups over a protected socket to a public
// resolver. Inside the app the system resolver is the tunnel itself, which
// cannot answer before a circuit exists, so the first ledger call would
// otherwise fail on "no such host".
var bootstrapResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
	d := net.Dialer{Timeout: 8 * time.Second, Control: relay.DialControl}
	return d.DialContext(ctx, "udp", "1.1.1.1:53")
}}

func newNano() *nano.Client {
	c := nano.NewClient()
	c.HTTP = &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 15 * time.Second, Control: relay.DialControl, Resolver: bootstrapResolver}).DialContext, DisableKeepAlives: true}}
	if k := os.Getenv("NANO_RPC_KEY"); k != "" {
		c.APIKey = k
	}
	return c
}

type clientOpts struct {
	hops    int
	exitCC  string
	exclude map[string]bool // exit countries the user refuses
	anchor  *big.Int        // raw XNO per prepaid anchor
	rate    uint32          // max price accepted on any hop (RateUnitRaw per MiB); 0 = 3x the median
	timeout time.Duration
	regDir  string
	freeTag string
	entry   string
	avoid   map[string]bool // relays never used in a path (the home node itself, its harbour)
}

// manager builds circuits, pays anchors, tracks local relay scores.
type manager struct {
	key     *nano.Key
	nc      *nano.Client
	reg     *relay.Registry
	opts    clientOpts
	mu      sync.Mutex
	score   map[string]float64
	scoreAt map[string]time.Time
	rtt     map[string]time.Duration // measured TCP connect time per public relay
	cur     *relay.Circuit
	drain   bool                          // the current circuit is nearly out of prepaid quota: new streams get a fresh one
	topMu   sync.Mutex                    // one top-up at a time
	live    atomic.Pointer[relay.Circuit] // same as cur, readable without m.mu
	tag     [32]byte
	entry   *relay.RelayInfo
	payment []byte // signed blocks of the current anchor (firewall mode)
	paidTo  string // relay the current anchor was paid to; a different entry needs a new anchor
	stealth bool   // every Nano RPC call goes through the circuit; none before one exists
	// censored: bridges are the only entries, nothing is probed or fetched
	// from listed relays before a circuit exists, and the ledger is never
	// contacted directly, not even on first run (the bridge grant covers it).
	censored bool
	skip     map[string]bool // hops that failed during the current build; not retried in the same build
	lastFail time.Time       // when the last build gave up; callers back off for a few seconds
	rotate   time.Duration
	anchors  map[string][]time.Time // recent anchor payments per entry: an entry that keeps claiming "quota exhausted" is not paid again
	// directBootstrap lets a stealth client reach the ledger directly while it
	// has no cached chain state at all (first run on a fresh device).
	directBootstrap bool
	fundsWatch      func()       // stops the confirmation watch while waiting for funds
	faucetAt        time.Time    // last faucet claim (one per day)
	polling         bool         // startFundsPoll is running
	mu2             sync.Mutex   // guards effCap alone, so it can be read while m.mu is held
	effCap          uint32       // the price cap the last path selection actually used
	stage           atomic.Value // what the client is doing right now, for screens
}

// dialViaCircuit is the transport for Nano RPC in stealth mode: the request
// leaves through the exit of the live circuit, so the local network sees only
// the TLS session to the entry relay. With no circuit up it fails at once and
// the caller falls back to its caches (chain state, relay list).
func (m *manager) dialViaCircuit(ctx context.Context, network, addr string) (net.Conn, error) {
	c := m.live.Load()
	if c == nil || c.Closed() {
		if m.directBootstrap {
			// first run: nothing cached yet, so one direct call is the only way to learn our balance
			return (&net.Dialer{Timeout: 15 * time.Second, Control: relay.DialControl, Resolver: bootstrapResolver}).DialContext(ctx, network, addr)
		}
		return nil, errors("stealth: no circuit yet (using cached state)")
	}
	st, err := c.Open(addr, 20*time.Second)
	if err != nil {
		return nil, err
	}
	return streamConn{st}, nil
}

func (m *manager) scoreOf(a string) float64 {
	if s, ok := m.score[a]; ok {
		// scores drift back toward neutral (0.7) at 0.05 per minute, so a relay
		// that failed transiently is retried instead of shunned forever
		if t, ok := m.scoreAt[a]; ok {
			s += 0.05 * time.Since(t).Minutes() * sign(0.7-s)
			if (s > 0.7) == (m.score[a] < 0.7) {
				s = 0.7
			}
		}
		return s
	}
	return 0.7
}

func sign(x float64) float64 {
	if x < 0 {
		return -1
	}
	return 1
}

func (m *manager) mark(a string, ok bool) {
	v := 0.0
	if ok {
		v = 1
	}
	m.score[a] = 0.9*m.scoreOf(a) + 0.1*v
	if m.scoreAt == nil {
		m.scoreAt = map[string]time.Time{}
	}
	m.scoreAt[a] = time.Now()
}

// choosePath: distinct country and ASN per hop, exit must have FlagExit, entry sticky.
func (m *manager) choosePath() ([]*relay.RelayInfo, error) {
	all := m.reg.All()
	if len(all) < m.opts.hops {
		return nil, fmt.Errorf("only %d relays on the ledger, need %d", len(all), m.opts.hops)
	}
	// Proof of life first: a relay counts as alive when it is a bridge we
	// know, answered our probe, or signed a gossip record in the last three
	// hours. Dead registrations stay on the ledger (they must, so that a
	// relay which never upgrades is never dropped); they must not cost a
	// user a 6-second timeout each. Only when that leaves too few relays do
	// unknown ones come back in. Every judgement below is this client's own
	// measurement: no list, no authority, nobody to ask.
	alive := func(r *relay.RelayInfo) bool {
		_, probed := m.rtt[r.Account]
		return r.Unlisted || probed || time.Since(m.reg.LastSeen(r.Account)) < 3*time.Hour
	}
	liveCount := 0
	for _, r := range all {
		if alive(r) {
			liveCount++
		}
	}
	requireAlive := liveCount >= m.opts.hops+1
	// Price: the customer is the buyer. Every hop must be at or under the
	// cap (by default five times the median published price, so the cap
	// follows the market rather than a number baked into a binary), and the
	// draw is weighted by (median / price)^2, so a relay that doubles its
	// price gets a quarter of the traffic. That, plus the fact that anyone
	// can register a cheaper relay, is what keeps a cartel from holding.
	//
	// The median counts only relays that are actually there. A registration
	// costs one raw, so a thousand records naming a price of nothing would
	// otherwise drag the median down and put every real relay over the cap,
	// which would empty the network at almost no cost to the attacker.
	priced := all
	if requireAlive {
		priced = all[:0:0]
		for _, r := range all {
			if alive(r) {
				priced = append(priced, r)
			}
		}
	}
	median, cap := m.priceCap(priced)
	// Everything the client would use if price were no object.
	candidates := all[:0:0]
	for _, r := range all {
		if requireAlive && !alive(r) {
			continue
		}
		if m.scoreOf(r.Account) >= 0.3 && !m.opts.avoid[r.Account] && !m.skip[r.Account] {
			candidates = append(candidates, r)
		}
	}
	// A price cap may never empty the network. Registrations are cheap and
	// permanent, so a pile of stale or forged records naming a low price
	// drags the median down until every relay that is really there looks
	// expensive, and a client that trusted the cap would refuse to connect
	// at all. When the cap admits fewer relays than a circuit needs, it is
	// widened to the cheapest ones that exist: still a cap, still ordered by
	// price, but never a reason to have no network (RULES.md rule 4).
	if m.opts.rate == 0 {
		var rates []int
		for _, r := range candidates {
			rates = append(rates, int(r.MinRate))
		}
		sort.Ints(rates)
		if need := m.opts.hops; len(rates) >= need {
			admits := 0
			for _, x := range rates {
				if uint32(x) <= cap {
					admits++
				}
			}
			if admits < need {
				if widened := uint32(rates[need-1]); widened > cap {
					log.Printf("price cap %s XNO/MiB would leave %d relays, fewer than the %d a circuit needs; using %s instead", token.FormatXNO(token.RateToRaw(cap)), admits, need, token.FormatXNO(token.RateToRaw(widened)))
					cap = widened
				}
			}
		}
	}
	m.mu2.Lock()
	m.effCap = cap // the payment step refuses at the same price this one accepted, never a fresher, stricter one
	m.mu2.Unlock()
	usable := candidates[:0:0]
	for _, r := range candidates {
		if r.Unlisted || r.MinRate <= cap {
			usable = append(usable, r) // a bridge was chosen by the user out of band: the market's price cap does not veto it
		}
	}
	if len(usable) < m.opts.hops {
		return nil, fmt.Errorf("only %d relays at or under %s XNO/MiB (median %s), need %d", len(usable), token.FormatXNO(token.RateToRaw(cap)), token.FormatXNO(token.RateToRaw(median)), m.opts.hops)
	}
	var path []*relay.RelayInfo
	used := map[string]bool{}
	// rarity: relays in under-represented countries/ASNs are preferred, so
	// demand (and income, and stake) flows to where the network is thin.
	cc, asn := map[string]int{}, map[uint32]int{}
	for _, r := range all {
		cc[r.Country]++
		asn[r.ASN]++
	}
	// The reward lottery: for this circuit, with probability 0.6 hops are drawn
	// in proportion to seniority, with probability 0.4 in proportion to
	// performance. Over many circuits the demand, and so the income, splits
	// 60/40 by construction; rarity and latency only re-order within a draw.
	mode := "perf"
	if mathrand.Float64() < 0.6 {
		mode = "age"
	}
	weight := func(r *relay.RelayInfo) float64 {
		w := m.scoreOf(r.Account) * m.reg.RewardTerm(r.Account, mode)
		w /= math.Sqrt(math.Sqrt(float64(cc[r.Country]) * float64(asn[r.ASN]))) // rarity, dampened
		if median > 0 {                                                         // cheaper relays get more of the demand
			price := float64(r.MinRate)
			if price < float64(median)/4 {
				price = float64(median) / 4 // a free or near-free relay is not handed everything
			}
			w *= (float64(median) / price) * (float64(median) / price)
		}
		if rtt, ok := m.rtt[r.Account]; ok && rtt > 0 { // nearer first, dampened
			w /= math.Sqrt(math.Sqrt(rtt.Seconds()*10 + 0.1))
		}
		return w
	}
	// Speed without choosing deterministically: the entry is drawn with
	// weight 1/rtt among the candidates (a relay twice as far gets half the
	// draws, never zero), and middle and exit are drawn with a strong
	// preference for the same region as each other, so the two inter-relay
	// hops are short. Everything stays a weighted random draw, so a relay
	// cannot win a client's path by being fast alone.
	near := func(r *relay.RelayInfo) float64 {
		if rtt, ok := m.rtt[r.Account]; ok && rtt > 0 {
			return 1 / (rtt.Seconds() + 0.02)
		}
		return 1 / 0.2
	}
	sameRegion := func(a, b *relay.RelayInfo) bool { return continentOf(a.Country) == continentOf(b.Country) }
	// pick draws one candidate at random in proportion to its weight
	// (argmax would hand every circuit to the single best-scored relay).
	var bias func(*relay.RelayInfo) float64 // per-draw multiplier (nearness, region)
	pick := func(pred func(*relay.RelayInfo) bool) *relay.RelayInfo {
		var cands []*relay.RelayInfo
		var ws []float64
		total := 0.0
		for _, r := range usable {
			if used[r.Account] || !pred(r) {
				continue
			}
			w := weight(r)
			if bias != nil {
				w *= bias(r)
			}
			if w <= 0 {
				continue
			}
			cands = append(cands, r)
			ws = append(ws, w)
			total += w
		}
		if len(cands) == 0 {
			return nil
		}
		x := mathrand.Float64() * total
		for i, w := range ws {
			x -= w
			if x <= 0 {
				return cands[i]
			}
		}
		return cands[len(cands)-1]
	}
	diverse := func(r *relay.RelayInfo) bool {
		if r.Flags&token.FlagHome != 0 {
			return false // a home node is reached only through its harbour, never picked as a middle or exit
		}
		for _, p := range path {
			if p.Country == r.Country || (p.ASN != 0 && p.ASN == r.ASN) {
				return false
			}
		}
		return true
	}
	// entry (sticky guard; --entry pins it)
	if m.opts.entry != "" {
		if e := m.reg.Get(m.opts.entry); e != nil {
			m.entry = e
		}
	}
	if m.entry != nil && m.reg.Get(m.entry.Account) != nil && m.scoreOf(m.entry.Account) >= 0.3 && (m.reg.Get(m.entry.Account).Unlisted || (!m.censored && (len(m.bridges()) == 0 || !m.canAfford(m.opts.anchor)))) {
		path = append(path, m.reg.Get(m.entry.Account))
	} else {
		// entry must be directly reachable; a bridge (unlisted, unblockable by
		// ledger reading) wins over a public relay whenever one is usable and
		// we can pay it (a new entry means a new anchor)
		var e *relay.RelayInfo
		bias = near
		if m.censored || m.canAfford(m.opts.anchor) || (m.entry != nil && m.entry.Unlisted) {
			e = pick(func(r *relay.RelayInfo) bool { return r.Unlisted })
		}
		if e == nil && m.censored {
			// Every bridge is gone or blocked. A listed relay as entry is
			// visible to a censor who reads the ledger, but a dead network
			// protects nobody: the client says so and connects anyway. The
			// network therefore outlives its bridge operators.
			if m.canAfford(m.opts.anchor) {
				log.Printf("no bridge is reachable: using a listed relay as entry (visible on the ledger; add a bridge line when you have one)")
			}
		}
		if e == nil {
			e = pick(func(r *relay.RelayInfo) bool { return r.Flags&token.FlagHome == 0 })
		}
		if e == nil {
			return nil, errors("no entry relay available")
		}
		path = append(path, e)
		m.entry = e
	}
	bias = nil
	used[path[0].Account] = true
	// Exit first, then middles near the exit: the two long hops of a circuit
	// are the ones between relays, so they are the ones kept short.
	var exit *relay.RelayInfo
	if m.opts.hops > 1 {
		exit = pick(func(r *relay.RelayInfo) bool {
			return r.Flags&token.FlagExit != 0 && diverse(r) && !m.opts.exclude[strings.ToUpper(r.Country)] && (m.opts.exitCC == "" || r.Country == strings.ToUpper(m.opts.exitCC))
		})
		if exit == nil {
			exit = pick(func(r *relay.RelayInfo) bool { return r.Flags&token.FlagExit != 0 && r.Flags&token.FlagHome == 0 })
		}
		if exit == nil {
			return nil, errors("no exit relay available")
		}
		used[exit.Account] = true
		bias = func(r *relay.RelayInfo) float64 {
			if sameRegion(r, exit) || sameRegion(r, path[0]) {
				return 4
			}
			return 1
		}
	}
	for len(path) < m.opts.hops-1 {
		r := pick(diverse)
		if r == nil {
			r = pick(func(r *relay.RelayInfo) bool { return r.Flags&token.FlagHome == 0 }) // relax diversity
		}
		if r == nil {
			return nil, errors("not enough relays for the requested hops")
		}
		path = append(path, r)
		used[r.Account] = true
	}
	bias = nil
	if exit != nil {
		path = append(path, exit)
	}
	return path, nil
}

// priceCap returns the median published price and the cap a hop must meet:
// the user's --rate when set, else three times the median.
func (m *manager) priceCap(all []*relay.RelayInfo) (median, cap uint32) {
	var rates []int
	for _, r := range all {
		if r.MinRate > 0 {
			rates = append(rates, int(r.MinRate))
		}
	}
	if len(rates) > 0 {
		sort.Ints(rates)
		median = uint32(rates[len(rates)/2])
	}
	cap = m.opts.rate
	if cap == 0 {
		// Five times the median, not three: we lowered our own default price
		// on 2026-09-05, and a relay still running the previous default
		// (four times the new one) must stay eligible. Nobody who never
		// upgrades may be priced out of the network by our own change
		// (COMPATIBILITY.md). It is still a cap: a relay asking more than
		// five times what the market asks is skipped.
		cap = 5 * median
		if cap == 0 {
			cap = math.MaxUint32
		}
	}
	return median, cap
}

type errors string

func (e errors) Error() string { return string(e) }

// anchorTo prepays the entry relay with a plain XNO send; the block hash is
// the circuit tag, and the signed block is handed to the relay so it can
// publish and verify it without the client touching any RPC.
func (m *manager) anchorTo(entry *relay.RelayInfo) error {
	// The cap the path was chosen with, not a fresh one: recomputing it here
	// over every ledger record let stale cheap registrations veto a payment
	// to a relay the client had just decided to use, which stopped new
	// clients connecting at all. A bridge is exempt: the user chose it out of
	// band, so the market's opinion of its price is not a reason to refuse.
	m.mu2.Lock()
	cap := m.effCap
	m.mu2.Unlock()
	if cap == 0 {
		_, cap = m.priceCap(m.reg.All())
	}
	if !entry.Unlisted && entry.MinRate > cap {
		return fmt.Errorf("entry %s wants %s XNO/MiB, above your cap", entry.Account, token.FormatXNO(token.RateToRaw(entry.MinRate)))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if !m.canAfford(m.opts.anchor) {
		m.pocket() // a faucet or a friend may just have paid us
	}
	if !m.canAfford(m.opts.anchor) {
		m.setStage("Waiting for XNO")
		return errors("wallet has no XNO yet: send it a little (0.0005 XNO buys about 25 MB), it connects by itself when the funds arrive")
	}
	m.setStage("Paying the entry relay")
	if m.anchors == nil {
		m.anchors = map[string][]time.Time{}
	}
	var recent []time.Time
	for _, t := range m.anchors[entry.Account] {
		if time.Since(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	if len(recent) >= 3 {
		m.mark(entry.Account, false)
		return fmt.Errorf("entry %s asked for a fourth payment within an hour; refusing and routing around it", short(entry.Account))
	}
	m.anchors[entry.Account] = recent
	acct := &nano.Account{Key: m.key, Client: m.nc, State: chainState(m.key)}
	// Pocket anything pending first: a fresh wallet is funded by a send it has
	// not received yet, and receiving is what opens the account.
	if n, err := acct.ReceiveAll(ctx); err == nil && n > 0 {
		log.Printf("received %d pending payment(s)", n)
	} else if err != nil {
		log.Printf("pocket: %v", err)
	}
	h, blk, err := acct.SendBlock(ctx, entry.Account, m.opts.anchor)
	var pe *nano.PublishError
	if err != nil && stdErrors.As(err, &pe) && blk != nil {
		log.Printf("payment signed offline; the entry relay will publish it")
		err = nil
	}
	if err != nil {
		return err
	}
	tb, _ := hex.DecodeString(h)
	copy(m.tag[:], tb)
	m.payment, _ = json.Marshal(blk.JSON())
	m.entry, m.paidTo = entry, entry.Account
	m.anchors[entry.Account] = append(m.anchors[entry.Account], time.Now()) // only payments that happened count toward the cap
	m.saveAnchor()
	log.Printf("paid %s XNO → %s (tag %s)", token.FormatXNO(m.opts.anchor), entry.Account, h[:8])
	m.setStage("Paid the entry relay; waiting for the ledger")
	return nil
}

// The current anchor (entry relay, payment tag, signed block) is kept on
// disk: a restart reuses the prepaid quota instead of paying again.
type anchorFile struct {
	Entry   string          `json:"entry"`
	Tag     string          `json:"tag"`
	Payment json.RawMessage `json:"payment,omitempty"`
}

// One anchor file per wallet: a client and a home node sharing SAIL_HOME
// must not pick up each other's prepaid quota.
func (m *manager) anchorPath() string {
	return filepath.Join(dataDir(), "anchor-"+m.key.Address[len(m.key.Address)-8:]+".json")
}

func (m *manager) saveAnchor() {
	data, _ := json.MarshalIndent(anchorFile{Entry: m.entry.Account, Tag: strings.ToUpper(hex.EncodeToString(m.tag[:])), Payment: m.payment}, "", "  ")
	os.WriteFile(m.anchorPath(), data, 0o600)
}

func (m *manager) loadAnchor() {
	data, err := os.ReadFile(m.anchorPath())
	if err != nil {
		return
	}
	var a anchorFile
	if json.Unmarshal(data, &a) != nil || len(a.Tag) != 64 {
		return
	}
	e := m.reg.Get(a.Entry)
	if e == nil {
		return
	}
	tb, _ := hex.DecodeString(a.Tag)
	copy(m.tag[:], tb)
	m.entry, m.payment, m.paidTo = e, a.Payment, e.Account
	log.Printf("reusing prepaid anchor %s… at %s", a.Tag[:8], short(a.Entry))
}

// circuit returns a healthy circuit, building and paying as needed.
func (m *manager) circuit() (*relay.Circuit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur != nil && !m.cur.Closed() && !m.drain && time.Since(m.cur.Built) < m.rotateAfter() {
		return m.cur, nil
	}
	if m.cur != nil {
		if m.cur.Closed() && m.cur.LinkLost && len(m.cur.Path) > 0 {
			m.mark(m.cur.Path[0].Account, false) // the entry vanished or is restarting: next build uses another one
		}
		// Rotate without cutting anyone off: the old circuit keeps serving
		// the streams it has and closes once they are gone.
		go drainCircuit(m.cur)
		m.cur = nil
		m.drain = false
	}
	if time.Since(m.lastFail) < 5*time.Second {
		return nil, errors("circuit: retrying shortly")
	}
	m.skip = map[string]bool{}
	for attempt := 0; attempt < 4; attempt++ {
		path, err := m.choosePath()
		if err != nil {
			return nil, err
		}
		if m.tag != ([32]byte{}) && m.opts.freeTag == "" && path[0].Account != m.paidTo {
			m.tag = [32]byte{} // the anchor belongs to another entry
		}
		if m.tag == ([32]byte{}) {
			if m.opts.freeTag != "" {
				tb, _ := hex.DecodeString(m.opts.freeTag)
				copy(m.tag[:], tb)
			} else {
				if err := m.anchorTo(path[0]); err != nil {
					if m.censored && path[0].Unlisted && path[0].Secret != ([16]byte{}) {
						// No usable wallet state yet and no way to reach the ledger
						// directly: use the bridge's bootstrap grant with a random
						// tag, reach the ledger through this circuit, then pay.
						rand.Read(m.tag[:])
						m.payment = nil
						log.Printf("bootstrap: using the bridge's free grant for the first circuit")
					} else {
						return nil, err
					}
				} else {
					time.Sleep(4 * time.Second) // let the indexer see the blocks
				}
			}
		}
		names := make([]string, len(path))
		for i, p := range path {
			names[i] = fmt.Sprintf("%s(%s)", p.Country, short(p.Account))
		}
		log.Printf("building circuit: %s", strings.Join(names, " → "))
		m.setStage("Building circuit: hop 1 of " + fmt.Sprint(len(path)))
		t0 := time.Now()
		c, err := relay.Build(path, m.tag, m.opts.timeout, m.payment, func(pub, tag [32]byte) []byte { return relay.SignCreate(m.key, pub, tag) })
		if err != nil {
			if c != nil && c.Failed >= 0 {
				log.Printf("build failed at hop %d %s: %v", c.Failed, path[c.Failed].Account, err)
				// A hop whose upstream is still prepaying it ("warming up") is not at
				// fault, but retrying the same path is useless: nudge its score down so
				// the next attempt routes around it (scores drift back within minutes).
				m.mark(path[c.Failed].Account, false)
				if strings.Contains(err.Error(), "dial") || strings.Contains(err.Error(), "unknown relay") || strings.Contains(err.Error(), "not on the ledger") {
					// Nobody could even reach it: treat it as gone, not as flaky.
					// The score drifts back over a couple of hours, so a relay that
					// really returns is retried eventually.
					m.score[path[c.Failed].Account] = 0
				}
				if c.Failed > 0 {
					m.skip[path[c.Failed].Account] = true // try a different hop this time
				}
				if c.Failed == 0 && strings.Contains(err.Error(), "transient") {
					// The entry saw our payment but the ledger has not confirmed it yet:
					// same tag, short wait. Paying again here would burn XNO.
					time.Sleep(5 * time.Second)
				} else if c.Failed == 0 && strings.Contains(err.Error(), "not this relay") {
					m.tag = [32]byte{} // our anchor is at another entry; it stays valid there
				} else if c.Failed == 0 && (strings.Contains(err.Error(), "quota") || strings.Contains(err.Error(), "payment")) {
					m.tag = [32]byte{} // pay again next time
					os.Remove(m.anchorPath())
				}
			} else {
				log.Printf("build failed: %v", err)
			}
			if c != nil {
				c.Close()
			}
			continue
		}
		for _, p := range path {
			m.mark(p.Account, true)
		}
		mode := ""
		if path[len(path)-1].Flags&token.FlagFlow != 0 {
			mode = " (windowed streams)"
		}
		log.Printf("circuit built in %s: %s%s", time.Since(t0).Round(time.Millisecond), strings.Join(names, " → "), mode)
		m.setStage("Connected")
		c.Flow = path[len(path)-1].Flags&token.FlagFlow != 0 // windowed streams when the exit can
		c.OnQuota = func(q int64) { m.quotaLow(c, q) }
		m.cur = c
		m.rotate = 0 // fresh lifetime for the new circuit
		m.live.Store(c)
		go m.keepalive(c)
		go m.pocket()
		return c, nil
	}
	m.lastFail = time.Now()
	return nil, errors("could not build a circuit")
}

// drainCircuit closes c once its streams are done (or after a long grace).
func drainCircuit(c *relay.Circuit) {
	deadline := time.Now().Add(30 * time.Minute)
	for !c.Closed() && c.Streams() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
	}
	c.Close()
}

func (m *manager) keepalive(c *relay.Circuit) {
	for !c.Closed() {
		time.Sleep(15*time.Second + time.Duration(mathrand.Intn(12000))*time.Millisecond) // jittered: no fixed rhythm on the wire
		if bad := c.Ping(8 * time.Second); bad >= 0 {
			if time.Since(c.LastRecv()) < 10*time.Second {
				// The pong is queued behind a busy download; the circuit is
				// plainly alive, so a late pong is not a dead hop.
				continue
			}
			log.Printf("keepalive: hop %d (%s) failed (%s; last cell %s ago); rebuilding", bad, c.Path[bad].Account, c.PingErr(), time.Since(c.LastRecv()).Round(time.Millisecond))
			m.mu.Lock()
			m.mark(c.Path[bad].Account, false)
			m.mu.Unlock()
			c.Close()
			return
		}
		if !m.topMu.TryLock() {
			continue // a top-up is talking to the entry right now
		}
		q, err := c.QueryQuota(8 * time.Second)
		m.topMu.Unlock()
		if err == nil {
			need := relay.BytesFor(m.opts.anchor, token.RateToRaw(c.Path[0].MinRate)) / 4
			if q < need {
				m.quotaLow(c, q)
			}
		}
	}
}

// quotaLow runs when the circuit's prepaid quota is nearly spent, from the
// keepalive or from the entry's own low-quota push. The circuit is topped
// up in place so no stream is cut; only if that fails do new streams move
// to a fresh circuit while this one drains.
func (m *manager) quotaLow(c *relay.Circuit, q int64) {
	if !m.topMu.TryLock() {
		return
	}
	defer m.topMu.Unlock()
	if c.Closed() {
		return
	}
	rate := c.BytesMoved() / int64(math.Max(time.Since(c.Built).Seconds(), 1))
	if need := relay.BytesFor(m.opts.anchor, token.RateToRaw(c.Path[0].MinRate)) / 4; q >= need && q >= 8<<20 && q >= rate*30 {
		return // plenty left: a stray notice
	}
	if rem, err := m.topUp(c); err == nil {
		log.Printf("quota topped up in place: %d MiB left on the circuit", rem>>20)
		return
	} else {
		log.Printf("quota low (%d bytes left) and top-up failed (%v): new streams get a fresh circuit", q, err)
	}
	// The tag is kept: a top-up that lands late still credits it, and the
	// next build tries it first, paying a new anchor only if the entry
	// says it is really spent.
	m.mu.Lock()
	if m.cur == c {
		m.drain = true
	}
	m.mu.Unlock()
}

// topUp pays the entry again for the running circuit. The amount covers
// about five minutes at the circuit's recent rate, never less than one
// anchor, never more than the wallet can spare.
func (m *manager) topUp(c *relay.Circuit) (int64, error) {
	if m.opts.freeTag != "" {
		return 0, errors("free tag: nothing to top up")
	}
	entry := c.Path[0]
	rateRaw := token.RateToRaw(entry.MinRate)
	if rateRaw.Sign() <= 0 {
		return 0, errors("entry is free: nothing to top up")
	}
	elapsed := time.Since(c.Built).Seconds()
	if elapsed < 1 {
		elapsed = 1
	}
	used := c.BytesMoved()
	want := int64(float64(used) / elapsed * 300) // five minutes of runway at the recent rate
	if limit := 10 * relay.BytesFor(m.opts.anchor, rateRaw); want > limit {
		want = limit // never more than ten anchors in one go
	}
	amount := new(big.Int).Mul(big.NewInt((want+(1<<20)-1)/(1<<20)), rateRaw)
	if amount.Cmp(m.opts.anchor) < 0 {
		amount.Set(m.opts.anchor)
	}
	_, bal, _, _, ok := chainState(m.key).Get()
	if !ok || bal.Sign() <= 0 {
		return 0, errors("wallet state unknown")
	}
	half := new(big.Int).Rsh(bal, 1)
	if amount.Cmp(half) > 0 {
		amount.Set(half)
	}
	if amount.Cmp(m.opts.anchor) < 0 && bal.Cmp(m.opts.anchor) >= 0 {
		amount.Set(m.opts.anchor)
	}
	if amount.Sign() <= 0 || bal.Cmp(amount) < 0 {
		return 0, errors("wallet has no XNO for a top-up")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	acct := &nano.Account{Key: m.key, Client: m.nc, State: chainState(m.key)}
	h, blk, err := acct.SendBlock(ctx, entry.Account, amount)
	var pe *nano.PublishError
	if err != nil && stdErrors.As(err, &pe) && blk != nil {
		err = nil // signed offline; the entry publishes it
	}
	if err != nil {
		return 0, err
	}
	payment, _ := json.Marshal(blk.JSON())
	rem, err := c.TopUp(payment, 2*time.Minute)
	if err != nil {
		return 0, err
	}
	log.Printf("paid %s XNO → %s (top-up %s)", token.FormatXNO(amount), entry.Account, h[:8])
	return rem, nil
}

func runClient(args []string) {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	socks := fs.String("socks", "127.0.0.1:1080", "SOCKS5 listen address")
	hops := fs.Int("hops", 3, "circuit length")
	exitCC := fs.String("exit-cc", "", "preferred exit country (optional)")
	excludeCC := fs.String("exclude-cc", "", "exit countries never to use, comma-separated (e.g. US,GB)")
	anchor := fs.String("anchor", "0.0005", "XNO per prepaid anchor")
	rate := fs.String("rate", "0", "max XNO per MiB you accept on any hop (0 = three times the median published price)")
	regDir := fs.String("registry-dir", "", "test mode: static relay descriptors directory")
	freeTag := fs.String("tag", "", "use an existing payment tag (fragment-B hash of a SAIL transfer to the entry) instead of paying")
	entry := fs.String("entry", "", "pin the entry relay account")
	stealth := new(bool)
	*stealth = true // always: no direct Nano RPC except the first-run bootstrap through Sailnet's endpoint
	dns := fs.String("dns", "127.0.0.1:5300", "answer DNS here by resolving through the circuit at the exit (empty = off)")
	status := fs.String("status", "127.0.0.1:1090", "JSON status endpoint for UIs and the browser extension (empty = off)")
	rpcURL := fs.String("rpc", "", "Nano RPC endpoint(s), comma-separated, tried in order (default: Sailnet's endpoint, then public nodes)")
	rpcKey := fs.String("rpc-key", "", "API key for a configured rpc.nano.to endpoint")
	allowHTTP := fs.Bool("allow-http", false, "let plain HTTP (port 80) through the tunnel; off by default because the exit could read it")
	capture := fs.Bool("capture", false, "whole-device mode: DNS sinkhole on :53 and listeners on :80/:443 that route every flow through the circuit by Host/SNI (needs administrator rights)")
	capAddrs := fs.String("capture-ports", "127.0.0.1:53,127.0.0.1:80,127.0.0.1:443", "with --capture: DNS sinkhole, HTTP and HTTPS listen addresses")
	subvert := fs.Bool("subvert-dns", false, "with --capture: point the operating system's resolver at 127.0.0.1 and restore it on exit")
	dnsUp := fs.String("dns-upstream", "1.1.1.1:53", "resolver the exit asks on your behalf")
	nickFlag := fs.String("nick", "", "nickname shown instead of your wallet address and device IPs in logs and status")
	*censoredFlag = true // always on
	bridge := fs.String("bridge", "", "bridge line(s) of unlisted entry relays, comma-separated (also read from SAIL_HOME/bridges.txt); bridges are preferred as entry")
	fs.Parse(args)
	AllowPlainHTTP = *allowHTTP
	if *rpcURL != "" || *rpcKey != "" {
		nano.ConfigureRPC(*rpcURL, *rpcKey)
	}
	// SAIL_TRACE=<file> records every TLS record of the client's relay
	// connections, as a censor on the path would see them.
	if tf := os.Getenv("SAIL_TRACE"); tf != "" {
		sink, err := shape.Create(tf)
		if err != nil {
			log.Fatalf("SAIL_TRACE: %v", err)
		}
		shape.SetDialHook(func(c net.Conn, site string) net.Conn {
			return shape.NewTap(c, sink, "live", site, os.Getenv("SAIL_SHAPE"))
		})
		log.Printf("tracing relay connections to %s", tf)
	}
	for _, line := range strings.Split(*bridge, ",") {
		if strings.TrimSpace(line) != "" {
			cliBridges = append(cliBridges, line)
		}
	}
	var m *manager
	if *stealth && *regDir == "" {
		key0 := EnsureWallet()
		_, noChain := os.Stat(filepath.Join(dataDir(), "chain-"+key0.Address[len(key0.Address)-8:]+".json"))
		m = newStealthManager(*hops, *exitCC, *anchor, *rate, *freeTag, noChain != nil)
		if noChain != nil {
			log.Printf("first run: ledger read through the entry relay until the wallet state is cached")
		}
	} else {
		m = newManager(*hops, *exitCC, *anchor, *rate, *regDir, *freeTag)
	}
	m.opts.entry = *entry
	SetNick(*nickFlag, m.key.Address)
	m.SetExcludeExit(*excludeCC)
	log.SetOutput(RedactingWriter{W: os.Stderr})
	if bs := m.bridges(); len(bs) > 0 {
		log.Printf("%d unlisted entry relay(s) (bridges) will be preferred as entry", len(bs))
	}
	m.SetCensored(true)
	if len(m.bridges()) == 0 {
		log.Fatal("no bridge known: add a bridge line (--bridge or bridges.txt)")
	}
	ln, err := net.Listen("tcp", *socks)
	if err != nil {
		log.Fatal(err)
	}
	if *dns != "" {
		go m.serveDNS(*dns, *dnsUp)
	}
	if *status != "" {
		go m.ServeStatus(*status)
	}
	if *capture {
		ca := strings.Split(*capAddrs, ",")
		if len(ca) != 3 {
			log.Fatal("--capture-ports needs three addresses: dns,http,https")
		}
		go m.serveSinkholeDNS(strings.TrimSpace(ca[0]), *dnsUp)
		go m.serveCapture(strings.TrimSpace(ca[1]), false)
		go m.serveCapture(strings.TrimSpace(ca[2]), true)
		if *subvert {
			revertDNS() // a previous run that died without restoring leaves a backup behind: repair first
			if err := subvertDNS(); err != nil {
				log.Fatal("capture: ", err)
			}
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			go func() { <-sig; revertDNS(); os.Exit(0) }()
			defer revertDNS()
		}
	}
	log.Printf("SOCKS5 proxy on %s (wallet %s)", *socks, m.key.Address)
	go func() { // an empty wallet: watch for the first payment through the entry
		for {
			if m.NeedsFunds() {
				m.EnsureFundsWatch()
			} else {
				m.StopFundsWatch()
			}
			time.Sleep(30 * time.Second)
		}
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go m.serveSocks(conn)
	}
}

// newStealthManager is newManager with every Nano RPC call routed through the
// live circuit. Before the first circuit exists nothing is sent: the relay
// list comes from the cache and the anchor payment is signed from the cached
// chain state and published by the entry relay. The local network never sees
// a Nano node's name or address.
func newStealthManager(hops int, exitCC, anchor, rate, freeTag string, direct bool) *manager {
	m := &manager{stealth: true, directBootstrap: direct, censored: true}
	nc := newNano()
	// The entry relay and the exit protect the ledger source with their own
	// limits, so the client need not pace itself like it would against a
	// public node: a first run's forty-odd reads take seconds, not minutes.
	nc.Budget = nano.NewBudget(4, 30)
	// Every ledger call goes through the circuit; before one exists (first
	// run, nothing cached) it goes to the entry relay on circuit 0, which
	// forwards it. The client never connects to anything but its entry.
	nc.HTTP = &http.Client{Timeout: 60 * time.Second, Transport: &stealthTransport{m: m,
		circuit: &http.Transport{DialContext: m.dialViaCircuit, TLSHandshakeTimeout: 20 * time.Second, MaxIdleConnsPerHost: 2, IdleConnTimeout: 90 * time.Second}}}
	init := newManagerWith(m, nc, hops, exitCC, anchor, rate, "", freeTag)
	log.Printf("ledger through the entry relay until a circuit is up, then through the circuit; payments signed offline")
	return init
}

// stealthTransport routes Nano RPC: through the live circuit when there is
// one, otherwise through the entry relay's CmdRPC channel.
type stealthTransport struct {
	m       *manager
	circuit http.RoundTripper
	mu      sync.Mutex
	entry   *relay.EntryRPC
}

func (t *stealthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if c := t.m.live.Load(); c != nil && !c.Closed() {
		return t.circuit.RoundTrip(req)
	}
	// No circuit: ask through the entry relay's ledger channel. That path is
	// inside the tunnel TLS, so it is as private as the circuit itself and
	// there is no reason to limit it to a first run: a wallet that ran dry
	// and was refilled must be able to notice the refill too.
	body, _ := io.ReadAll(req.Body)
	req.Body.Close()
	out, err := t.viaEntry(body)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(out)), Header: http.Header{"Content-Type": {"application/json"}}, Request: req, ContentLength: int64(len(out))}, nil
}

func (t *stealthTransport) viaEntry(body []byte) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entry == nil {
		bs := t.m.entryCandidates()
		if len(bs) == 0 {
			return nil, errors("no entry known for the first ledger read")
		}
		t.entry = relay.NewEntryRPC(bs[mathrand.Intn(len(bs))], 8*time.Second)
	}
	out, err := t.entry.Call(body, 60*time.Second)
	if os.Getenv("SAIL_DEBUG_RPC") != "" {
		log.Printf("ledger via entry: %s -> %d bytes err=%v: %.200s", body, len(out), err, out)
	}
	if err != nil {
		log.Printf("ledger via entry: %v", err)
		t.entry.Close()
		t.entry = nil
	}
	return out, err
}

func newManager(hops int, exitCC, anchor, rate, regDir, freeTag string) *manager {
	return newManagerWith(&manager{}, newNano(), hops, exitCC, anchor, rate, regDir, freeTag)
}

func newManagerWith(m *manager, nc *nano.Client, hops int, exitCC, anchor, rate, regDir, freeTag string) *manager {
	a, err := token.ParseXNO(anchor)
	if err != nil {
		log.Fatal(err)
	}
	r, err := token.RateFromXNO(rate)
	if err != nil {
		log.Fatal(err)
	}
	m.key, m.nc, m.score = EnsureWallet(), nc, map[string]float64{}
	m.opts = clientOpts{hops: hops, exitCC: exitCC, anchor: a, rate: r, timeout: 25 * time.Second, regDir: regDir, freeTag: freeTag}
	m.reg = &relay.Registry{Client: m.nc, Treasury: Treasury, CacheFile: filepath.Join(dataDir(), "registry.json")}
	if regDir != "" {
		if err := m.reg.LoadDir(regDir); err != nil {
			log.Fatal("registry-dir:", err)
		}
		log.Printf("%d relays in %s (test mode)", len(m.reg.All()), regDir)
		go func() {
			for {
				time.Sleep(5 * time.Second)
				m.reg.LoadDir(regDir)
			}
		}()
		return m
	}
	if n := m.reg.LoadCache(); n > 0 {
		log.Printf("%d relays from cache", n)
	}
	// Bridges come first: with one bridge line a client needs neither cache nor ledger.
	for _, line := range strings.Split(builtinBridges, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			cliBridges = append(cliBridges, line)
		}
	}
	for _, line := range cliBridges {
		ri, err := relay.ParseBridgeLine(line)
		if err != nil {
			log.Fatal(err)
		}
		m.reg.Add(ri)
	}
	if n, err := m.reg.LoadBridges(filepath.Join(dataDir(), "bridges.txt")); err == nil && n > 0 {
		log.Printf("%d bridge(s) from bridges.txt", n)
	}
	if err := m.reg.Refresh(context.Background()); err != nil {
		if len(m.reg.All()) == 0 {
			log.Fatal("registry: ", err, " (no cached relay list and no bridge: run once where the ledger is reachable, or add a bridge line)")
		}
		log.Printf("registry: %v; using the cached list", err)
	} else {
		log.Printf("%d relays on the ledger", len(m.reg.All()))
	}
	m.setStage("Measuring relays")
	m.measureRTT()
	m.loadAnchor()
	m.gossipBootstrap() // learn the relays' own signed records: exit flag, country, rate (bridges carry none)
	go func() {
		for {
			time.Sleep(2 * time.Minute)
			if m.stealth && (m.live.Load() == nil || m.live.Load().Closed()) {
				continue // in stealth mode the ledger is read only through a circuit, never directly
			}
			m.reg.Refresh(context.Background())
		}
	}()
	go func() { // seniority/performance weights: once a day, from the ledger, same answer everywhere
		time.Sleep(90 * time.Second)
		for {
			if m.stealth && (m.live.Load() == nil || m.live.Load().Closed()) {
				time.Sleep(2 * time.Minute)
				continue
			}
			if err := m.reg.RefreshScores(context.Background()); err != nil {
				log.Printf("rewards: %v", err)
			}
			time.Sleep(24 * time.Hour)
		}
	}()
	return m
}

// Minimal SOCKS5 (no auth, CONNECT, IPv4/domain).
func (m *manager) serveSocks(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 262)
	if _, err := io.ReadFull(conn, buf[:2]); err != nil || buf[0] != 5 {
		return
	}
	n := int(buf[1])
	io.ReadFull(conn, buf[:n])
	conn.Write([]byte{5, 0})
	if _, err := io.ReadFull(conn, buf[:4]); err != nil || buf[1] != 1 {
		return
	}
	var host string
	switch buf[3] {
	case 1:
		io.ReadFull(conn, buf[:4])
		host = net.IP(buf[:4]).String()
	case 3:
		io.ReadFull(conn, buf[:1])
		l := int(buf[0])
		io.ReadFull(conn, buf[:l])
		host = string(buf[:l])
	case 4:
		io.ReadFull(conn, buf[:16])
		host = net.IP(buf[:16]).String()
	default:
		return
	}
	io.ReadFull(conn, buf[:2])
	port := int(buf[0])<<8 | int(buf[1])
	target := net.JoinHostPort(host, strconv.Itoa(port))
	if ip := net.ParseIP(host); ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
		// A LAN or local destination (a Nano node on this network, a
		// printer, a router): nothing behind the exit can reach it, so it
		// is dialed directly. Only public destinations go through the tunnel.
		direct, err := (&net.Dialer{Timeout: 10 * time.Second, Control: relay.DialControl}).Dial("tcp", target)
		if err != nil {
			conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
			return
		}
		defer direct.Close()
		conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		done := make(chan struct{}, 2)
		go func() { io.Copy(direct, conn); direct.Close(); done <- struct{}{} }()
		go func() { io.Copy(conn, direct); done <- struct{}{} }()
		<-done
		return
	}
	if port == 80 && !AllowPlainHTTP {
		// Plain HTTP would leave the exit readable by its operator and every
		// network after it. Refused unless the operator explicitly allows it.
		conn.Write([]byte{5, 2, 0, 1, 0, 0, 0, 0, 0, 0}) // SOCKS: connection not allowed by ruleset
		log.Printf("refused plain HTTP (port 80): only encrypted destinations leave the exit; --allow-http overrides")
		return
	}
	c, err := m.circuit()
	if err != nil {
		log.Println("circuit:", err)
		conn.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	st, err := c.OpenOptimistic(target)
	if err != nil {
		log.Printf("open stream: %v", err) // never the destination
		conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	// Optimistic: tell the app "connected" now so its first bytes ride right
	// behind BEGIN, saving a full 3-hop round trip per connection.
	conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	done := make(chan struct{}, 2)
	go func() { io.Copy(Up(st), conn); st.Close(); done <- struct{}{} }()
	go func() { io.Copy(Down(conn), st); done <- struct{}{} }()
	<-done
}

// ---------------------------------------------------------------- fetch (test)

func runFetch(args []string) {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	hops := fs.Int("hops", 3, "circuit length")
	anchor := fs.String("anchor", "0.0005", "XNO per prepaid anchor")
	rate := fs.String("rate", "0", "max XNO per MiB on any hop (0 = three times the median published price)")
	regDir := fs.String("registry-dir", "", "test mode: static relay descriptors directory")
	freeTag := fs.String("tag", "", "use an existing payment tag instead of paying")
	entry := fs.String("entry", "", "pin the entry relay account")
	fs.Parse(args)
	if fs.NArg() < 1 {
		log.Fatal("usage: sailnode fetch <url>")
	}
	m := newManager(*hops, "", *anchor, *rate, *regDir, *freeTag)
	m.opts.entry = *entry
	c, err := m.circuit()
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		st, err := c.Open(addr, 20*time.Second)
		if err != nil {
			return nil, err
		}
		return streamConn{st}, nil
	}}
	resp, err := (&http.Client{Transport: tr, Timeout: 60 * time.Second}).Get(fs.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	fmt.Printf("%s via %d hops (exit %s %s)\n%s\n", resp.Status, len(c.Path), c.Path[len(c.Path)-1].Country, short(c.Path[len(c.Path)-1].Account), strings.TrimSpace(string(body)))
	if q, err := c.QueryQuota(8 * time.Second); err == nil {
		fmt.Printf("prepaid bytes remaining at entry: %d\n", q)
	}
}

type streamConn struct{ *relay.Stream }

var errIngress = stdErrors.New("ingress circuit")

func short(a string) string {
	if len(a) > 16 {
		return a[:16] + "…"
	}
	return a
}

// canAfford reports whether the cached chain state shows at least amount raw.
// Shutdown closes the live circuit and stops background watches without
// building anything new. Apps call it on disconnect.
func (m *manager) Shutdown() {
	m.StopFundsWatch()
	m.mu.Lock()
	c := m.cur
	m.cur = nil
	m.mu.Unlock()
	if c != nil {
		c.Close()
	}
	if live := m.live.Load(); live != nil {
		live.Close()
	}
	m.setStage("")
}

// lastStage is the most recent stage of any manager in this process, so a
// screen can show progress while the manager is still being constructed.
var lastStage atomic.Value

// SetLastStage records a stage before any manager exists.
func SetLastStage(s string) { lastStage.Store(s) }

// LastStage is the process-wide current stage (see Stage).
func LastStage() string {
	if v, ok := lastStage.Load().(string); ok {
		return v
	}
	return ""
}

// setStage records what the client is doing, for the apps' progress views.
func (m *manager) setStage(s string) { m.stage.Store(s); lastStage.Store(s) }

// Stage is the current step in plain words: "Reading the relay list",
// "Measuring relays", "Paying the entry relay", "Building circuit: hop 2 of
// 3", "Connected", "Waiting for XNO".
func (m *manager) Stage() string { return LastStage() }

func init() {
	relay.BuildProgress = func(hop, total int) { lastStage.Store(fmt.Sprintf("Building circuit: hop %d of %d", hop, total)) }
}

// NeedsFunds reports whether the wallet cannot pay for a circuit yet: the
// cached balance is below one anchor. Screens use it to ask the user to fund
// the wallet instead of showing a silent "building".
func (m *manager) NeedsFunds() bool {
	if m.opts.freeTag != "" {
		return false
	}
	return !m.canAfford(m.opts.anchor)
}

func (m *manager) canAfford(amount *big.Int) bool {
	_, bal, _, _, ok := chainState(m.key).Get()
	return ok && bal.Cmp(amount) >= 0
}

// pocket receives pending XNO (through the circuit in stealth mode) and so
// refreshes the cached chain state; runs after each circuit build.
func (m *manager) pocket() {
	acct := &nano.Account{Key: m.key, Client: m.nc, State: chainState(m.key)}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if n, err := acct.ReceiveAll(ctx); err == nil && n > 0 {
		log.Printf("received %d pending payment(s)", n)
	} else if err != nil {
		log.Printf("pocket: %v", err)
	}
	if _, ok, err := m.nc.AccountInfo(ctx, m.key.Address); err == nil && ok {
		m.directBootstrap = false // chain state is cached now: no more direct ledger calls
		if _, bal, _, _, cached := chainState(m.key).Get(); cached {
			log.Printf("wallet balance %s XNO", token.FormatXNO(bal))
		}
	}
}

// cliBridges holds --bridge lines until the registry exists.
var cliBridges []string

// builtinBridges are the founding entry relays, compiled in so every client
// has an entry without reading the ledger or any server first.
//
//go:embed bridges_builtin.txt
var builtinBridges string

// censoredFlag is read after the manager exists.
var censoredFlag = new(bool)

// SetCensored switches the censored-network profile on (bridges only, no
// probes, no direct ledger access); used by the app and --censored.
func (m *manager) SetCensored(on bool) {
	m.censored = true // always on: bridges as entries, no probes from the real address
	_ = on
}

// gossipBootstrap asks the relays we can reach (bridges first) for the
// signed records they know. With one bridge line and no ledger, this is how
// a client learns the rest of the network.
func (m *manager) gossipBootstrap() {
	asked, added := 0, 0
	cands := m.bridges()
	if len(cands) == 0 && !m.censored {
		cands = m.reg.All()
	}
	for _, r := range cands {
		if asked >= 2 || r.Flags&token.FlagHome != 0 {
			continue
		}
		recs, err := relay.FetchRelays(r, 15*time.Second)
		if err != nil {
			continue
		}
		asked++
		for _, rec := range recs {
			if m.reg.AddGossip(rec) {
				added++
			}
		}
	}
	for _, r := range m.reg.All() {
		KeepIP(r.Desc.IP.String()) // a relay address that slips into a log reads "relay", never as the user's device
	}
	if added > 0 {
		log.Printf("gossip: learned %d relay record(s) from %d relay(s); %d relays known", added, asked, len(m.reg.All()))
	}
}

// rotateAfter is the circuit lifetime: 8 to 14 minutes, drawn once per
// circuit, so rotations do not tick at a fixed period.
func (m *manager) rotateAfter() time.Duration {
	if m.rotate == 0 {
		m.rotate = 8*time.Minute + time.Duration(mathrand.Intn(360))*time.Second
	}
	return m.rotate
}

// entryCandidates is the relays a client may talk to before it has a
// circuit: bridges when it knows any, otherwise the listed public relays,
// so the network keeps working after every bridge operator is gone.
func (m *manager) entryCandidates() []*relay.RelayInfo {
	if bs := m.bridges(); len(bs) > 0 {
		return bs
	}
	var out []*relay.RelayInfo
	for _, r := range m.reg.All() {
		if r.Flags&token.FlagHome == 0 && r.Flags&token.FlagPublic != 0 {
			out = append(out, r)
		}
	}
	return out
}

// bridges lists the unlisted entries known to this client.
func (m *manager) bridges() []*relay.RelayInfo {
	var out []*relay.RelayInfo
	for _, r := range m.reg.All() {
		if r.Unlisted {
			out = append(out, r)
		}
	}
	return out
}

// measureRTT records the TCP connect time to every public relay (one SYN each,
// no RPC) so the entry choice prefers nearby relays.
func (m *manager) measureRTT() {
	m.mu.Lock()
	if m.rtt == nil {
		m.rtt = map[string]time.Duration{}
	}
	m.mu.Unlock()
	// All probes at once: with many relays a sequential sweep with 5 s
	// timeouts is what made the first connect take a minute.
	var wg sync.WaitGroup
	for _, r := range m.reg.All() {
		KeepIP(r.Desc.IP.String())
		if r.Flags&token.FlagHome != 0 || (len(m.bridges()) > 0 && !r.Unlisted) {
			continue // never touch a listed relay from the real IP when bridges exist
		}
		wg.Add(1)
		go func(r *relay.RelayInfo) {
			defer wg.Done()
			t0 := time.Now()
			c, err := (&net.Dialer{Timeout: 5 * time.Second, Control: relay.DialControl}).Dial("tcp", r.Desc.Addr())
			if err != nil {
				m.mu.Lock()
				m.mark(r.Account, false) // blocked or down from here: route around it
				m.mu.Unlock()
				log.Printf("relay %s (%s) unreachable from this network: %v", r.Country, short(r.Account), err)
				return
			}
			c.Close()
			m.mu.Lock()
			m.rtt[r.Account] = time.Since(t0)
			m.mu.Unlock()
			log.Printf("rtt %s (%s): %s", r.Country, short(r.Account), time.Since(t0).Round(time.Millisecond))
		}(r)
	}
	wg.Wait()
}

// ---------------------------------------------------------------- exported surface

// Manager is the client's circuit manager.
type Manager = manager

// StreamConn adapts a circuit stream to net.Conn.
type StreamConn = streamConn

// ErrIngress marks a failure on the client's own ingress circuit.
var ErrIngress = errIngress

func DataDir() string    { return dataDir() }
func LoadKey() *nano.Key { return loadKey() }

// LoadKeyFrom reads a wallet file (seed + index) at an explicit path.
func LoadKeyFrom(path string) (*nano.Key, error) {
	var wf struct {
		Seed  string `json:"seed"`
		Index uint32 `json:"index"`
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &wf); err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(wf.Seed)
	if err != nil || len(seed) != 32 {
		return nil, fmt.Errorf("bad seed in %s", path)
	}
	return nano.DeriveKey(seed, wf.Index)
}
func ChainState(k *nano.Key) *nano.ChainState { return chainState(k) }
func NewNano() *nano.Client                   { return newNano() }
func Short(a string) string                   { return short(a) }
func RunClient(args []string)                 { runClient(args) }
func RunFetch(args []string)                  { runFetch(args) }

// NewManager builds a manager that talks to the ledger directly.
func NewManager(hops int, exitCC, anchor, rate, regDir, freeTag string) *Manager {
	return newManager(hops, exitCC, anchor, rate, regDir, freeTag)
}

// NewStealthManager builds a manager whose ledger calls go through the circuit.
func NewStealthManager(hops int, exitCC, anchor, rate, freeTag string) *Manager {
	return newStealthManager(hops, exitCC, anchor, rate, freeTag, false)
}

// NewStealthManagerBootstrap is NewStealthManager for a first run: with
// direct set, the ledger is read directly (relay list, wallet state) until
// the state is cached, instead of failing until a circuit exists. Without it
// a fresh install only knew the built-in bridges and could not pick an exit.
func NewStealthManagerBootstrap(hops int, exitCC, anchor, rate, freeTag string, direct bool) *Manager {
	return newStealthManager(hops, exitCC, anchor, rate, freeTag, direct)
}

// AllowPlainHTTP lets port-80 connections through the tunnel. Off by default:
// an unencrypted request is readable by the exit and by everyone after it.
var AllowPlainHTTP = false

// AddBridge registers a bridge line before a manager is built.
func AddBridge(line string) { cliBridges = append(cliBridges, line) }

func (m *manager) Circuit() (*relay.Circuit, error) { return m.circuit() }
func (m *manager) SetAvoid(a map[string]bool)       { m.opts.avoid = a }
func (m *manager) Key() *nano.Key                   { return m.key }
func (m *manager) Registry() *relay.Registry        { return m.reg }
func (m *manager) Nano() *nano.Client               { return m.nc }
func (m *manager) ResolveViaCircuit(q []byte, upstream string) ([]byte, error) {
	return m.resolveViaCircuit(q, upstream)
}

func (streamConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (streamConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (streamConn) SetDeadline(time.Time) error      { return nil }
func (streamConn) SetReadDeadline(time.Time) error  { return nil }
func (streamConn) SetWriteDeadline(time.Time) error { return nil }

// NewStreamConn wraps a circuit stream as a net.Conn.
func NewStreamConn(st *relay.Stream) net.Conn { return streamConn{st} }

// EnsureWallet creates the wallet file if none exists and returns the key.
func EnsureWallet() *nano.Key {
	os.MkdirAll(dataDir(), 0o700)
	wp := filepath.Join(dataDir(), "wallet.json")
	if os.Getenv("SAIL_WALLET") != "" {
		wp = os.Getenv("SAIL_WALLET")
	}
	if _, err := os.Stat(wp); err != nil {
		seed, _ := nano.NewSeed()
		k, _ := nano.DeriveKey(seed, 0)
		data, _ := json.MarshalIndent(map[string]any{"seed": hex.EncodeToString(seed), "index": 0, "address": k.Address}, "", "  ")
		os.WriteFile(wp, data, 0o600)
	}
	return loadKey()
}

// AllowDirectBootstrap lets a stealth manager reach the ledger directly while
// it has no cached chain state (first run).
func (m *manager) AllowDirectBootstrap(on bool) { m.directBootstrap = on }

// Path describes the live circuit, or "" if none.
func (m *manager) Path() string {
	c := m.live.Load()
	if c == nil || c.Closed() {
		return ""
	}
	var parts []string
	for _, p := range c.Path {
		parts = append(parts, p.Country+"("+short(p.Account)+")")
	}
	return strings.Join(parts, " → ")
}

// Balance is the cached wallet balance in XNO (as a string), "" if unknown.
func (m *manager) Balance() string {
	if _, bal, _, _, ok := chainState(m.key).Get(); ok {
		return token.FormatXNO(bal)
	}
	return ""
}

// Relays is how many relays the client currently knows.
// Relays counts relays known to be alive: a bridge we use, a relay that
// answered a probe, or one whose own signed gossip record is recent (relays
// sign a fresh record whenever they answer gossip). Dead registrations stay
// on the ledger forever, so the raw ledger count would overstate the network.
func (m *manager) Relays() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.reg.All() {
		if m.scoreOf(r.Account) < 0.3 {
			continue
		}
		_, probed := m.rtt[r.Account]
		if r.Unlisted || probed || time.Since(m.reg.LastSeen(r.Account)) < 3*time.Hour {
			n++
		}
	}
	return n
}

// RunUDPTest sends a DNS query for example.com as a datagram through the
// circuit to the resolver given (default 1.1.1.1:53) and prints the answer
// size: proves the udp: stream path end to end.
func RunUDPTest(args []string) {
	fs := flag.NewFlagSet("udptest", flag.ExitOnError)
	target := fs.String("to", "1.1.1.1:53", "UDP host:port at the exit side")
	fs.Parse(args)
	m := newManager(3, "", "0.0005", "0.00005", "", "")
	c, err := m.circuit()
	if err != nil {
		log.Fatal(err)
	}
	st, err := c.Open(relay.UDPPrefix+*target, 20*time.Second)
	if err != nil {
		log.Fatal("open udp stream: ", err)
	}
	defer st.Close()
	q := []byte{0x12, 0x34, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1}
	t0 := time.Now()
	if _, err := st.Write(relay.Frame(q)); err != nil {
		log.Fatal(err)
	}
	var d relay.Deframer
	buf := make([]byte, 4096)
	deadline := time.After(15 * time.Second)
	got := make(chan []byte, 1)
	go func() {
		for {
			n, err := st.Read(buf)
			if err != nil {
				return
			}
			d.Push(buf[:n])
			if dg := d.Next(); dg != nil {
				got <- dg
				return
			}
		}
	}()
	select {
	case ans := <-got:
		fmt.Printf("udp through %d hops to %s: %d-byte DNS answer in %s (id %x, answers %d)\n", len(c.Path), *target, len(ans), time.Since(t0).Round(time.Millisecond), ans[:2], int(ans[6])<<8|int(ans[7]))
	case <-deadline:
		log.Fatal("no datagram came back")
	}
}

// RequireLocalNode checks that some Nano RPC answers before a relay or home
// node starts serving. Sailnet's own endpoint is the default and needs no
// setup; a relay operator who wants ledger lookups to stay private can run a
// node and pass --rpc http://127.0.0.1:7076. allowPublic is kept for old
// command lines and changes nothing.
func RequireLocalNode(nc *nano.Client, allowPublic bool) {
	_ = allowPublic
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := nc.Probe(ctx)
	switch {
	case err != nil:
		log.Printf("WARNING: no Nano RPC answered yet (%v); payments will be verified once one does", err)
		return
	case !st.Local && (strings.HasPrefix(st.URL, nano.PrimaryRPC) || strings.HasPrefix(st.URL, nano.FallbackRPC)):
		log.Printf("Nano RPC: Sailnet's endpoint %s (optional: run your own node and pass --rpc http://127.0.0.1:7076)", st.URL)
	case !st.Local:
		log.Printf("Nano RPC: %s", st.URL)
	case !st.Synced():
		log.Fatalf("local Nano node %s (%s) is not synced: %d of %d blocks cemented. Wait for it to catch up before serving", st.URL, st.Version, st.Cemented, st.Count)
	default:
		log.Printf("Nano node %s (%s): synced, %d blocks cemented", st.URL, st.Version, st.Cemented)
	}
}

// ServeSOCKS listens on addr and serves SOCKS5 until the listener is closed.
// The desktop app uses it; the CLI has its own loop in runClient.
func (m *manager) ServeSOCKS(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go m.serveSocks(conn)
		}
	}()
	return ln, nil
}

// ServeDNS answers DNS on addr by forwarding through the circuit to upstream.
func (m *manager) ServeDNS(addr, upstream string) { m.serveDNS(addr, upstream) }

// SetExcludeExit sets the exit countries that must never be used (comma or
// space separated ISO codes). Exclusion is the user's real concern: "not
// through there", rather than "only through here".
func (m *manager) SetExcludeExit(list string) {
	ex := map[string]bool{}
	for _, c := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' || r == ';' || r == '\n' }) {
		if c = strings.ToUpper(strings.TrimSpace(c)); c != "" {
			ex[c] = true
		}
	}
	m.opts.exclude = ex
}

// Countries lists the distinct countries of the relays this client knows,
// sorted, for a settings screen.
func (m *manager) Countries() []string {
	seen := map[string]bool{}
	for _, r := range m.reg.All() {
		if r.Country != "" && r.Country != "XX" {
			seen[strings.ToUpper(r.Country)] = true
		}
	}
	var out []string
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// CachedCountries is Countries without a running manager: it reads the
// registry cache in SAIL_HOME, so a settings screen can offer the list
// before the first connection.
func CachedCountries() []string {
	reg := &relay.Registry{CacheFile: filepath.Join(dataDir(), "registry.json")}
	if reg.LoadCache() == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, r := range reg.All() {
		if r.Country != "" && r.Country != "XX" {
			seen[strings.ToUpper(r.Country)] = true
		}
	}
	var out []string
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// EnsureFundsWatch asks the entry to push confirmations for our own account
// while the wallet cannot pay, so a faucet or a friend paying us turns into
// a circuit within seconds. Everything travels inside the tunnel connection
// to the entry; the client opens nothing else. Idempotent.
func (m *manager) EnsureFundsWatch() {
	m.mu.Lock()
	if m.fundsWatch != nil || !m.NeedsFunds() {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.claimFaucetOnce()
	m.startFundsPoll()
	bs := m.entryCandidates()
	if len(bs) == 0 {
		return
	}
	rel := bs[mathrand.Intn(len(bs))]
	stop, err := relay.WatchOver(rel, m.key.Address, 20*time.Second, func(n relay.Notification) {
		log.Printf("payment confirmed on the ledger; connecting")
		m.StopFundsWatch()
		go m.circuit()
	})
	if err != nil {
		return
	}
	m.mu.Lock()
	if m.fundsWatch != nil { // raced with another caller
		m.mu.Unlock()
		stop()
		return
	}
	m.fundsWatch = stop
	m.mu.Unlock()
	log.Printf("watching the ledger for the first payment to this wallet")
}

// RefreshFunds is the apps' Refresh button: pocket every pending payment,
// re-read the balance, and connect if the wallet can now pay. Returns the
// balance in XNO ("" when unknown).
func (m *manager) RefreshFunds() string {
	m.pocket()
	if !m.NeedsFunds() {
		if live := m.live.Load(); live == nil || live.Closed() {
			m.StopFundsWatch()
			go m.circuit()
		}
	}
	return m.Balance()
}

// startFundsPoll pockets receivables every 30 s while the wallet is empty and
// builds a circuit as soon as it can pay. The entry's confirmation push is
// the fast path; this is the one that cannot be missed: a payment that lands
// while the app waits is received within half a minute, on every platform.
func (m *manager) startFundsPoll() {
	m.mu.Lock()
	if m.polling {
		m.mu.Unlock()
		return
	}
	m.polling = true
	m.mu.Unlock()
	go func() {
		defer func() {
			m.mu.Lock()
			m.polling = false
			m.mu.Unlock()
		}()
		for {
			time.Sleep(30 * time.Second)
			if !m.NeedsFunds() {
				return
			}
			m.pocket()
			if !m.NeedsFunds() {
				log.Printf("funds arrived; connecting")
				m.StopFundsWatch()
				go m.circuit()
				return
			}
		}
	}()
}

// claimFaucetOnce asks the faucet (through the entry relay) for the
// registration amount, at most once per day per wallet; the confirmation
// watch then sees it arrive like any other payment. A refusal is logged
// with the amount the user would have to send.
func (m *manager) claimFaucetOnce() {
	m.mu.Lock()
	if time.Since(m.faucetAt) < 24*time.Hour {
		m.mu.Unlock()
		return
	}
	m.faucetAt = time.Now()
	m.mu.Unlock()
	go func() {
		// The faucet is a convenience, never a dependency: whatever it does,
		// including answering nonsense or nothing at all, the app carries on
		// waiting for funds exactly as it would without one.
		defer func() {
			if r := recover(); r != nil {
				log.Printf("faucet: skipped after an internal error (%v); fund the wallet by hand to connect now", r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if fr, err := ClaimFaucet(ctx, m.nc.HTTP, m.key.Address); err != nil {
			log.Printf("faucet: %v", err)
			m.mu.Lock()
			m.faucetAt = time.Now().Add(-24*time.Hour + 10*time.Minute) // a failed claim is retried in ten minutes, not tomorrow
			m.mu.Unlock()
		} else {
			log.Printf("faucet: %s XNO on its way (%s); connecting when it confirms", fr.Amount, shortHash(fr.Hash))
		}
	}()
}

// StopFundsWatch ends the confirmation watch, if any.
func (m *manager) StopFundsWatch() {
	m.mu.Lock()
	stop := m.fundsWatch
	m.fundsWatch = nil
	m.mu.Unlock()
	if stop != nil {
		stop()
	}
}
