package client

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Traffic counters for the status surfaces (app screen, browser extension).
var (
	bytesUp   atomic.Int64 // toward the network
	bytesDown atomic.Int64 // toward the application
	startedAt = time.Now()
)

// CountingWriter adds what it writes to a counter.
type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// Up and Down wrap writers in the two directions of a relayed flow.
func Up(w io.Writer) io.Writer   { return countingWriter{w, &bytesUp} }
func Down(w io.Writer) io.Writer { return countingWriter{w, &bytesDown} }

// StatusJSON summarises the client for UIs.
func (m *manager) StatusJSON() map[string]any {
	out := map[string]any{
		"running":   true,
		"path":      Redact(m.Path()),
		"balance":   m.Balance(),
		"relays":    m.Relays(),
		"address":   m.key.Address,
		"uptime":    int(time.Since(startedAt).Seconds()),
		"bytesUp":   bytesUp.Load(),
		"bytesDown": bytesDown.Load(),
		"stealth":   m.stealth,
		"hops":      m.opts.hops,
		"exitCC":    m.opts.exitCC,
		"nick":      Nick(),
	}
	return out
}

// RelaysJSON lists what the client knows about every relay.
func (m *manager) RelaysJSON() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []map[string]any
	for _, r := range m.reg.All() {
		e := map[string]any{"account": r.Account, "cc": r.Country, "asn": r.ASN, "port": r.Desc.Port, "exit": r.Flags&2 != 0, "home": r.Flags&4 != 0, "bridge": r.Unlisted, "score": m.scoreOf(r.Account)}
		if rtt, ok := m.rtt[r.Account]; ok {
			e["rttMs"] = rtt.Milliseconds()
		}
		out = append(out, e)
	}
	return out
}

// ServeStatus answers GET /status and /relays as JSON on addr (loopback),
// with CORS open so a browser extension's popup can read it.
func (m *manager) ServeStatus(addr string) {
	mux := http.NewServeMux()
	// Only browser extensions may read this. A web page's fetch carries an
	// http(s) Origin and gets no CORS header (the browser hides the body);
	// the X-Sail header forces a preflight, which such pages also fail.
	guard := func(w http.ResponseWriter, r *http.Request) bool {
		o := r.Header.Get("Origin")
		if strings.HasPrefix(o, "chrome-extension://") || strings.HasPrefix(o, "moz-extension://") || strings.HasPrefix(o, "safari-web-extension://") {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Access-Control-Allow-Headers", "X-Sail")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
			w.Header().Set("Vary", "Origin")
		} else if o != "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return false
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return false
		}
		if r.Header.Get("X-Sail") == "" {
			http.Error(w, "missing X-Sail header", http.StatusForbidden)
			return false
		}
		return true
	}
	h := func(f func() any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !guard(w, r) {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(f())
		}
	}
	mux.HandleFunc("/status", h(func() any { return m.StatusJSON() }))
	mux.HandleFunc("/relays", h(func() any { return m.RelaysJSON() }))
	mux.HandleFunc("/flows", h(func() any { var v []any; json.Unmarshal([]byte(Flows()), &v); return v }))
	mux.HandleFunc("/rebuild", func(w http.ResponseWriter, r *http.Request) {
		if !guard(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		m.mu.Lock()
		if m.cur != nil {
			m.cur.Close()
			m.cur = nil
		}
		m.mu.Unlock()
		go m.circuit()
		w.Write([]byte(`{"ok":true}`))
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("status: %v", err)
		return
	}
	log.Printf("status endpoint on http://%s/status", addr)
	http.Serve(ln, mux)
}

// SetExitCC changes the preferred exit country for the next circuit.
func (m *manager) SetExitCC(cc string) {
	m.mu.Lock()
	m.opts.exitCC = cc
	m.mu.Unlock()
}
