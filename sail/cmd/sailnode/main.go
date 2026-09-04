// sailnode — Sailnet relay and client.
//
//	sailnode relay  --listen :443 --ip <public-ipv4> --cc NL --asn 14061 --rate 0.5 [--exit] [--register]
//	sailnode client --socks :1080 --hops 3 [--exit-cc US] [--anchor 10] [--rate 0.5]
//	sailnode relays                      list relays from the ledger
//	sailnode fetch <url> [--hops 3]      one-shot HTTP GET through a fresh circuit (test)
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	stdErrors "errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/dhyabi2/sail/automap"
	"github.com/dhyabi2/sail/client"
	"github.com/dhyabi2/sail/nano"
	"github.com/dhyabi2/sail/relay"
	"github.com/dhyabi2/sail/shape"
	"github.com/dhyabi2/sail/token"
	"github.com/dhyabi2/sail/wire"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: sailnode relay|client|relays|fetch ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "relay":
		runRelay(os.Args[2:])
	case "client":
		client.RunClient(os.Args[2:])
	case "relays":
		reg := &relay.Registry{Client: client.NewNano(), Treasury: client.Treasury}
		if err := reg.Refresh(context.Background()); err != nil {
			log.Fatal(err)
		}
		for _, r := range reg.All() {
			fmt.Printf("%s  %s  cc=%s asn=%d rate=%s XNO/MiB flags=%d\n", r.Account, r.Desc.Addr(), r.Country, r.ASN, token.FormatXNO(token.RateToRaw(r.MinRate)), r.Flags)
		}
	case "fetch":
		client.RunFetch(os.Args[2:])
	case "udptest":
		client.RunUDPTest(os.Args[2:])
	case "earn":
		runEarn(os.Args[2:])
	default:
		fmt.Println("usage: sailnode relay|client|relays|fetch ...")
		os.Exit(2)
	}
}

const decoyHTML = `<!doctype html><html><head><meta charset="utf-8"><title>Harbour Weather</title>
<style>body{font-family:Georgia,serif;max-width:640px;margin:4rem auto;color:#223}</style></head>
<body><h1>Harbour Weather</h1><p>Daily tide and wind notes for small-boat sailors. Updated most mornings.</p>
<p>Today: light airs from the south-west, backing westerly by afternoon. Sea state slight.</p>
<p><small>&copy; Harbour Weather</small></p></body></html>`

// ---------------------------------------------------------------- relay

