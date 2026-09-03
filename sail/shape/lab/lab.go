// Package lab captures real record-level traces: ordinary HTTPS on one side,
// Sailnet circuits carrying real HTTPS on the other. It is the measurement
// half of the shaping work; nothing here runs in a relay or a client.
package lab

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dhyabi2/sail/nano"
	"github.com/dhyabi2/sail/relay"
	"github.com/dhyabi2/sail/shape"
	utls "github.com/refraction-networking/utls"
)

// Sites is the default fetch list: ordinary web pages of a range of sizes, so
// neither class is a single traffic pattern.
var Sites = []string{
	"https://example.com/", "https://www.wikipedia.org/", "https://www.bbc.com/",
	"https://news.ycombinator.com/", "https://go.dev/", "https://www.rust-lang.org/",
	"https://www.debian.org/", "https://www.kernel.org/", "https://curl.se/",
	"https://www.python.org/", "https://nginx.org/", "https://www.openssl.org/",
	"https://www.gnu.org/", "https://httpbin.org/bytes/20000", "https://httpbin.org/bytes/2000",
	"https://www.iana.org/", "https://www.rfc-editor.org/", "https://ftp.gnu.org/",
	"https://www.apache.org/", "https://www.postgresql.org/",
}

// chromeConn dials host with a Chrome ClientHello, exactly as the Sailnet
// client does to a relay, so the two classes differ only in what rides inside.
func chromeConn(raw net.Conn, host string, alpn []string) (net.Conn, error) {
	cfg := &utls.Config{ServerName: host, NextProtos: alpn}
	uc := utls.UClient(raw, cfg, utls.HelloChrome_Auto)
	uc.SetDeadline(time.Now().Add(20 * time.Second))
	if err := uc.Handshake(); err != nil {
		raw.Close()
		return nil, err
	}
	uc.SetDeadline(time.Time{})
	return uc, nil
}

// CaptureDirect records one ordinary HTTPS session per URL: a Chrome
// ClientHello, HTTP/1.1 (the protocol a relay's decoy site speaks), a real
// request and a real response, tapped under TLS.
func CaptureDirect(urls []string, sink *shape.Sink, params string) error {
	for _, u := range urls {
		host, path := splitURL(u)
		raw, err := net.DialTimeout("tcp", host+":443", 15*time.Second)
		if err != nil {
			continue
		}
		tap := shape.NewTap(raw, sink, "direct", u, params)
		c, err := chromeConn(tap, host, []string{"http/1.1"})
		if err != nil {
			continue
		}
		// A browser keeps the connection and asks for several resources on it,
		// which is what gives ordinary HTTPS its request/response rhythm. One
		// request per connection would make the negative class too easy.
		c.SetDeadline(time.Now().Add(40 * time.Second))
		paths := []string{path, "/favicon.ico", "/robots.txt"}
		br := bufio.NewReader(c)
		ok := true
		for i, pth := range paths {
			last := i == len(paths)-1
			conn := "keep-alive"
			if last {
				conn = "close"
			}
			req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0 Safari/537.36\r\nAccept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8\r\nAccept-Language: en-US,en;q=0.9\r\nAccept-Encoding: gzip, deflate\r\nConnection: %s\r\n\r\n", pth, host, conn)
			if _, err := c.Write([]byte(req)); err != nil {
				ok = false
				break
			}
			if err := drainResponse(br); err != nil {
				ok = false
				break
			}
		}
		_ = ok
		c.Close()
		tap.Close()
	}
	return nil
}

// Net is a running three-hop test network on loopback.
type Net struct {
	Infos  []*relay.RelayInfo
	Srvs   []*relay.Server
	closer []func()
}

func (n *Net) Close() {
	for _, f := range n.closer {
		f()
	}
}

