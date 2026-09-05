// Package mobile is the surface gomobile binds for the Android app: it runs
// the Sailnet client in-process and feeds it from a TUN file descriptor
// through a userspace network stack, so every app on the device goes
// through the circuit with no SOCKS configuration.
package mobile

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dhyabi2/sail/client"
	"github.com/dhyabi2/sail/nano"
	"github.com/dhyabi2/sail/relay"
	"github.com/xjasonlyu/tun2socks/v2/core"
	"github.com/xjasonlyu/tun2socks/v2/core/adapter"
	"github.com/xjasonlyu/tun2socks/v2/core/device"
	"github.com/xjasonlyu/tun2socks/v2/core/device/fdbased"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// Protector marks a socket so the operating system's VPN routes it outside
// the tunnel (Android VpnService.protect). Without it the client's own
// connections to relays would loop back into the TUN.
type Protector interface {
	Protect(fd int) bool
}

// Options is the JSON the app passes to Start.
type Options struct {
	Hops        int    `json:"hops"`        // 2..4, default 3
	ExitCC      string `json:"exitCC"`      // preferred exit country, "" = any (optional)
	ExcludeCC   string `json:"excludeCC"`   // exit countries never to use, comma-separated
	Anchor      string `json:"anchor"`      // XNO per prepaid anchor, default 0.0005
	MaxRate     string `json:"maxRate"`     // max XNO per MiB on any hop; "" = three times the median published price
	Stealth     bool   `json:"stealth"`     // ignored: always on
	Bridges     string `json:"bridges"`     // bridge lines, newline separated
	DNSUpstream string `json:"dnsUpstream"` // resolver asked at the exit, default 1.1.1.1:53
	Nick        string `json:"nick"`        // replaces the wallet address and device IPs in every log and screen
	Censored    bool   `json:"censored"`    // ignored: always on
	RPCURL      string `json:"rpcUrl"`      // Nano RPC endpoint tried first, default Sailnet's endpoint
	RPCKey      string `json:"rpcKey"`      // API key for rpc.nano.to (sent to that host only)
}

var (
	mu       sync.Mutex
	mgr      *client.Manager
	starting bool // Start is running; Status() reports it without waiting
	netst    *stack.Stack
	tunDev   device.Device
	lastErr  string
	started  time.Time
	upstream = "1.1.1.1:53"
)

type ringLog struct {
	mu    sync.Mutex
	lines []string
}

func (r *ringLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.lines = append(r.lines, strings.TrimRight(string(p), "\n"))
	if len(r.lines) > 300 {
		r.lines = r.lines[len(r.lines)-300:]
	}
	r.mu.Unlock()
	return len(p), nil
}

var logs = &ringLog{}