func runRelay(args []string) {
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	listen := fs.String("listen", ":443", "listen address(es), comma-separated: the first is published on the ledger; the others are extra ports for bridge lines, so blocking one port is not enough")
	ip := fs.String("ip", "", "public IPv4 to publish")
	host := fs.String("host", "", "TLS name the relay presents (a real domain pointing here is best; default: a plausible generated name)")
	cc := fs.String("cc", "XX", "country code")
	asn := fs.Uint("asn", 0, "autonomous system number")
	rate := fs.String("rate", "0.000005", "starting price, XNO per MiB (about $0.002 per GB)")
	reprice := fs.Bool("reprice", true, "adjust the price to demand every --reprice-days: down 10% when usage falls, up 3% when it grows, never above four times the starting price")
	repriceDays := fs.Int("reprice-days", 10, "length of a repricing window in days")
	exit := fs.Bool("exit", true, "offer exit service")
	register := fs.Bool("register", false, "publish REGISTER + DESCRIPTOR on the ledger")
	allowPublicRPC := fs.Bool("allow-public-rpc", false, "no effect (kept for old command lines): relays use Sailnet's endpoint unless --rpc names your own node")
	faucetWallet := fs.String("faucet-wallet", "", "serve a faucet at /faucet paying the registration amount from this wallet file (empty = no faucet)")
	faucetAmount := fs.String("faucet-amount", "0.0005", "XNO per faucet claim (the registration amount: one anchor)")
	faucetPerIP := fs.Int("faucet-per-ip", 10, "faucet claims per public IP per day")
	rpcURL := fs.String("rpc", "", "Nano RPC endpoint(s), comma-separated, tried in order (default: Sailnet's endpoint, then public nodes; your own node: http://127.0.0.1:7076)")
	rpcKey := fs.String("rpc-key", "", "API key for a configured rpc.nano.to endpoint")
	payout := fs.String("payout", "", "forward everything this node earns to this nano_ address every hour, keeping only --payout-keep on the node")
	payoutKeep := fs.String("payout-keep", "0.02", "XNO kept on the node as operating float for pools and the levy")
	levy := fs.Bool("levy", false, "EXPERIMENTAL: pay the daily 10 % redistribution levy (off by default)")
	unlisted := fs.Bool("unlisted", false, "bridge mode: never publish on the ledger; print a bridge line to hand to clients out of band (censors reading the ledger cannot find this relay)")
	certFile := fs.String("cert", "", "PEM certificate chain to present (e.g. Let's Encrypt for --host); default: a generated self-signed cert")
	acme := fs.Bool("acme", false, "obtain and renew a real certificate for --host from Let's Encrypt automatically (the domain must point at this host; port 443 required)")
	keyFile := fs.String("key", "", "PEM private key for --cert")
	pool := fs.String("pool", "0.005", "XNO per downstream pool top-up, refilled at a quarter left (0 = static pool tags, test mode)")
	regDir := fs.String("registry-dir", "", "test mode: read/write relay descriptors as JSON in this directory instead of the ledger")
	name := fs.String("name", "", "test mode: descriptor file name (default: account)")
	freeTag := fs.String("free-tag", "", "test mode: preauthorized 64-hex payment tag")
	freeBytes := fs.Int64("free-bytes", 200<<20, "test mode: bytes granted to --free-tag and to every peer's pool tag")
	fs.Parse(args)
	if *rpcURL != "" || *rpcKey != "" {
		nano.ConfigureRPC(*rpcURL, *rpcKey)
	}
	// SIGHUP reloads the shaping parameters (SAIL_SHAPE) without a restart,
	// so a measurement run can change them while circuits stay up.
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGHUP)
		for range c {
			if err := shape.LoadEnv(); err != nil {
				log.Printf("shape reload: %v", err)
			} else {
				log.Printf("shape parameters reloaded")
			}
		}
	}()
	if *ip == "" { // default: this host's first public IPv4, else what the internet sees
		if host, err := os.Hostname(); err == nil {
			if ips, err := net.LookupIP(host); err == nil {
				for _, a := range ips {
					if a4 := a.To4(); a4 != nil && !a4.IsLoopback() && !a4.IsPrivate() && !a4.IsLinkLocalUnicast() {
						*ip = a4.String()
						break
					}
				}
			}
		}
		if *ip == "" { // behind NAT or in a container: ask an echo service once
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if pub, err := automap.PublicIPViaProbe(ctx); err == nil && pub != nil {
				*ip = pub.String()
				log.Printf("public IP %s (detected; pass --ip to override)", *ip)
			}
			cancel()
		}
	}

	key := client.LoadKey()
	nc := client.NewNano()
	nano.AllowCPUWork = false // a relay never burns its core on proof-of-work; sends retry when the work service is back
	if *regDir == "" {
		client.RequireLocalNode(nc, *allowPublicRPC) // live prerequisite: your own Nano node
	}
	rateU, err := token.RateFromXNO(*rate)
	if err != nil || rateU == 0 {
		log.Fatalf("bad --rate %q (min 0.0000000001 XNO/MiB)", *rate)
	}
	rateRaw := token.RateToRaw(rateU)
	poolRaw, _ := token.ParseXNO(*pool)
	if poolRaw == nil || poolRaw.Sign() == 0 {
		poolRaw = nil // static pool tags (no on-chain top-ups)
	}
	os.MkdirAll(client.DataDir(), 0o700)

	if *host == "" {
		*host = decoyHost(key.Public)
	}
	// TLS cert is stable across restarts (its fingerprint is on the ledger).
	// With --cert/--key a real, CA-issued certificate is presented instead, so
	// a firewall that blocks "untrusted server certificates" lets it through.
	var cert tlsCert
	var fp [6]byte
	var getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	if *acme {
		if *host == "" || net.ParseIP(*host) != nil {
			log.Fatal("--acme needs --host <domain> that resolves to this relay")
		}
		mgr := &autocert.Manager{Prompt: autocert.AcceptTOS, HostPolicy: autocert.HostWhitelist(*host), Cache: autocert.DirCache(filepath.Join(client.DataDir(), "acme"))}
		getCert = mgr.GetCertificate
		// No pin on the ledger: clients verify the real certificate through
		// system roots and the ack binds the leaf that is being served.
		log.Printf("ACME: real certificate for %s (cache %s)", *host, filepath.Join(client.DataDir(), "acme"))
	} else if *certFile != "" {
		c, e := tlsLoadX509KeyPair(*certFile, *keyFile)
		if e != nil {
			log.Fatal("--cert/--key: ", e)
		}
		cert, fp = c, relay.CertFP6(c.Certificate[0])
	} else {
		cert, fp, err = loadOrMakeCert(filepath.Join(client.DataDir(), "relay-cert.json"), *host)
		if err != nil {
			log.Fatal(err)
		}
	}
	listens := strings.Split(*listen, ",")
	_, portStr, _ := net.SplitHostPort(listens[0])
	port, _ := strconv.Atoi(portStr)
	desc := relay.Descriptor{IP: net.ParseIP(*ip), Port: uint16(port), CertFP: fp}
	if *acme {
		desc.CertFP = [6]byte{} // zero pin = verify through system roots
	}

	flags := uint16(token.FlagPublic)
	if *exit {
		flags |= token.FlagExit | token.FlagFlow
	}
	var bridgeSecret [16]byte
	if *unlisted {
		*register = false
		// A bridge's tunnel token needs a secret from its bridge line, so a
		// censor who learns the address still cannot confirm it by probing.
		sp := filepath.Join(client.DataDir(), "bridge-secret")
		if b, err := os.ReadFile(sp); err == nil && len(b) >= 32 {
			hex.Decode(bridgeSecret[:], b[:32])
		} else {
			rand.Read(bridgeSecret[:])
			os.WriteFile(sp, []byte(hex.EncodeToString(bridgeSecret[:])), 0o600)
		}
		bl := (&relay.RelayInfo{Account: key.Address, Desc: desc, Host: *host, Secret: bridgeSecret}).BridgeLine()
		os.WriteFile(filepath.Join(client.DataDir(), "bridge.txt"), []byte(bl+"\n"), 0o600)
		log.Printf("bridge mode: not on the ledger. Give clients this line (also in %s):\n%s", filepath.Join(client.DataDir(), "bridge.txt"), bl)
	}
	// Registration is idempotent and never fatal: publish REGISTER/DESCRIPTOR
	// only if the ledger does not already show this exact record, and retry in
	// the background (10-minute backoff) if the wallet is not funded yet.
	if *register {
		// A wallet that was never opened cannot publish REGISTER: ask the
		// faucet for the registration amount first. Failure is not fatal;
		// the message names the amount to send by hand.
		if _, ok, err := nc.AccountInfo(context.Background(), key.Address); err == nil && !ok {
			log.Printf("wallet %s is empty: asking the faucet for the registration amount", client.Short(key.Address))
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			if err := client.FundFromFaucet(ctx, &http.Client{Timeout: 150 * time.Second}, nc, key); err != nil {
				log.Printf("faucet: %v", err)
			} else {
				log.Printf("faucet: wallet funded and opened")
			}
			cancel()
		}
		go keepRegistered(nc, key, strings.ToUpper(*cc), uint32(*asn), rateU, flags, desc)
	}

	q, err := relay.NewQuota(filepath.Join(client.DataDir(), "quota.wal"), rateRaw)
	if err != nil {
		log.Fatal(err)
	}
	// The relay list survives restarts: loaded from the cache at once, so a
	// relay that just came up already knows whom to EXTEND to, and refreshed
	// from the ledger in the background. Reads through Sailnet's endpoint need
	// not crawl at public-node pace.
	reg := &relay.Registry{Client: nc, Treasury: client.Treasury, CacheFile: filepath.Join(client.DataDir(), "registry.json")}
	if n := reg.LoadCache(); n > 0 {
		log.Printf("registry: %d relays from cache", n)
	}
	nc.Budget = nano.NewBudget(4, 30)
	self := &relay.RelayInfo{Account: key.Address, Pub: key.Public, Country: strings.ToUpper(*cc), ASN: uint32(*asn), MinRate: rateU, Flags: flags, Desc: desc, Host: *host}
	if *regDir != "" {
		os.MkdirAll(*regDir, 0o755)
		fn := *name
		if fn == "" {
			fn = key.Address
		}
		if err := relay.WriteStatic(filepath.Join(*regDir, fn+".json"), self); err != nil {
			log.Fatal(err)
		}
		if *freeTag != "" {
			q.Credit(*freeTag, *freeBytes, "")
		}
		go func() {
			for {
				if err := reg.LoadDir(*regDir); err != nil {
					log.Println("registry-dir:", err)
				}
				for _, peer := range reg.All() { // every peer may extend through us with its static pool tag
					if peer.Account != key.Address {
						q.Credit(relay.PoolTag(peer.Account, key.Address), *freeBytes, "")
					}
				}
				time.Sleep(3 * time.Second)
			}
		}()
	} else {
		go func() { // ledger registry: ~5 RPC calls per refresh, so every 10 minutes
			for {
				if err := reg.Refresh(context.Background()); err != nil {
					log.Println("registry:", err)
				}
				time.Sleep(10 * time.Minute)
			}
		}()
	}
	go func() { // pocket incoming payments (receive blocks) every 3 minutes
		if *regDir != "" {
			return
		}
		acct := &nano.Account{Key: key, Client: nc}
		for {
			acct.ReceiveAll(context.Background())
			time.Sleep(3 * time.Minute)
		}
	}()

	s := &relay.Server{Key: key, Nano: nc, Quota: q, TLS: cert, Registry: reg, Exit: *exit, PoolRaw: poolRaw, Decoy: decoyHTML, PoolsFile: filepath.Join(client.DataDir(), "pools.json"), AllowPrivate: *regDir != "", BridgeSecret: bridgeSecret, GetCertificate: getCert, Host: *host}
	if *unlisted {
		s.BootstrapBytes = 2 << 20 // a first-run client in a censored network gets 2 MiB to reach the ledger through us
	}
	if !*unlisted {
		s.Self = self // bridges are never gossiped: their whole point is not being listed
	}
	s.LoadPools()
	if *regDir == "" {
		go func() { time.Sleep(3 * time.Minute); s.RunLevy(*levy) }()
		if *payout != "" {
			keep, err := token.ParseXNO(*payoutKeep)
			if err != nil {
				log.Fatal("--payout-keep: ", err)
			}
			if _, err := nano.AddressToPubkey(*payout); err != nil || *payout == key.Address {
				log.Fatal("--payout: not a valid address, or this node's own wallet")
			}
			log.Printf("payout: earnings above %s XNO go to %s every hour", *payoutKeep, client.Short(*payout))
			go func() { time.Sleep(5 * time.Minute); s.RunPayout(*payout, keep, time.Hour) }()
		}
	}
	go func() { // gossip: learn peers from peers, so the network knows itself without the ledger
		for i := 0; i < 36 && len(reg.All()) < 2; i++ { // wait for the ledger replay (or gossip) to name a peer
			time.Sleep(5 * time.Second)
		}
		for {
			s.Gossip(4)
			time.Sleep(10 * time.Minute)
		}
	}()
	nano.DefaultBudget.Persist(filepath.Join(client.DataDir(), "rpc-budget.json"))
	go func() { // pool warm-up: on-chain top-ups for every peer, sequential, off the request path
		time.Sleep(10 * time.Second)
		for {
			s.WarmPools()
			time.Sleep(10 * time.Minute)
		}
	}()
	s.Metrics.Started = time.Now()
	go func() { // hourly report: what a month costs is visible from day one
		for {
			time.Sleep(time.Hour)
			b := nano.DefaultBudget.Snapshot()
			m := map[string]any{"uptime_h": time.Since(s.Metrics.Started).Hours(), "circuits": s.Metrics.Circuits.Load(), "payments": s.Metrics.Payments.Load(), "payments_bad": s.Metrics.PaymentsBad.Load(), "spam_rejected": s.Metrics.RejectedSpam.Load(), "streams": s.Metrics.Streams.Load(), "bytes_relayed": s.Metrics.BytesRelayed.Load(), "bytes_exit": s.Metrics.BytesExit.Load(), "rpc_calls_total": b.Total, "rpc_calls_per_day": b.PerDay, "rpc_by_action": b.Counts}
			data, _ := json.MarshalIndent(m, "", "  ")
			os.WriteFile(filepath.Join(client.DataDir(), "metrics.json"), data, 0o644)
			log.Printf("metrics %s", data)
		}
	}()
	if *faucetWallet != "" {
		fk, err := client.LoadKeyFrom(*faucetWallet)
		if err != nil {
			log.Fatalf("--faucet-wallet: %v", err)
		}
		amt, err := token.ParseXNO(*faucetAmount)
		if err != nil || amt.Sign() <= 0 {
			log.Fatalf("bad --faucet-amount %q", *faucetAmount)
		}
		s.Faucet = &relay.Faucet{Key: fk, Nano: nc, State: client.ChainState(fk), Amount: amt, PerIP: *faucetPerIP, Secret: os.Getenv("FAUCET_SECRET"), File: filepath.Join(client.DataDir(), "faucet-state.json")}
		log.Printf("faucet: %s XNO per claim, %d per IP per day, from %s", *faucetAmount, *faucetPerIP, client.Short(fk.Address))
	}
	log.Printf("sailnode relay %s on %s (ip %s, cc %s, asn %d, rate %s XNO/MiB, exit=%v, certfp %x)", key.Address, *listen, *ip, *cc, *asn, *rate, *exit, fp)
	go func() { // a restart is announced: clients move to another entry before this process exits
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGTERM, syscall.SIGINT)
		<-c
		log.Printf("shutting down: telling connected clients to move to another entry")
		s.Drain(20 * time.Second)
		q.Flush()
		os.Exit(0)
	}()
	if *reprice && *register {
		// Demand-driven price: every window the relay looks at the bytes
		// it carried and moves its price down 10% when usage fell, up 3%
		// when it grew. A price change is a new REGISTER. Nothing in
		// here may take the relay down: every step is recovered and
		// logged.
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("repricing stopped after an internal error: %v", r)
				}
			}()
			pr := &relay.Pricing{File: filepath.Join(client.DataDir(), "pricing.json"), Days: *repriceDays, Min: 1, Max: rateU * 4}
			cur := pr.Load(rateU, s.Metrics.BytesRelayed.Load())
			apply := func(r uint32) {
				q.SetMinRate(token.RateToRaw(r))
				self.MinRate = r
				log.Printf("price is now %s XNO/MiB", token.FormatXNO(token.RateToRaw(r)))
				go keepRegistered(nc, key, strings.ToUpper(*cc), uint32(*asn), r, flags, desc)
			}
			if cur != rateU {
				apply(cur) // a saved price from an earlier window
			}
			for {
				time.Sleep(time.Hour)
				if r, changed := pr.Tick(s.Metrics.BytesRelayed.Load()); changed {
					apply(r)
				}
			}
		}()
	}
	for _, extra := range listens[1:] {
		go func(a string) { log.Fatal(s.ListenAndServe(a)) }(a(extra))
		_, p, _ := net.SplitHostPort(extra)
		ep, _ := strconv.Atoi(p)
		log.Printf("extra listener %s; bridge line: %s", extra, (&relay.RelayInfo{Account: key.Address, Desc: relay.Descriptor{IP: desc.IP, Port: uint16(ep), CertFP: fp}, Host: *host}).BridgeLine())
	}
	log.Fatal(s.ListenAndServe(listens[0]))
}