// StartNet brings up hops relays on loopback, the last one an exit that may
// reach the real internet.
func StartNet(hops int) (*Net, error) {
	reg := &relay.Registry{}
	n := &Net{}
	for i := 0; i < hops; i++ {
		seed := make([]byte, 32)
		rand.Read(seed)
		key, err := nano.DeriveKey(seed, 0)
		if err != nil {
			return nil, err
		}
		cert, fp, err := relay.SelfSignedCert("127.0.0.1")
		if err != nil {
			return nil, err
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		q, _ := relay.NewQuota("", big.NewInt(1))
		s := &relay.Server{Key: key, Quota: q, TLS: cert, Registry: reg, Exit: i == hops-1, AllowPrivate: true, Decoy: "<html>hello</html>"}
		srv := &http.Server{Handler: s.Handler(), TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}, TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){}}
		go srv.ServeTLS(ln, "", "")
		port := ln.Addr().(*net.TCPAddr).Port
		ri := &relay.RelayInfo{Account: key.Address, Pub: key.Public, Country: fmt.Sprintf("C%d", i), ASN: uint32(i + 1), MinRate: 1, Flags: 3,
			Desc: relay.Descriptor{IP: net.ParseIP("127.0.0.1"), Port: uint16(port), CertFP: fp}}
		reg.Add(ri)
		n.Srvs = append(n.Srvs, s)
		n.Infos = append(n.Infos, ri)
		n.closer = append(n.closer, func() { ln.Close() })
	}
	return n, nil
}

// credit gives the circuit and the inter-relay pools enough quota for a run.
func (n *Net) credit(tag [32]byte) {
	n.Srvs[0].Quota.Credit(hex.EncodeToString(tag[:]), 200<<20, "")
	for i := 1; i < len(n.Srvs); i++ {
		n.Srvs[i].Quota.Credit(relay.PoolTag(n.Infos[i-1].Account, n.Infos[i].Account), 200<<20, "")
	}
}

type fakeConn struct{ *relay.Stream }

func (fakeConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (fakeConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (fakeConn) SetDeadline(time.Time) error      { return nil }
func (fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (fakeConn) SetWriteDeadline(time.Time) error { return nil }

// CaptureTunnel builds one circuit per URL and fetches that URL through it
// over real HTTPS, so what the tap records is a genuine TLS-in-TLS session:
// the outer connection is the tunnel, the inner one is the browser's TLS to
// the site. label distinguishes the shaping variant in the trace file.
func CaptureTunnel(n *Net, urls []string, sink *shape.Sink, label, params string) error {
	var mu sync.Mutex
	var firstErr error
	for _, u := range urls {
		var tag [32]byte
		rand.Read(tag[:])
		n.credit(tag)
		var tap *shape.Tap
		// Only the client's own connection to the entry relay is tapped: that
		// is the one a censor inside the country can see. The relay-to-relay
		// dials that follow during EXTEND run untapped.
		shape.SetDialHook(func(c net.Conn, site string) net.Conn {
			mu.Lock()
			defer mu.Unlock()
			if tap != nil {
				return c
			}
			tap = shape.NewTap(c, sink, label, u, params)
			return tap
		})
		c, err := relay.Build(n.Infos, tag, 20*time.Second, nil, nil)
		shape.SetDialHook(nil)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		tr := &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				st, err := c.Open(addr, 20*time.Second)
				if err != nil {
					return nil, err
				}
				return fakeConn{st}, nil
			},
			TLSHandshakeTimeout: 20 * time.Second,
			DisableKeepAlives:   true,
		}
		resp, err := (&http.Client{Transport: tr, Timeout: 60 * time.Second}).Get(u)
		if err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
		} else if firstErr == nil {
			firstErr = err
		}
		time.Sleep(150 * time.Millisecond) // let the last cells drain before the tap closes
		c.Close()
		tr.CloseIdleConnections()
		if tap != nil {
			tap.Close()
		}
	}
	return firstErr
}

func splitURL(u string) (host, path string) {
	s := strings.TrimPrefix(u, "https://")
	if i := strings.Index(s, "/"); i >= 0 {
		return s[:i], s[i:]
	}
	return s, "/"
}

// drainResponse reads one HTTP/1.1 response off a keep-alive connection.
func drainResponse(br *bufio.Reader) error {
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	return nil
}

// A Sailnet circuit is long-lived and carries a whole browsing session, while
// a single page fetch over ordinary HTTPS is one short connection. Comparing
// the two measures session length, not shaping. These two functions capture
// the matched workload instead: one connection on each side, the same number
// of requests, the same think time between them.

const sessionReqs = 12