// Start runs the client. home is a writable directory for the wallet and
// caches; optionsJSON is an Options object; tunFd is the TUN descriptor from
// VpnService.establish() (0 = no TUN); mtu its MTU.
func Start(home, optionsJSON string, tunFd int, mtu int, p Protector) (err error) {
	// The lock is held only around the shared fields, never across the
	// slow work (relay list, RTT probes, payment): Status() must answer at
	// once while starting, or the app's screen freezes on it.
	mu.Lock()
	if mgr != nil || starting {
		mu.Unlock()
		return fmt.Errorf("already running")
	}
	starting = true
	mu.Unlock()
	defer func() {
		mu.Lock()
		starting = false
		mu.Unlock()
	}()
	client.SetLastStage("Reading the relay list")
	defer func() {
		if err != nil {
			lastErr = err.Error()
		}
	}()
	lastErr = ""
	var o Options
	if optionsJSON != "" {
		if err := json.Unmarshal([]byte(optionsJSON), &o); err != nil {
			return fmt.Errorf("options: %w", err)
		}
	}
	o.Hops = 3 // the protocol's choice, not a setting
	if o.Anchor == "" {
		o.Anchor = "0.0005"
	}
	if o.MaxRate == "" {
		o.MaxRate = "0"
	}
	if o.DNSUpstream != "" {
		upstream = o.DNSUpstream
	}
	os.Setenv("SAIL_HOME", home)
	os.MkdirAll(home, 0o700)
	if strings.TrimSuffix(strings.TrimSpace(o.RPCURL), "/") == "https://rpc.nano.to" && strings.TrimSpace(o.RPCKey) == "" {
		o.RPCURL = "" // the earlier build's default; without a key it means "use Sailnet's endpoint"
	}
	nano.ConfigureRPC(o.RPCURL, o.RPCKey)
	key0 := client.EnsureWallet()
	client.SetNick(o.Nick, key0.Address)
	log.SetOutput(client.RedactingWriter{W: io.MultiWriter(os.Stderr, logs)})
	if p != nil {
		relay.DialControl = func(network, address string, c syscall.RawConn) error {
			var ok bool
			c.Control(func(fd uintptr) { ok = p.Protect(int(fd)) })
			if !ok {
				return fmt.Errorf("protect failed")
			}
			return nil
		}
	}
	key := client.EnsureWallet()
	for _, line := range strings.Split(o.Bridges, "\n") {
		if strings.TrimSpace(line) != "" {
			client.AddBridge(strings.TrimSpace(line))
		}
	}
	_, noChain := os.Stat(filepath.Join(home, "chain-"+key.Address[len(key.Address)-8:]+".json"))
	var m *client.Manager
	// Stealth and the censored-network profile are always on; the options
	// are accepted for compatibility and ignored.
	m = client.NewStealthManagerBootstrap(o.Hops, o.ExitCC, o.Anchor, o.MaxRate, "", noChain != nil)
	if noChain != nil {
		log.Printf("first run: ledger read through the entry relay until the wallet state is cached")
	}
	m.SetCensored(true)
	m.SetExcludeExit(o.ExcludeCC)
	mgr = m
	started = time.Now()
	go func() { // keep trying while the tunnel is up: funds arriving become a circuit without a tap
		for {
			time.Sleep(20 * time.Second)
			mu.Lock()
			cur := mgr
			mu.Unlock()
			if cur != m {
				return
			}
			if c, err := m.Circuit(); err == nil && c != nil {
				m.StopFundsWatch()
				continue
			} else if err != nil && !strings.Contains(err.Error(), "retrying shortly") {
				logFundsErr(err)
				if strings.Contains(err.Error(), "no XNO") {
					m.EnsureFundsWatch() // the entry tells us the moment funds confirm
				}
			}
		}
	}()
	if tunFd > 0 {
		dev, err := fdbased.Open(fmt.Sprint(tunFd), uint32(mtu), 0)
		if err != nil {
			mgr = nil
			return fmt.Errorf("tun: %w", err)
		}
		st, err := core.CreateStack(&core.Config{LinkEndpoint: dev, TransportHandler: &handler{m: m}})
		if err != nil {
			dev.Close()
			mgr = nil
			return fmt.Errorf("netstack: %w", err)
		}
		netst, tunDev = st, dev
		log.Printf("tun attached (fd %d, mtu %d)", tunFd, mtu)
	}
	go m.Circuit() // build the first circuit now so the first app request does not wait for payment
	return nil
}

// Stop tears the tunnel and the client down.
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if netst != nil {
		netst.Close()
		netst = nil
	}
	if tunDev != nil {
		tunDev.Close()
		tunDev = nil
	}
	mgr = nil
}

// Rebuild drops the current circuit and builds a new one (new exit).
func Rebuild() {
	mu.Lock()
	m := mgr
	mu.Unlock()
	if m == nil {
		return
	}
	go func() {
		if c, err := m.Circuit(); err == nil {
			c.Close()
		}
		m.Circuit()
	}()
}

// Refresh pockets pending payments and re-reads the balance (the Refresh
// button); returns the balance in XNO, "" when unknown.
func Refresh() string {
	mu.Lock()
	m := mgr
	mu.Unlock()
	if m == nil {
		return ""
	}
	return m.RefreshFunds()
}

// SetExcludeExit changes the excluded exit countries for the next circuit.
func SetExcludeExit(list string) {
	mu.Lock()
	m := mgr
	mu.Unlock()
	if m != nil {
		m.SetExcludeExit(list)
	}
}

// Countries returns the relay countries known to this client as a JSON
// array, for the exclusion picker; works before the first connection.
func Countries() string {
	mu.Lock()
	m := mgr
	mu.Unlock()
	var cs []string
	if m != nil {
		cs = m.Countries()
	} else {
		cs = client.CachedCountries()
	}
	if cs == nil {
		cs = []string{}
	}
	b, _ := json.Marshal(cs)
	return string(b)
}

func Address(home string) string {
	os.Setenv("SAIL_HOME", home)
	return client.EnsureWallet().Address
}

// Status returns a JSON object: running, path, balance, relays, uptime,
// bytesUp, bytesDown, address, error, log.
func startingNow() bool {
	mu.Lock()
	defer mu.Unlock()
	return starting
}