func a(s string) string { return strings.TrimSpace(s) }

// registered reports whether the ledger already carries this relay's current record.
// keepRegistered publishes REGISTER and DESCRIPTOR until the ledger shows
// exactly this record; it retries on a 10-minute backoff and never panics.
func keepRegistered(nc *nano.Client, key *nano.Key, cc string, asn, rate uint32, flags uint16, desc relay.Descriptor) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("registration stopped after an internal error: %v", r)
		}
	}()
	for {
		if registered(nc, key, cc, asn, rate, flags, desc) {
			return
		}
		acct := &nano.Account{Key: key, Client: nc}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		rep, _ := token.Encode(token.Op{Code: token.OpRegister, Aux: token.RegisterAux(cc, asn, rate, flags)})
		h, err := acct.Send(ctx, client.Treasury, big.NewInt(1), &rep)
		if err == nil {
			log.Println("REGISTER published", h)
			op := token.Op{Code: token.OpDescriptor}
			op.Aux = desc.Encode()
			rep, _ = token.Encode(op)
			if h, err = acct.Send(ctx, client.Treasury, big.NewInt(1), &rep); err == nil {
				log.Println("DESCRIPTOR published", h)
			}
		}
		cancel()
		if err == nil {
			return
		}
		log.Printf("registration not published yet (%v); retrying in 10 min", err)
		time.Sleep(10 * time.Minute)
	}
}