func thinkTime() time.Duration { return time.Duration(100+rand.Intn(700)) * time.Millisecond }

// CaptureDirectSession records one long-lived keep-alive HTTPS session per
// site: the reference class for a tunnel that claims to be a web app's
// connection.
func CaptureDirectSession(urls []string, sink *shape.Sink, params string) error {
	for _, u := range urls {
		host, path := splitURL(u)
		raw, err := net.DialTimeout("tcp", host+":443", 15*time.Second)
		if err != nil {
			continue
		}
		tap := shape.NewTap(raw, sink, "direct-session", u, params)
		c, err := chromeConn(tap, host, []string{"http/1.1"})
		if err != nil {
			continue
		}
		c.SetDeadline(time.Now().Add(120 * time.Second))
		br := bufio.NewReader(c)
		paths := []string{path, "/robots.txt", "/favicon.ico"}
		for i := 0; i < sessionReqs; i++ {
			last := i == sessionReqs-1
			conn := "keep-alive"
			if last {
				conn = "close"
			}
			req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0 Safari/537.36\r\nAccept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8\r\nAccept-Language: en-US,en;q=0.9\r\nAccept-Encoding: gzip, deflate\r\nConnection: %s\r\n\r\n", paths[i%len(paths)], host, conn)
			if _, err := c.Write([]byte(req)); err != nil {
				break
			}
			if err := drainResponse(br); err != nil {
				break
			}
			if !last {
				time.Sleep(thinkTime())
			}
		}
		c.Close()
		tap.Close()
	}
	return nil
}

// CaptureTunnelSession builds one circuit and fetches several pages through
// it, with the same think time, so the trace is the matched positive class.
func CaptureTunnelSession(n *Net, urls []string, sink *shape.Sink, label, params string, sessions int) error {
	var firstErr error
	for s := 0; s < sessions; s++ {
		var tag [32]byte
		rand.Read(tag[:])
		n.credit(tag)
		var tap *shape.Tap
		var mu sync.Mutex
		shape.SetDialHook(func(c net.Conn, site string) net.Conn {
			mu.Lock()
			defer mu.Unlock()
			if tap != nil {
				return c
			}
			tap = shape.NewTap(c, sink, label, "session", params)
			return tap
		})
		c, err := relay.Build(n.Infos, tag, 20*time.Second, nil, nil)
		shape.SetDialHook(nil)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		tr := &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				st, err := c.Open(addr, 20*time.Second)
				if err != nil {
					return nil, err
				}
				return fakeConn{st}, nil
			},
			TLSHandshakeTimeout: 20 * time.Second,
			MaxIdleConnsPerHost: 4,
		}
		cl := &http.Client{Transport: tr, Timeout: 60 * time.Second}
		for i := 0; i < sessionReqs; i++ {
			u := urls[(s*sessionReqs+i)%len(urls)]
			resp, err := cl.Get(u)
			if err == nil {
				io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<20))
				resp.Body.Close()
			} else if firstErr == nil {
				firstErr = err
			}
			time.Sleep(thinkTime())
		}
		time.Sleep(200 * time.Millisecond)
		c.Close()
		tr.CloseIdleConnections()
		if tap != nil {
			tap.Close()
		}
	}
	return firstErr
}

// A fair comparison needs the same work on both sides: the same host, the
// same requests, the same response sizes, the same think time. Only then does
// a classifier separate the tunnel from ordinary HTTPS rather than separating
// a long bulk flow from a short page fetch. Each Bulk flow is one connection
// carrying several range-limited downloads from one real site, which is also
// the cover story a long-lived tunnel connection has to hold up: a web app or
// a download keeping a connection open.

type bulkHost struct {
	host  string
	paths []string
}

