package client

import (
	"context"
	"crypto/rand"
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

func newNano() *nano.Client {
	c := nano.NewClient()
	c.HTTP = &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 15 * time.Second, Control: relay.DialControl}).DialContext, DisableKeepAlives: true}}
	if k := os.Getenv("NANO_RPC_KEY"); k != "" {
		c.APIKey = k
	}
	return c
}

type clientOpts struct {
	hops    int
	exitCC  string
	anchor  *big.Int // raw XNO per prepaid anchor
	rate    uint32   // max price accepted on any hop (RateUnitRaw per MiB); 0 = 3x the median
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
			return (&net.Dialer{Timeout: 15 * time.Second, Control: relay.DialControl}).DialContext(ctx, network, addr)
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
	// Price: the customer is the buyer. Every hop must be at or under the
	// cap (by default three times the median published price, so the cap
	// follows the market rather than a number baked into a binary), and the
	// draw is weighted by (median / price)^2, so a relay that doubles its
	// price gets a quarter of the traffic. That, plus the fact that anyone
	// can register a cheaper relay, is what keeps a cartel from holding.
	median, cap := m.priceCap(all)
	usable := all[:0:0]
	for _, r := range all {
		if m.scoreOf(r.Account) >= 0.3 && !m.opts.avoid[r.Account] && !m.skip[r.Account] && r.MinRate <= cap {
			usable = append(usable, r)
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
	// pick draws one candidate at random in proportion to its weight
	// (argmax would hand every circuit to the single best-scored relay).
	pick := func(pred func(*relay.RelayInfo) bool) *relay.RelayInfo {
		var cands []*relay.RelayInfo
		var ws []float64
		total := 0.0
		for _, r := range usable {
			if used[r.Account] || !pred(r) {
				continue
			}
			w := weight(r)
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
		if m.censored || m.canAfford(m.opts.anchor) || (m.entry != nil && m.entry.Unlisted) {
			e = pick(func(r *relay.RelayInfo) bool { return r.Unlisted })
		}
		if e == nil && m.censored {
			return nil, errors("censored mode: no usable bridge")
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
	used[path[0].Account] = true
	for len(path) < m.opts.hops-1 {
		r := pick(diverse)
		if r == nil {
			r = pick(func(*relay.RelayInfo) bool { return true }) // relax diversity
		}
		if r == nil {
			return nil, errors("not enough relays for the requested hops")
		}
		path = append(path, r)
		used[r.Account] = true
	}
	if m.opts.hops > 1 {
		x := pick(func(r *relay.RelayInfo) bool {
			return r.Flags&token.FlagExit != 0 && diverse(r) && (m.opts.exitCC == "" || r.Country == strings.ToUpper(m.opts.exitCC))
		})
		if x == nil {
			x = pick(func(r *relay.RelayInfo) bool { return r.Flags&token.FlagExit != 0 })
		}
		if x == nil {
			return nil, errors("no exit relay available")
		}
		path = append(path, x)
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
		cap = 3 * median
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
	if _, cap := m.priceCap(m.reg.All()); entry.MinRate > cap {
		return fmt.Errorf("entry %s wants %s XNO/MiB, above your cap", entry.Account, token.FormatXNO(token.RateToRaw(entry.MinRate)))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
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
	if m.cur != nil && !m.cur.Closed() && time.Since(m.cur.Built) < m.rotateAfter() {
		return m.cur, nil
	}
	if m.cur != nil {
		m.cur.Close()
		m.cur = nil
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
		t0 := time.Now()
		c, err := relay.Build(path, m.tag, m.opts.timeout, m.payment, func(pub, tag [32]byte) []byte { return relay.SignCreate(m.key, pub, tag) })
		if err != nil {
			if c != nil && c.Failed >= 0 {
				log.Printf("build failed at hop %d %s: %v", c.Failed, path[c.Failed].Account, err)
				// A hop whose upstream is still prepaying it ("warming up") is not at
				// fault, but retrying the same path is useless: nudge its score down so
				// the next attempt routes around it (scores drift back within minutes).
				m.mark(path[c.Failed].Account, false)
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
		log.Printf("circuit built in %s: %s", time.Since(t0).Round(time.Millisecond), strings.Join(names, " → "))
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

func (m *manager) keepalive(c *relay.Circuit) {
	for !c.Closed() {
		time.Sleep(15*time.Second + time.Duration(mathrand.Intn(12000))*time.Millisecond) // jittered: no fixed rhythm on the wire
		if bad := c.Ping(8 * time.Second); bad >= 0 {
			log.Printf("keepalive: hop %d (%s) failed; rebuilding", bad, c.Path[bad].Account)
			m.mu.Lock()
			m.mark(c.Path[bad].Account, false)
			m.mu.Unlock()
			c.Close()
			return
		}
		if q, err := c.QueryQuota(8 * time.Second); err == nil {
			need := relay.BytesFor(m.opts.anchor, token.RateToRaw(c.Path[0].MinRate)) / 4
			if q < need {
				log.Printf("quota low (%d bytes left): next circuit will pay again", q)
				m.mu.Lock()
				m.tag = [32]byte{}
				m.mu.Unlock()
			}
		}
	}
}

func runClient(args []string) {
	fs := flag.NewFlagSet("client", flag.ExitOnError)
	socks := fs.String("socks", "127.0.0.1:1080", "SOCKS5 listen address")
	hops := fs.Int("hops", 3, "circuit length")
	exitCC := fs.String("exit-cc", "", "preferred exit country")
	anchor := fs.String("anchor", "0.0005", "XNO per prepaid anchor")
	rate := fs.String("rate", "0", "max XNO per MiB you accept on any hop (0 = three times the median published price)")
	regDir := fs.String("registry-dir", "", "test mode: static relay descriptors directory")
	freeTag := fs.String("tag", "", "use an existing payment tag (fragment-B hash of a SAIL transfer to the entry) instead of paying")
	entry := fs.String("entry", "", "pin the entry relay account")
	stealth := fs.Bool("stealth", true, "no direct Nano RPC: ledger calls go through the circuit, payments are signed from the cached chain state and published by the relay")
	dns := fs.String("dns", "127.0.0.1:5300", "answer DNS here by resolving through the circuit at the exit (empty = off)")
	status := fs.String("status", "127.0.0.1:1090", "JSON status endpoint for UIs and the browser extension (empty = off)")
	capture := fs.Bool("capture", false, "whole-device mode: DNS sinkhole on :53 and listeners on :80/:443 that route every flow through the circuit by Host/SNI (needs administrator rights)")
	capAddrs := fs.String("capture-ports", "127.0.0.1:53,127.0.0.1:80,127.0.0.1:443", "with --capture: DNS sinkhole, HTTP and HTTPS listen addresses")
	subvert := fs.Bool("subvert-dns", false, "with --capture: point the operating system's resolver at 127.0.0.1 and restore it on exit")
	dnsUp := fs.String("dns-upstream", "1.1.1.1:53", "resolver the exit asks on your behalf")
	nickFlag := fs.String("nick", "", "nickname shown instead of your wallet address and device IPs in logs and status")
	censoredFlag = fs.Bool("censored", false, "censored-network profile: bridges are the only entries, no startup probes to listed relays, never any direct ledger call")
	bridge := fs.String("bridge", "", "bridge line(s) of unlisted entry relays, comma-separated (also read from SAIL_HOME/bridges.txt); bridges are preferred as entry")
	fs.Parse(args)
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
		m = newStealthManager(*hops, *exitCC, *anchor, *rate, *freeTag)
	} else {
		m = newManager(*hops, *exitCC, *anchor, *rate, *regDir, *freeTag)
	}
	m.opts.entry = *entry
	SetNick(*nickFlag, m.key.Address)
	log.SetOutput(RedactingWriter{W: os.Stderr})
	if bs := m.bridges(); len(bs) > 0 {
		log.Printf("%d unlisted entry relay(s) (bridges) will be preferred as entry", len(bs))
	}
	if *censoredFlag {
		if len(m.bridges()) == 0 {
			log.Fatal("--censored needs at least one bridge line (--bridge or bridges.txt)")
		}
		m.SetCensored(true)
		log.Printf("censored-network profile: bridges only, no probes, no direct ledger access")
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
			if err := subvertDNS(); err != nil {
				log.Fatal("capture: ", err)
			}
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			go func() { <-sig; revertDNS(); os.Exit(0) }()
		}
	}
	log.Printf("SOCKS5 proxy on %s (wallet %s)", *socks, m.key.Address)
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
func newStealthManager(hops int, exitCC, anchor, rate, freeTag string) *manager {
	m := &manager{stealth: true}
	nc := newNano()
	// Keep the connection to the RPC open between calls: every call on its
	// own stream cost BEGIN, an inner TLS handshake, the request and END,
	// a dozen cells of upstream per circuit that the shaping measurement found.
	nc.HTTP = &http.Client{Timeout: 40 * time.Second, Transport: &http.Transport{DialContext: m.dialViaCircuit, TLSHandshakeTimeout: 20 * time.Second, MaxIdleConnsPerHost: 2, IdleConnTimeout: 90 * time.Second}}
	init := newManagerWith(m, nc, hops, exitCC, anchor, rate, "", freeTag)
	log.Printf("stealth: Nano RPC only through the circuit; payments signed offline")
	return init
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

// censoredFlag is read after the manager exists.
var censoredFlag = new(bool)

// SetCensored switches the censored-network profile on (bridges only, no
// probes, no direct ledger access); used by the app and --censored.
func (m *manager) SetCensored(on bool) {
	m.censored = on
	if on {
		m.directBootstrap = false
	}
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
	for _, r := range m.reg.All() {
		KeepIP(r.Desc.IP.String())
		if r.Flags&token.FlagHome != 0 || (len(m.bridges()) > 0 && !r.Unlisted) {
			continue // never touch a listed relay from the real IP when bridges exist
		}
		t0 := time.Now()
		c, err := (&net.Dialer{Timeout: 5 * time.Second, Control: relay.DialControl}).Dial("tcp", r.Desc.Addr())
		if err != nil {
			m.mu.Lock()
			m.mark(r.Account, false) // blocked or down from here: route around it
			m.mu.Unlock()
			log.Printf("relay %s (%s) unreachable from this network: %v", r.Country, short(r.Account), err)
			continue
		}
		c.Close()
		m.mu.Lock()
		m.rtt[r.Account] = time.Since(t0)
		m.mu.Unlock()
		log.Printf("rtt %s (%s): %s", r.Country, short(r.Account), time.Since(t0).Round(time.Millisecond))
	}
}

// ---------------------------------------------------------------- exported surface

// Manager is the client's circuit manager.
type Manager = manager

// StreamConn adapts a circuit stream to net.Conn.
type StreamConn = streamConn

// ErrIngress marks a failure on the client's own ingress circuit.
var ErrIngress = errIngress

func DataDir() string                         { return dataDir() }
func LoadKey() *nano.Key                      { return loadKey() }
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
	return newStealthManager(hops, exitCC, anchor, rate, freeTag)
}

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
		parts = append(parts, p.Country+"("+p.Desc.IP.String()+")")
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
func (m *manager) Relays() int { return len(m.reg.All()) }

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

// RequireLocalNode is the live-network prerequisite for running a relay or
// home node: a Nano node on this machine or private network, answering RPC
// and synced. Without it, every payer account, tag and peer a relay looks up
// is disclosed to a third-party RPC provider, service is granted on that
// provider's word, and the provider's rate limits decide uptime.
// allowPublic is for tests only.
func RequireLocalNode(nc *nano.Client, allowPublic bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := nc.Probe(ctx)
	switch {
	case err != nil && allowPublic:
		log.Printf("WARNING: no Nano RPC answered (%v); continuing because --allow-public-rpc is set", err)
		return
	case err != nil:
		log.Fatalf("Nano RPC: %v. A relay needs its own Nano node: run deploy/nano-node.sh, then set NANO_RPC_URLS=http://127.0.0.1:7076 (or pass --allow-public-rpc for tests only)", err)
	case !st.Local && !allowPublic:
		log.Fatalf("Nano RPC %s is a public endpoint. A live relay must use its own node (it would otherwise disclose every payer, tag and peer it looks up to that provider, and trust it for confirmations). Run deploy/nano-node.sh and set NANO_RPC_URLS=http://127.0.0.1:7076, or pass --allow-public-rpc for tests only", st.URL)
	case !st.Local:
		log.Printf("WARNING: using public Nano RPC %s (--allow-public-rpc): payments and peers are visible to that provider; do not run this way live", st.URL)
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