func registered(nc *nano.Client, key *nano.Key, cc string, asn, rate uint32, flags uint16, desc relay.Descriptor) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	st, err := token.NewIndexer(nc, client.Treasury).Run(ctx)
	if err != nil {
		return false
	}
	r := st.Relays[key.Address]
	if r == nil {
		return false
	}
	return r.Country == cc && r.ASN == asn && r.MinRate == rate && r.Flags == flags && r.Descriptor == desc.Encode()
}

// decoyHost derives a stable, ordinary-looking hostname from the relay key.
func decoyHost(pub [32]byte) string {
	words := []string{"harbour", "tide", "north", "blue", "cedar", "maple", "river", "stone", "quiet", "lantern", "summit", "meadow"}
	tlds := []string{"com", "net", "org", "io"}
	return fmt.Sprintf("www.%s%s.%s", words[int(pub[0])%len(words)], words[int(pub[1])%len(words)], tlds[int(pub[2])%len(tlds)])
}

func loadOrMakeCert(path, host string) (cert tlsCert, fp [6]byte, err error) {
	var saved struct{ Cert, Key string }
	if data, e := os.ReadFile(path); e == nil && json.Unmarshal(data, &saved) == nil {
		c, e := tlsX509KeyPair([]byte(saved.Cert), []byte(saved.Key))
		if e == nil {
			return c, relay.CertFP6(c.Certificate[0]), nil
		}
	}
	c, fp, err := relay.SelfSignedCert(host)
	if err != nil {
		return c, fp, err
	}
	pemCert, pemKey := pemOf(c)
	data, _ := json.Marshal(struct{ Cert, Key string }{string(pemCert), string(pemKey)})
	os.WriteFile(path, data, 0o600)
	return c, fp, nil
}