var bulkHosts = []bulkHost{
	{"ftp.gnu.org", []string{"/gnu/hello/hello-2.12.tar.gz", "/gnu/wget/wget-1.21.tar.gz", "/gnu/tar/tar-1.34.tar.gz", "/gnu/sed/sed-4.9.tar.gz", "/gnu/grep/grep-3.11.tar.gz", "/gnu/gzip/gzip-1.13.tar.gz"}},
	{"www.rfc-editor.org", []string{"/rfc/rfc9000.txt", "/rfc/rfc8446.txt", "/rfc/rfc793.txt", "/rfc/rfc2616.txt", "/rfc/rfc5246.txt", "/rfc/rfc7540.txt"}},
	{"cdn.kernel.org", []string{"/pub/linux/kernel/v6.x/ChangeLog-6.6", "/pub/linux/kernel/v6.x/ChangeLog-6.5", "/pub/linux/kernel/v6.x/ChangeLog-6.4", "/pub/linux/kernel/v6.x/ChangeLog-6.3", "/pub/linux/kernel/v6.x/ChangeLog-6.2", "/pub/linux/kernel/v6.x/ChangeLog-6.1"}},
	{"curl.se", []string{"/ca/cacert.pem", "/ca/cacert.pem", "/ca/cacert.pem", "/ca/cacert.pem", "/ca/cacert.pem", "/ca/cacert.pem"}},
}

// BulkReqs and BulkBytes define the shared workload.
const (
	BulkReqs  = 6
	BulkBytes = 200 << 10
)

func bulkRequest(host, path string, last bool) string {
	conn := "keep-alive"
	if last {
		conn = "close"
	}
	return fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0 Safari/537.36\r\nAccept: */*\r\nAccept-Language: en-US,en;q=0.9\r\nRange: bytes=0-%d\r\nConnection: %s\r\n\r\n", path, host, BulkBytes-1, conn)
}

// CaptureBulkDirect records ordinary HTTPS flows running the shared workload.
func CaptureBulkDirect(flows int, sink *shape.Sink, params string) error {
	for i := 0; i < flows; i++ {
		h := bulkHosts[i%len(bulkHosts)]
		raw, err := net.DialTimeout("tcp", h.host+":443", 15*time.Second)
		if err != nil {
			continue
		}
		tap := shape.NewTap(raw, sink, "direct-bulk", h.host, params)
		c, err := chromeConn(tap, h.host, []string{"http/1.1"})
		if err != nil {
			continue
		}
		c.SetDeadline(time.Now().Add(180 * time.Second))
		br := bufio.NewReader(c)
		for j := 0; j < BulkReqs; j++ {
			last := j == BulkReqs-1
			if _, err := c.Write([]byte(bulkRequest(h.host, h.paths[j%len(h.paths)], last))); err != nil {
				break
			}
			if err := drainResponse(br); err != nil {
				break
			}
			if !last {
				time.Sleep(thinkTime())
			}
		}
		c.Close()
		tap.Close()
	}
	return nil
}

// CaptureBulkTunnel runs the same workload through a circuit: one connection
// to the entry relay, one inner TLS session to the same host, the same six
// range requests, the same think time.
func CaptureBulkTunnel(n *Net, flows int, sink *shape.Sink, label, params string) error {
	var firstErr error
	for i := 0; i < flows; i++ {
		h := bulkHosts[i%len(bulkHosts)]
		var tag [32]byte
		rand.Read(tag[:])
		n.credit(tag)
		var tap *shape.Tap
		var mu sync.Mutex
		shape.SetDialHook(func(c net.Conn, site string) net.Conn {
			mu.Lock()
			defer mu.Unlock()
			if tap != nil {
				return c
			}
			tap = shape.NewTap(c, sink, label, h.host, params)
			return tap
		})
		cir, err := relay.Build(n.Infos, tag, 20*time.Second, nil, nil)
		shape.SetDialHook(nil)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		st, err := cir.Open(h.host+":443", 20*time.Second)
		if err == nil {
			c, err2 := chromeConn(fakeConn{st}, h.host, []string{"http/1.1"})
			if err2 == nil {
				br := bufio.NewReader(c)
				for j := 0; j < BulkReqs; j++ {
					last := j == BulkReqs-1
					if _, err := c.Write([]byte(bulkRequest(h.host, h.paths[j%len(h.paths)], last))); err != nil {
						break
					}
					if err := drainResponse(br); err != nil {
						break
					}
					if !last {
						time.Sleep(thinkTime())
					}
				}
				c.Close()
			} else if firstErr == nil {
				firstErr = err2
			}
		} else if firstErr == nil {
			firstErr = err
		}
		time.Sleep(200 * time.Millisecond)
		cir.Close()
		if tap != nil {
			tap.Close()
		}
	}
	return firstErr
}

// Origin is a TLS web server used as the destination for both classes of the
// matched experiment. Holding the origin constant is what makes the
// comparison clean: the negative fetches it directly, the positive fetches it
// through the circuit, and the server, the requests and the response sizes are
// identical, so a classifier that separates them is separating the tunnel and
// nothing else. It also keeps a tuning run from hammering public mirrors.
type Origin struct {
	Host string
	ln   net.Listener
	cert tls.Certificate
}

func StartOrigin() (*Origin, error) {
	cert, _, err := relay.SelfSignedCert("127.0.0.1")
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	body := make([]byte, 1<<20)
	rand.Read(body) // incompressible, like the archives the real workload fetches
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n := BulkBytes
		if v := r.URL.Query().Get("n"); v != "" {
			fmt.Sscanf(v, "%d", &n)
		}
		if n > len(body) {
			n = len(body)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(body[:n])
	})
	srv := &http.Server{Handler: mux, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}},
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){}}
	go srv.ServeTLS(ln, "", "")
	return &Origin{Host: ln.Addr().String(), ln: ln, cert: cert}, nil
}

func (o *Origin) Close() {
	if o.ln != nil {
		o.ln.Close()
	}
}

// originConn runs the same TLS handshake and request sequence over whatever
// transport it is given: a plain socket for the negative class, a circuit
// stream for the positive one.
func (o *Origin) run(raw net.Conn) error {
	cfg := &utls.Config{ServerName: "127.0.0.1", InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}}
	uc := utls.UClient(raw, cfg, utls.HelloChrome_Auto)
	uc.SetDeadline(time.Now().Add(120 * time.Second))
	if err := uc.Handshake(); err != nil {
		raw.Close()
		return err
	}
	br := bufio.NewReader(uc)
	for j := 0; j < BulkReqs; j++ {
		last := j == BulkReqs-1
		conn := "keep-alive"
		if last {
			conn = "close"
		}
		req := fmt.Sprintf("GET /f%d?n=%d HTTP/1.1\r\nHost: %s\r\nUser-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0 Safari/537.36\r\nAccept: */*\r\nConnection: %s\r\n\r\n", j, BulkBytes, o.Host, conn)
		if _, err := uc.Write([]byte(req)); err != nil {
			uc.Close()
			return fmt.Errorf("request %d: %w", j, err)
		}
		if err := drainResponse(br); err != nil {
			uc.Close()
			return fmt.Errorf("response %d: %w", j, err)
		}
		if !last {
			time.Sleep(thinkTime())
		}
	}
	uc.Close()
	return nil
}

