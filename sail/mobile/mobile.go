// Package mobile is the surface gomobile binds for the Android app: it runs
// the Sailnet client in-process and feeds it from a TUN file descriptor
// through a userspace network stack, so every app on the device goes
// through the circuit with no SOCKS configuration.
package mobile

import (
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
	ExitCC      string `json:"exitCC"`      // preferred exit country, "" = any
	Anchor      string `json:"anchor"`      // XNO per prepaid anchor, default 0.0005
	MaxRate     string `json:"maxRate"`     // max XNO per MiB on any hop; "" = three times the median published price
	Stealth     bool   `json:"stealth"`     // no direct ledger calls once the wallet state is cached
	Bridges     string `json:"bridges"`     // bridge lines, newline separated
	DNSUpstream string `json:"dnsUpstream"` // resolver asked at the exit, default 1.1.1.1:53
	Nick        string `json:"nick"`        // replaces the wallet address and device IPs in every log and screen
	Censored    bool   `json:"censored"`    // bridges only, no probes, never a direct ledger call
}

var (
	mu       sync.Mutex
	mgr      *client.Manager
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
	mu.Lock()
	defer mu.Unlock()
	if mgr != nil {
		return fmt.Errorf("already running")
	}
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
	if o.Hops < 2 || o.Hops > 4 {
		o.Hops = 3
	}
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
	if o.Stealth {
		m = client.NewStealthManager(o.Hops, o.ExitCC, o.Anchor, o.MaxRate, "")
		if noChain != nil {
			m.AllowDirectBootstrap(true)
			log.Printf("first run: ledger reached directly until the wallet state is cached")
		}
	} else {
		m = client.NewManager(o.Hops, o.ExitCC, o.Anchor, o.MaxRate, "", "")
	}
	if o.Censored {
		m.SetCensored(true)
		log.Printf("censored-network profile: bridges only, no probes, no direct ledger access")
	}
	mgr = m
	started = time.Now()
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

// SetExitCC changes the preferred exit country for the next circuit.
func SetExitCC(cc string) {
	mu.Lock()
	m := mgr
	mu.Unlock()
	if m != nil {
		m.SetExitCC(strings.ToUpper(strings.TrimSpace(cc)))
	}
}

// Address returns the wallet address to fund (creates the wallet if needed).
func Address(home string) string {
	os.Setenv("SAIL_HOME", home)
	return client.EnsureWallet().Address
}

// Status returns a JSON object: running, path, balance, relays, uptime,
// bytesUp, bytesDown, address, error, log.
func Status() string {
	mu.Lock()
	m := mgr
	e := lastErr
	mu.Unlock()
	out := map[string]any{"running": m != nil, "error": e}
	if m != nil {
		for k, v := range m.StatusJSON() {
			out[k] = v
		}
		out["uptime"] = int(time.Since(started).Seconds())
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
		} else if ip := net.ParseIP(id.LocalAddress.String()); ip != nil && ip.IsPrivate() {
			return // the fake DNS address or a LAN host: nothing behind the exit can reach it
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
					}
				}()
			}
		}
		if ip := net.ParseIP(id.LocalAddress.String()); ip != nil && ip.IsPrivate() {
			return
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
		defer func() { log.Printf("udp flow closed: %d datagrams up, %d down", up, down) }()
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