// ---------------------------------------------------------------- client

// ---------------------------------------------------------------- earn (one command, no server)

// runEarn turns any PC into a relay: wallet, router port mapping, public IP,
// registration, then the relay loop. If the router refuses or the connection
// is behind CGNAT it says so plainly and keeps retrying (reverse-tunnel home
// mode is the planned fallback).
func runEarn(args []string) {
	fs := flag.NewFlagSet("earn", flag.ExitOnError)
	port := fs.Uint("port", 8443, "TCP port to expose (443 needs admin on most systems)")
	cc := fs.String("cc", "", "country code (auto from the exit-probe if empty)")
	asn := fs.Uint("asn", 0, "ASN (0 = unknown)")
	rate := fs.String("rate", "0.00002", "price, XNO per MiB")
	exit := fs.Bool("exit", false, "also serve as exit (your IP is what websites see)")
	home := fs.Bool("home", false, "skip port mapping: attach to a public relay (harbour) through an outbound tunnel")
	harbourFlag := fs.String("harbour", "", "harbour relay account (default: best public relay on the ledger)")
	pool := fs.String("pool", "0.001", "XNO prepaid to the harbour for relaying your traffic")
	ingress := fs.Int("ingress", 2, "home mode: reach the harbour through a circuit of this many relays so it never sees your address (0 = connect directly)")
	anchor := fs.String("anchor", "0.0005", "home mode: XNO prepaid to the entry of the ingress circuit")
	allowPublicRPC := fs.Bool("allow-public-rpc", false, "TESTS ONLY: run without a local Nano node")
	rpcURL := fs.String("rpc", "", "Nano RPC endpoint(s), comma-separated, tried in order (default: Sailnet's endpoint, then public nodes)")
	rpcKey := fs.String("rpc-key", "", "API key for a configured rpc.nano.to endpoint")
	payout := fs.String("payout", "", "forward everything this node earns to this nano_ address every hour, keeping only --payout-keep on the node")
	payoutKeep := fs.String("payout-keep", "0.02", "XNO kept on the node as operating float for pools, the harbour and the levy")
	fs.Parse(args)
	if *rpcURL != "" || *rpcKey != "" {
		nano.ConfigureRPC(*rpcURL, *rpcKey)
	}
	os.MkdirAll(client.DataDir(), 0o700)
	wp := filepath.Join(client.DataDir(), "wallet.json")
	if os.Getenv("SAIL_WALLET") != "" {
		wp = os.Getenv("SAIL_WALLET")
	}
	if _, err := os.Stat(wp); err != nil {
		seed, _ := nano.NewSeed()
		k, _ := nano.DeriveKey(seed, 0)
		data, _ := json.MarshalIndent(map[string]any{"seed": hex.EncodeToString(seed), "index": 0, "address": k.Address}, "", "  ")
		os.WriteFile(wp, data, 0o600)
		fmt.Println("created wallet", wp)
	}
	key := client.LoadKey()
	nc := client.NewNano()
	client.RequireLocalNode(nc, *allowPublicRPC) // live prerequisite: your own Nano node
	fmt.Println("relay wallet:", key.Address)
	fmt.Println("fund it with ~0.005 XNO (registration + prepaying peers); waiting...")
	acct := &nano.Account{Key: key, Client: nc}
	for {
		acct.ReceiveAll(context.Background())
		info, ok, err := nc.AccountInfo(context.Background(), key.Address)
		if err == nil && ok && info.Balance != "0" {
			bal, _ := new(big.Int).SetString(info.Balance, 10)
			fmt.Println("funded:", token.FormatXNO(bal), "XNO")
			break
		}
		time.Sleep(60 * time.Second)
	}
	ctx := context.Background()
	if *home {
		runHome(key, nc, *rate, *exit, *harbourFlag, *pool, *ingress, *anchor, *payout, *payoutKeep)
		return
	}
	fmt.Printf("asking the router to forward TCP %d (NAT-PMP/PCP, then UPnP)...\n", *port)
	var pub net.IP
	for attempt := 0; ; attempt++ {
		m, err := automap.Map(ctx, uint16(*port), 2*time.Hour)
		probe, perr := automap.PublicIPViaProbe(ctx)
		if err == nil && !m.CGNAT && (perr != nil || probe.Equal(m.PublicIP)) {
			pub = m.PublicIP
			fmt.Printf("mapped via %s: %s:%d (lease %s)\n", m.Protocol, m.PublicIP, m.ExternalPort, m.Lease)
			go automap.Renew(ctx, uint16(*port), 2*time.Hour, m, func(n *automap.Mapping) {
				log.Printf("public IP changed to %s: re-registering", n.PublicIP)
				os.Exit(3)
			})
			break
		}
		switch {
		case err != nil:
			fmt.Println("router:", err)
		case m.CGNAT:
			fmt.Printf("your router's public side is %s, a shared/CGNAT address: the internet cannot reach you\n", m.PublicIP)
		default:
			fmt.Printf("router says %s but the internet sees %s (double NAT)\n", m.PublicIP, probe)
		}
		fmt.Println("cannot be reached directly: switching to home mode (outbound tunnel to a public relay)")
		runHome(key, nc, *rate, *exit, *harbourFlag, *pool, *ingress, *anchor, *payout, *payoutKeep)
		return
	}
	if *cc == "" {
		*cc = "XX"
	}
	log.Printf("starting relay on %s:%d", pub, *port)
	relayArgs := []string{"--listen", fmt.Sprintf(":%d", *port), "--ip", pub.String(), "--cc", *cc, "--asn", fmt.Sprint(*asn), "--rate", *rate, "--register", fmt.Sprintf("--exit=%v", *exit)}
	runRelay(relayArgs)
}