// CaptureLocalDirect records the negative class of the matched experiment.
func CaptureLocalDirect(o *Origin, flows int, sink *shape.Sink, params string) error {
	for i := 0; i < flows; i++ {
		raw, err := net.DialTimeout("tcp", o.Host, 10*time.Second)
		if err != nil {
			return err
		}
		tap := shape.NewTap(raw, sink, "local-direct", o.Host, params)
		o.run(tap)
		tap.Close()
	}
	return nil
}

// CaptureLocalTunnel records the positive class: the same workload against the
// same origin, through a three-hop circuit.
func CaptureLocalTunnel(n *Net, o *Origin, flows int, sink *shape.Sink, label, params string) error {
	var firstErr error
	for i := 0; i < flows; i++ {
		var tag [32]byte
		rand.Read(tag[:])
		n.credit(tag)
		var tap *shape.Tap
		var mu sync.Mutex
		shape.SetDialHook(func(c net.Conn, site string) net.Conn {
			mu.Lock()
			defer mu.Unlock()
			if tap != nil {
				return c
			}
			tap = shape.NewTap(c, sink, label, o.Host, params)
			return tap
		})
		cir, err := relay.Build(n.Infos, tag, 20*time.Second, nil, nil)
		shape.SetDialHook(nil)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		st, err := cir.Open(o.Host, 20*time.Second)
		if err == nil {
			if err := o.run(fakeConn{st}); err != nil && firstErr == nil {
				firstErr = err
			}
		} else if firstErr == nil {
			firstErr = err
		}
		time.Sleep(200 * time.Millisecond)
		cir.Close()
		if tap != nil {
			tap.Close()
		}
	}
	return firstErr
}

