package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dhyabi2/sail/nano"
	"github.com/dhyabi2/sail/wire"
)

// Ledger over the entry. A fresh client has no circuit and no cached ledger
// state, and it must not connect to a Nano node or to any website to get
// them: the only connection it ever makes is to its entry relay. So the
// entry forwards a small, rate-limited set of read/publish/work requests to
// its own ledger source and streams the answer back on circuit 0.

var rpcAllowed = map[string]bool{
	"account_info": true, "account_history": true, "account_balance": true,
	"blocks_info": true, "block_info": true, "receivable": true, "pending": true,
	"faucet":  true, // not a ledger action: forwarded to the website faucet with the client's IP
	"process": true, "work_generate": true, "block_count": true,
}

// rpcLimiter bounds how much ledger work one address can push through an
// entry: enough for a first run and a wallet refresh, not enough to make
// relays a free RPC proxy.
type rpcLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func (l *rpcLimiter) allow(ip string, limit int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	now := time.Now()
	var keep []time.Time
	for _, t := range l.hits[ip] {
		if now.Sub(t) < window {
			keep = append(keep, t)
		}
	}
	if len(keep) >= limit {
		l.hits[ip] = keep
		return false
	}
	l.hits[ip] = append(keep, now)
	if len(l.hits) > 50000 {
		l.hits = map[string][]time.Time{}
	}
	return true
}

var rpcLimit rpcLimiter

// handleRPC answers a CmdRPC cell from a client connection.
func (s *Server) handleRPC(cell *wire.Cell, in *connWriter, remote string) {
	reply := func(body []byte) {
		seq := uint16(0)
		for len(body) > 0 {
			n := len(body)
			if n > 1000 {
				n = 1000
			}
			if in.write(&wire.Cell{Cmd: wire.CmdRPCReply, StreamID: cell.StreamID, Payload: body[:n]}) != nil {
				return
			}
			body = body[n:]
			seq++
		}
		in.write(&wire.Cell{Cmd: wire.CmdRPCReply, StreamID: cell.StreamID})
	}
	fail := func(msg string) { reply([]byte(`{"error":` + jsonString(msg) + `}`)) }
	ip, _, _ := net.SplitHostPort(remote)
	if ip == "" {
		ip = remote
	}
	var req struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(cell.Payload, &req) != nil || !rpcAllowed[req.Action] {
		fail("action not allowed here")
		return
	}
	limit, window := 240, time.Hour
	if req.Action == "work_generate" || req.Action == "process" {
		limit = 30
	}
	if !rpcLimit.allow(ip+"/"+req.Action, limit, window) {
		fail("rate limit")
		return
	}
	if req.Action == "faucet" {
		reply(forwardFaucet(cell.Payload, ip))
		return
	}
	if s.Nano == nil {
		fail("no ledger source")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := s.Nano.CallRaw(ctx, cell.Payload)
	if err != nil {
		// A node's own error text, unwrapped: the client decides on it (an
		// unopened account answers "Account not found", which is normal).
		var re *nano.RPCError
		if errors.As(err, &re) {
			fail(re.Msg)
			return
		}
		log.Printf("ledger for a client (%s): %v", req.Action, err)
		fail(err.Error())
		return
	}
	reply(out)
}

// FaucetURL is where a relay sends a client's faucet claim; the client's own
// address goes along so the faucet's per-IP limit counts the client, not the
// relay. FAUCET_SECRET, when this relay has it, proves that header.
var FaucetURL = "https://www.sailnet.space/api/faucet"

func forwardFaucet(body []byte, clientIP string) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, FaucetURL, bytes.NewReader(body))
	if err != nil {
		return []byte(`{"ok":false,"amount":"0.0005","error":"faucet request failed"}`)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", clientIP)
	if sec := os.Getenv("FAUCET_SECRET"); sec != "" {
		req.Header.Set("X-Faucet-Secret", sec)
	}
	resp, err := (&http.Client{Timeout: 150 * time.Second}).Do(req)
	if err != nil {
		return []byte(`{"ok":false,"amount":"0.0005","error":"faucet unreachable from the relay: the registration amount of 0.0005 XNO must be sent by hand"}`)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if len(out) == 0 || out[0] != '{' {
		return []byte(`{"ok":false,"amount":"0.0005","error":"faucet answered without JSON (HTTP ` + itoa(resp.StatusCode) + `)"}`)
	}
	return out
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// EntryRPC is the client side: one connection to an entry relay, reused for
// ledger requests until a circuit exists. Safe for one caller at a time.
type EntryRPC struct {
	mu    sync.Mutex
	rel   *RelayInfo
	conn  net.Conn
	w     *connWriter
	r     *bufio.Reader
	seq   uint16
	dialT time.Duration
}

// NewEntryRPC prepares a ledger channel through rel.
func NewEntryRPC(rel *RelayInfo, dialTimeout time.Duration) *EntryRPC {
	return &EntryRPC{rel: rel, dialT: dialTimeout}
}

func (e *EntryRPC) connect() error {
	if e.conn != nil {
		return nil
	}
	conn, err := DialRelay(e.rel, e.dialT)
	if err != nil {
		return err
	}
	e.conn, e.w, e.r = conn, newConnWriter(conn, true), bufio.NewReader(conn)
	return nil
}

// Close drops the connection.
func (e *EntryRPC) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reset()
}

func (e *EntryRPC) reset() {
	if e.w != nil {
		e.w.stop()
	}
	if e.conn != nil {
		e.conn.Close()
	}
	e.conn, e.w, e.r = nil, nil, nil
}

// Call sends one JSON request and returns the JSON answer.
func (e *EntryRPC) Call(body []byte, timeout time.Duration) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(body) > wire.PayloadSize {
		return nil, errors.New("request too large for one cell")
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := e.connect(); err != nil {
			return nil, err
		}
		e.seq++
		id := e.seq
		if err := e.w.write(&wire.Cell{Cmd: wire.CmdRPC, StreamID: id, Payload: body}); err != nil {
			e.reset()
			continue
		}
		e.conn.SetReadDeadline(time.Now().Add(timeout))
		var out []byte
		ok := false
		for i := 0; i < 256; i++ {
			cell, err := wire.ReadCell(e.r)
			if err != nil {
				break
			}
			if cell.Cmd == wire.CmdError {
				e.reset()
				return nil, fmt.Errorf("entry: %s", cell.Payload)
			}
			if cell.Cmd != wire.CmdRPCReply || cell.StreamID != id {
				continue
			}
			if len(cell.Payload) == 0 {
				ok = true
				break
			}
			out = append(out, cell.Payload...)
		}
		e.conn.SetReadDeadline(time.Time{})
		if ok {
			var e2 struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(out, &e2) == nil && strings.Contains(e2.Error, "rate limit") {
				return nil, errors.New("entry: rate limit")
			}
			return out, nil
		}
		e.reset()
	}
	return nil, errors.New("entry did not answer")
}