func Status() string {
	mu.Lock()
	m := mgr
	e := lastErr
	mu.Unlock()
	out := map[string]any{"running": m != nil, "error": e, "starting": startingNow(), "stage": client.LastStage()}
	if m != nil {
		for k, v := range m.StatusJSON() {
			out[k] = v
		}
		out["uptime"] = int(time.Since(started).Seconds())
		out["needsFunds"] = m.NeedsFunds()
		out["stage"] = m.Stage()
	}
	logs.mu.Lock()
	n := len(logs.lines)
	if n > 60 {
		out["log"] = strings.Join(logs.lines[n-60:], "\n")
	} else {
		out["log"] = strings.Join(logs.lines, "\n")
	}
	logs.mu.Unlock()
	b, _ := json.Marshal(out)
	return string(b)
}

// Relays returns a JSON array describing every relay the client knows.
func Relays() string {
	mu.Lock()
	m := mgr
	mu.Unlock()
	if m == nil {
		return "[]"
	}
	b, _ := json.Marshal(m.RelaysJSON())
	return string(b)
}

// handler routes flows from the userspace stack into the circuit.
type handler struct{ m *client.Manager }

func (h *handler) HandleTCP(conn adapter.TCPConn) {
	go func() {
		defer conn.Close()
		id := conn.ID()
		dst := net.JoinHostPort(id.LocalAddress.String(), fmt.Sprint(id.LocalPort))
		if ip := net.ParseIP(id.LocalAddress.String()); ip != nil && ip.To4() == nil {
			return // IPv6 is routed into the tunnel so it cannot leak, but not carried (exits are IPv4)
		}
		if id.LocalPort == 53 { // DNS over TCP: ask the resolver at the exit instead
			dst = upstream
		} else if id.LocalPort == 80 && !client.AllowPlainHTTP {
			logHTTPRefused()
			return // plain HTTP: readable by the exit and every network after it
		} else if ip := net.ParseIP(id.LocalAddress.String()); ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
			// A LAN host (a Nano node on this network, a printer, a router):
			// nothing behind the exit can reach it, so it is reached directly
			// on a protected socket, outside the tunnel.
			direct, err := (&net.Dialer{Timeout: 10 * time.Second, Control: relay.DialControl}).Dial("tcp", dst)
			if err != nil {
				return
			}
			defer direct.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(direct, conn); direct.Close(); done <- struct{}{} }()
			go func() { io.Copy(conn, direct); done <- struct{}{} }()
			<-done
			return
		}
		c, err := h.m.Circuit()
		if err != nil {
			log.Printf("tcp flow: %v", err) // never the destination
			return
		}
		st, err := c.OpenOptimistic(dst)
		if err != nil {
			return
		}
		defer st.Close()
		done := make(chan struct{}, 2)
		go func() { io.Copy(client.Up(st), conn); st.Close(); done <- struct{}{} }()
		go func() { io.Copy(client.Down(conn), st); done <- struct{}{} }()
		<-done
	}()
}