// RemoteOrigin points the workload at an origin served elsewhere, for example
// `sailtrace origin` on a VPS, so both classes cross the real internet.
func RemoteOrigin(host string) *Origin { return &Origin{Host: host} }

// ServeOrigin runs the origin in the foreground on addr.
func ServeOrigin(addr string) error {
	cert, _, err := relay.SelfSignedCert("origin")
	if err != nil {
		return err
	}
	body := make([]byte, 1<<20)
	rand.Read(body)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n := BulkBytes
		if v := r.URL.Query().Get("n"); v != "" {
			fmt.Sscanf(v, "%d", &n)
		}
		if n > len(body) {
			n = len(body)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(body[:n])
	})
	srv := &http.Server{Addr: addr, Handler: mux, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}},
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){}}
	return srv.ListenAndServeTLS("", "")
}

// CaptureSocks runs the workload through a running `sailnode client` (its
// SOCKS5 port) and asks it for a fresh circuit between flows through its
// status endpoint. The traces themselves are written by the client process
// (SAIL_TRACE), since that is where the connection to the entry relay lives.
func CaptureSocks(o *Origin, socks, status string, flows int) error {
	for i := 0; i < flows; i++ {
		if i > 0 {
			req, _ := http.NewRequest(http.MethodPost, "http://"+status+"/rebuild", nil)
			req.Header.Set("X-Sail", "1")
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
			// The client builds its next circuit on the first SOCKS dial, so
			// there is nothing to wait for here beyond letting it tear down.
			time.Sleep(2 * time.Second)
		}
		// The client builds a circuit on this dial; right after a relay
		// restart that can fail for a while, so retry before giving up.
		var raw net.Conn
		var err error
		for attempt := 0; attempt < 8; attempt++ {
			raw, err = socksDial(socks, o.Host)
			if err == nil {
				break
			}
			time.Sleep(6 * time.Second)
		}
		if err != nil {
			return err
		}
		if err := o.run(raw); err != nil {
			fmt.Fprintf(os.Stderr, "flow %d: %v\n", i, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	// A trace is written when its connection closes; close the last circuit
	// so the final flow is not lost when the client is stopped.
	req, _ := http.NewRequest(http.MethodPost, "http://"+status+"/rebuild", nil)
	req.Header.Set("X-Sail", "1")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	time.Sleep(2 * time.Second)
	return nil
}

func waitConnected(status string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, "http://"+status+"/status", nil)
		req.Header.Set("X-Sail", "1")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.Contains(string(b), `"path":"`) && !strings.Contains(string(b), `"path":""`) {
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("client did not reconnect within %s", d)
}

// socksDial opens host:port through a SOCKS5 proxy with remote name resolution.
func socksDial(proxy, target string) (net.Conn, error) {
	c, err := net.DialTimeout("tcp", proxy, 10*time.Second)
	if err != nil {
		return nil, err
	}
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		c.Close()
		return nil, err
	}
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	c.SetDeadline(time.Now().Add(60 * time.Second))
	c.Write([]byte{5, 1, 0})
	var r [2]byte
	if _, err := io.ReadFull(c, r[:]); err != nil || r[1] != 0 {
		c.Close()
		return nil, fmt.Errorf("socks: no-auth refused")
	}
	req := append([]byte{5, 1, 0, 3, byte(len(host))}, host...)
	req = append(req, byte(port>>8), byte(port))
	c.Write(req)
	var h [4]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		c.Close()
		return nil, err
	}
	if h[1] != 0 {
		c.Close()
		return nil, fmt.Errorf("socks: connect failed (%d)", h[1])
	}
	var skip int
	switch h[3] {
	case 1:
		skip = 4 + 2
	case 3:
		var n [1]byte
		io.ReadFull(c, n[:])
		skip = int(n[0]) + 2
	case 4:
		skip = 16 + 2
	}
	io.ReadFull(c, make([]byte, skip))
	c.SetDeadline(time.Time{})
	return c, nil
}