// runHome is the earn path for a PC behind CGNAT: register on the ledger as a
// home node whose descriptor points at a public relay (the harbour), prepay
// that relay, attach with a signed HOME_HELLO and serve circuits over the
// outbound tunnel. Reconnects forever.
func runHome(key *nano.Key, nc *nano.Client, rate string, exit bool, harbourAcct, poolXNO string, ingress int, anchorXNO, payout, payoutKeep string) {
	rateU, err := token.RateFromXNO(rate)
	if err != nil || rateU == 0 {
		log.Fatalf("bad rate %q", rate)
	}
	poolRaw, _ := token.ParseXNO(poolXNO)
	reg := &relay.Registry{Client: nc, Treasury: client.Treasury}
	if err := reg.Refresh(context.Background()); err != nil {
		log.Fatal("registry:", err)
	}
	// Harbour choice. If the chosen harbour stops answering (down, or its
	// country blocked between us), the node moves to another public relay
	// and republishes its descriptor.
	pickHarbour := func(avoid string) *relay.RelayInfo {
		for _, r := range reg.All() {
			if r.Flags&token.FlagHome != 0 || r.Account == key.Address || r.Account == avoid {
				continue
			}
			if harbourAcct == "" || r.Account == harbourAcct {
				return r
			}
		}
		return nil
	}
	harbour := pickHarbour("")
	if harbour == nil {
		log.Fatal("no public relay available to act as harbour")
	}
	var harbourPtr atomic.Pointer[relay.RelayInfo]
	harbourPtr.Store(harbour)
	flags := uint16(token.FlagHome)
	if exit {
		flags |= token.FlagExit | token.FlagFlow
	}
	q, err := relay.NewQuota(filepath.Join(client.DataDir(), "quota.wal"), token.RateToRaw(rateU))
	if err != nil {
		log.Fatal(err)
	}
	cert, _, _ := loadOrMakeCert(filepath.Join(client.DataDir(), "relay-cert.json"), decoyHost(key.Public))
	s := &relay.Server{Key: key, Nano: nc, Quota: q, TLS: cert, Registry: reg, Exit: exit, PoolRaw: poolRaw, Decoy: decoyHTML, PoolsFile: filepath.Join(client.DataDir(), "pools.json")}
	s.LoadPools()
	go func() { time.Sleep(3 * time.Minute); s.RunLevy(false) }()
	if payout != "" {
		keep, err := token.ParseXNO(payoutKeep)
		if err != nil {
			log.Fatal("--payout-keep: ", err)
		}
		if _, err := nano.AddressToPubkey(payout); err != nil || payout == key.Address {
			log.Fatal("--payout: not a valid address, or this node's own wallet")
		}
		log.Printf("payout: earnings above %s XNO go to %s every hour", payoutKeep, client.Short(payout))
		go func() { time.Sleep(5 * time.Minute); s.RunPayout(payout, keep, time.Hour) }()
	}
	nano.DefaultBudget.Persist(filepath.Join(client.DataDir(), "rpc-budget.json"))
	// Registration follows the current harbour: a home node publishes the
	// harbour's country, no ASN, and the harbour's descriptor — never where
	// its operator is. Checked against the ledger every 10 minutes from the
	// registry we refresh anyway (no extra RPC).
	go func() {
		first := true
		for {
			if !first {
				time.Sleep(10 * time.Minute)
				reg.Refresh(context.Background())
			}
			first = false
			h := harbourPtr.Load()
			cc, asn, desc := strings.ToUpper(h.Country), uint32(0), h.Desc
			if me := reg.Get(key.Address); me != nil && me.Country == cc && me.ASN == asn && me.MinRate == rateU && me.Flags == flags && me.Desc.Encode() == desc.Encode() {
				continue
			}
			acct := &nano.Account{Key: key, Client: nc, State: client.ChainState(key)}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			rep, _ := token.Encode(token.Op{Code: token.OpRegister, Aux: token.RegisterAux(cc, asn, rateU, flags)})
			hh, err := acct.Send(ctx, client.Treasury, big.NewInt(1), &rep)
			if err == nil {
				log.Println("REGISTER (home) published", hh)
				op := token.Op{Code: token.OpDescriptor}
				op.Aux = desc.Encode()
				rep, _ = token.Encode(op)
				if hh, err = acct.Send(ctx, client.Treasury, big.NewInt(1), &rep); err == nil {
					log.Println("DESCRIPTOR (via harbour) published", hh)
				}
			}
			cancel()
			if err != nil {
				log.Printf("registration not published yet (%v); retrying in 10 min", err)
			}
		}
	}()
	go func() {
		time.Sleep(10 * time.Second)
		for {
			s.WarmPools()
			time.Sleep(10 * time.Minute)
		}
	}()
	// prepay the harbour once; the pool tag is what HOME_HELLO carries. The tag
	// is kept on disk so a restart reuses the pool instead of paying again.
	acct := &nano.Account{Key: key, Client: nc, State: client.ChainState(key)}
	tagPath := filepath.Join(client.DataDir(), "harbour-pool.json")
	var poolTag string
	if data, err := os.ReadFile(tagPath); err == nil {
		var saved struct{ Harbour, Tag string }
		if json.Unmarshal(data, &saved) == nil && saved.Harbour == harbour.Account {
			poolTag = saved.Tag
			log.Printf("reusing harbour pool %s… (delete %s to prepay again)", poolTag[:8], tagPath)
		}
	}
	prepay := func() {
		harbour := harbourPtr.Load()
		for poolTag == "" {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			h, err := acct.Send(ctx, harbour.Account, poolRaw, nil)
			cancel()
			if err != nil {
				log.Printf("harbour prepayment failed (%v); retrying in 2 min", err)
				time.Sleep(2 * time.Minute)
				continue
			}
			poolTag = strings.ToUpper(h)
			data, _ := json.Marshal(map[string]string{"Harbour": harbour.Account, "Tag": poolTag})
			os.WriteFile(tagPath, data, 0o600)
			log.Printf("prepaid harbour %s with %s XNO (tag %s)", client.Short(harbour.Account), poolXNO, h[:8])
			time.Sleep(3 * time.Second)
		}
	}
	prepay()
	// Ingress: the tunnel to the harbour goes through a circuit of other
	// relays, so the harbour sees an exit relay's address, the exit sees a
	// TLS connection to a relay, and nobody sees both.
	var ingressMgr *client.Manager
	if ingress > 0 {
		ingressMgr = client.NewManager(ingress, "", anchorXNO, rate, "", "")
		ingressMgr.SetAvoid(map[string]bool{key.Address: true, harbour.Account: true})
		log.Printf("ingress: harbour reached through %d relays; your address is not shown to it", ingress)
	}
	switchHarbour := func() {
		old := harbourPtr.Load()
		reg.Refresh(context.Background())
		next := pickHarbour(old.Account)
		if next == nil {
			log.Printf("harbour %s keeps failing and no other public relay is listed; staying", client.Short(old.Account))
			return
		}
		harbourPtr.Store(next)
		poolTag = ""
		os.Remove(tagPath)
		if ingressMgr != nil {
			ingressMgr.SetAvoid(map[string]bool{key.Address: true, next.Account: true})
		}
		log.Printf("moving to harbour %s (%s, %s); descriptor will be republished", client.Short(next.Account), next.Country, next.Desc.Addr())
		prepay()
	}
	dial := func() (net.Conn, error) {
		harbour := harbourPtr.Load()
		if ingressMgr == nil {
			return relay.DialRelay(harbour, 20*time.Second)
		}
		c, err := ingressMgr.Circuit()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", client.ErrIngress, err) // our side, not the harbour's
		}
		st, err := c.Open(harbour.Desc.Addr(), 25*time.Second)
		if err != nil {
			c.Close()
			return nil, err
		}
		return relay.DialRelayOver(harbour, 25*time.Second, client.NewStreamConn(st))
	}
	failures := 0
	for {
		harbour := harbourPtr.Load()
		conn, err := dial()
		if err != nil {
			if stdErrors.Is(err, client.ErrIngress) {
				log.Printf("ingress circuit not ready: %v; retrying in 30 s", err)
				time.Sleep(30 * time.Second)
				continue
			}
			failures++
			log.Printf("harbour %s unreachable (%d): %v; retrying in 30 s", client.Short(harbour.Account), failures, err)
			if failures >= 3 {
				switchHarbour()
				failures = 0
			}
			time.Sleep(30 * time.Second)
			continue
		}
		failures = 0
		hello := relay.HomeHello(key, harbour.Pub, poolTag)
		if _, err := conn.Write(hello); err != nil {
			conn.Close()
			continue
		}
		r := bufio.NewReader(conn)
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		ack, err := wire.ReadCell(r)
		conn.SetReadDeadline(time.Time{})
		if err != nil {
			conn.Close()
			log.Printf("harbour did not answer HOME_HELLO: %v; retrying in 30 s", err)
			time.Sleep(30 * time.Second)
			continue
		}
		if ack.Cmd != wire.CmdHomeOK {
			conn.Close()
			log.Printf("harbour rejected HOME_HELLO: %s", string(ack.Payload))
			if strings.Contains(string(ack.Payload), "pool") { // pool unknown or spent: pay a fresh one
				os.Remove(tagPath)
				poolTag = ""
				prepay()
				continue
			}
			time.Sleep(30 * time.Second)
			continue
		}
		log.Printf("attached to harbour %s (%s); earning as home node %s", client.Short(harbour.Account), harbour.Desc.Addr(), key.Address)
		s.ServeHomeTunnel(conn, r) // returns when the tunnel drops
		log.Println("harbour tunnel closed; reconnecting")
		time.Sleep(5 * time.Second)
	}
}