func (h *handler) HandleUDP(conn adapter.UDPConn) {
	go func() {
		defer conn.Close()
		id := conn.ID()
		if id.LocalPort == 53 { // DNS: resolved through the circuit at the exit
			buf := make([]byte, 4096)
			for {
				conn.SetReadDeadline(time.Now().Add(60 * time.Second))
				n, from, err := conn.ReadFrom(buf)
				if err != nil {
					return
				}
				q := append([]byte(nil), buf[:n]...)
				go func() {
					if ans, err := h.m.ResolveViaCircuit(q, upstream); err == nil {
						conn.WriteTo(ans, from)
					} else {
						logDNSErr(err)
					}
				}()
			}
		}
		if ip := net.ParseIP(id.LocalAddress.String()); ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
			// LAN UDP (a local node's peering port, discovery): relayed
			// directly on a protected socket, outside the tunnel.
			direct, err := (&net.Dialer{Timeout: 10 * time.Second, Control: relay.DialControl}).Dial("udp", net.JoinHostPort(id.LocalAddress.String(), fmt.Sprint(id.LocalPort)))
			if err != nil {
				return
			}
			defer direct.Close()
			go func() {
				buf := make([]byte, 65535)
				for {
					n, err := direct.Read(buf)
					if err != nil {
						conn.Close()
						return
					}
					if _, err := conn.WriteTo(buf[:n], nil); err != nil {
						return
					}
				}
			}()
			buf := make([]byte, 65535)
			for {
				n, _, err := conn.ReadFrom(buf)
				if err != nil {
					return
				}
				if _, err := direct.Write(buf[:n]); err != nil {
					return
				}
			}
		}
		// Any other UDP flow (QUIC, calls, games): a datagram stream to the exit.
		dst := relay.UDPPrefix + net.JoinHostPort(id.LocalAddress.String(), fmt.Sprint(id.LocalPort))
		c, err := h.m.Circuit()
		if err != nil {
			return
		}
		st, err := c.OpenOptimistic(dst)
		if err != nil {
			return
		}
		defer st.Close()
		var first net.Addr
		var up, down int
		defer func() {
			if os.Getenv("SAIL_DEBUG") != "" {
				log.Printf("udp flow closed: %d datagrams up, %d down", up, down)
			}
		}()
		go func() { // exit → app
			var d relay.Deframer
			buf := make([]byte, 4096)
			for {
				n, err := st.Read(buf)
				if err != nil {
					conn.Close()
					return
				}
				d.Push(buf[:n])
				for dg := d.Next(); dg != nil; dg = d.Next() {
					if first != nil {
						client.Down(discard{}).Write(dg) // traffic counter
						down++
						conn.WriteTo(dg, first)
					}
				}
			}
		}()
		buf := make([]byte, relay.MaxDatagram)
		for { // app → exit
			conn.SetReadDeadline(time.Now().Add(120 * time.Second))
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if first == nil {
				first = from
			}
			up++
			if _, err := client.Up(st).Write(relay.Frame(buf[:n])); err != nil {
				return
			}
		}
	}()
}

// discard is a writer used only to feed the traffic counters.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

var dnsErrAt time.Time

// logDNSErr reports why a lookup could not be answered, at most once per
// 10 s: a silent failure here looked like a dead app.
func logDNSErr(err error) {
	mu.Lock()
	defer mu.Unlock()
	if time.Since(dnsErrAt) < 10*time.Second {
		return
	}
	dnsErrAt = time.Now()
	log.Printf("dns: %v", err)
}

var fundsErrAt time.Time

func logFundsErr(err error) {
	mu.Lock()
	defer mu.Unlock()
	if time.Since(fundsErrAt) < 2*time.Minute {
		return
	}
	fundsErrAt = time.Now()
	log.Printf("circuit: %v", err)
}

var httpRefusedAt time.Time

func logHTTPRefused() {
	mu.Lock()
	defer mu.Unlock()
	if time.Since(httpRefusedAt) < time.Minute {
		return
	}
	httpRefusedAt = time.Now()
	log.Printf("refused a plain HTTP (port 80) connection: only encrypted traffic leaves the exit")
}

// ExportWallet returns the seed to write down, as JSON:
// {"ok":true,"seed":"...","address":"nano_..."} or {"ok":false,"error":"..."}.
//
// Uninstalling an Android app deletes everything it stored, this wallet
// included, and no server anywhere keeps a copy of the seed. So the app has
// to be able to show it, and to take it back.
func ExportWallet(home string) string {
	defer func() { recover() }()
	os.Setenv("SAIL_HOME", home)
	seed, addr, err := client.ExportWallet()
	if err != nil {
		return errJSON(err)
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "seed": seed, "address": addr})
	return string(b)
}

// ImportWallet restores a wallet from a backup and returns
// {"ok":true,"address":"nano_..."} or {"ok":false,"error":"..."}.
//
// The tunnel is stopped first: circuits in flight are paid for out of the
// wallet being replaced. The wallet that was there is kept beside the new
// one, never deleted.
func ImportWallet(home, text string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = errJSON(fmt.Errorf("could not restore that backup"))
		}
	}()
	os.Setenv("SAIL_HOME", home)
	Stop()
	addr, err := client.ImportWallet(text)
	if err != nil {
		return errJSON(err)
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "address": addr})
	return string(b)
}

func errJSON(err error) string {
	b, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
	return string(b)
}

// Funds checks the wallet and claims the free trial if it cannot yet pay,
// without starting a tunnel. The app calls this when it opens: the Connect
// button stays out of reach until the answer says the wallet can pay, so
// nobody is invited to connect with an empty wallet.
//
// Returns JSON: {address, balance, needsFunds, required, faucet, error}.
func Funds(home string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = `{"needsFunds":true,"required":"` + client.AnchorXNO + `","error":"could not check the wallet"}`
		}
	}()
	os.Setenv("SAIL_HOME", home)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	b, _ := json.Marshal(client.EnsureFunded(ctx))
	return string(b)
}
