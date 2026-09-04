package relay

import (
	"bufio"
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"github.com/dhyabi2/sail/wire"
)

// Confirmations over WebSocket. One upstream subscription per relay (to
// WSUpstream, the standard Nano node WebSocket protocol) carries the union
// of the accounts clients asked to watch; each confirmation is pushed to the
// watching connections as a CmdNotify cell. Nothing is signed here: the
// feed is read-only, and blocks are always signed by the client that owns
// the key.

// WSUpstream is the WebSocket a relay subscribes to.
var WSUpstream = "wss://ws.nano.to"

// Notification is what a client receives.
type Notification struct {
	Account string `json:"account"`
	Amount  string `json:"amount"`
	Hash    string `json:"hash"`
	Subtype string `json:"subtype"`
	Link    string `json:"link"` // link_as_account: the receiver of a send
}

type upstreamMsg struct {
	Topic   string `json:"topic"`
	Ack     string `json:"ack"`
	Message struct {
		Account string `json:"account"`
		Amount  string `json:"amount"`
		Hash    string `json:"hash"`
		Block   struct {
			Subtype       string `json:"subtype"`
			LinkAsAccount string `json:"link_as_account"`
		} `json:"block"`
	} `json:"message"`
}

// Watcher multiplexes account subscriptions onto one upstream connection.
type Watcher struct {
	URL    string
	mu     sync.Mutex
	subs   map[string]map[*connWriter]bool // account → connections
	conn   *websocket.Conn
	dirty  chan struct{}
	notify func(in *connWriter, n Notification)
}

// NewWatcher starts the upstream loop. notify delivers to a connection.
func NewWatcher(url string, notify func(in *connWriter, n Notification)) *Watcher {
	w := &Watcher{URL: url, subs: map[string]map[*connWriter]bool{}, dirty: make(chan struct{}, 1), notify: notify}
	go w.loop()
	return w
}

// Watch adds an account for a connection (at most 8 per connection).
func (w *Watcher) Watch(acct string, in *connWriter) {
	w.mu.Lock()
	n := 0
	for _, conns := range w.subs {
		if conns[in] {
			n++
		}
	}
	if n < 8 {
		if w.subs[acct] == nil {
			w.subs[acct] = map[*connWriter]bool{}
		}
		w.subs[acct][in] = true
	}
	w.mu.Unlock()
	select {
	case w.dirty <- struct{}{}:
	default:
	}
}

// Unwatch drops every account a connection was watching.
func (w *Watcher) Unwatch(in *connWriter) {
	w.mu.Lock()
	for acct, conns := range w.subs {
		delete(conns, in)
		if len(conns) == 0 {
			delete(w.subs, acct)
		}
	}
	w.mu.Unlock()
	select {
	case w.dirty <- struct{}{}:
	default:
	}
}

func (w *Watcher) accounts() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.subs))
	for a := range w.subs {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

func (w *Watcher) loop() {
	backoff := time.Second
	for {
		accts := w.accounts()
		if len(accts) == 0 {
			<-w.dirty // nothing to watch: no upstream connection at all
			continue
		}
		conn, err := websocket.Dial(w.URL, "", "https://sailnet.space")
		if err != nil {
			time.Sleep(backoff)
			if backoff < time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		sub := map[string]any{"action": "subscribe", "topic": "confirmation", "options": map[string]any{"accounts": accts}}
		if b, _ := json.Marshal(sub); websocket.Message.Send(conn, string(b)) != nil {
			conn.Close()
			continue
		}
		w.mu.Lock()
		w.conn = conn
		w.mu.Unlock()
		known := map[string]bool{}
		for _, a := range accts {
			known[a] = true
		}
		stop := make(chan struct{})
		go func() { // keep the subscription equal to the union of watched accounts
			t := time.NewTicker(25 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					websocket.Message.Send(conn, `{"action":"ping"}`)
				case <-w.dirty:
					cur := w.accounts()
					var add, del []string
					now := map[string]bool{}
					for _, a := range cur {
						now[a] = true
						if !known[a] {
							add = append(add, a)
						}
					}
					for a := range known {
						if !now[a] {
							del = append(del, a)
						}
					}
					known = now
					if len(add)+len(del) == 0 {
						continue
					}
					opts := map[string]any{}
					if len(add) > 0 {
						opts["accounts_add"] = add
					}
					if len(del) > 0 {
						opts["accounts_del"] = del
					}
					b, _ := json.Marshal(map[string]any{"action": "update", "topic": "confirmation", "options": opts})
					if websocket.Message.Send(conn, string(b)) != nil {
						conn.Close()
						return
					}
				}
			}
		}()
		for {
			var s string
			conn.SetReadDeadline(time.Now().Add(90 * time.Second))
			if err := websocket.Message.Receive(conn, &s); err != nil {
				break
			}
			var m upstreamMsg
			if json.Unmarshal([]byte(s), &m) != nil || m.Topic != "confirmation" {
				continue
			}
			n := Notification{Account: m.Message.Account, Amount: m.Message.Amount, Hash: m.Message.Hash, Subtype: m.Message.Block.Subtype, Link: m.Message.Block.LinkAsAccount}
			w.mu.Lock()
			targets := map[*connWriter]bool{}
			for in := range w.subs[n.Account] {
				targets[in] = true
			}
			for in := range w.subs[n.Link] {
				targets[in] = true
			}
			w.mu.Unlock()
			for in := range targets {
				w.notify(in, n)
			}
		}
		close(stop)
		conn.Close()
		w.mu.Lock()
		w.conn = nil
		w.mu.Unlock()
		log.Printf("confirmations: upstream dropped, reconnecting")
	}
}

// notifyCell delivers a confirmation to a client connection.
func notifyCell(in *connWriter, n Notification) {
	b, _ := json.Marshal(n)
	in.write(&wire.Cell{Cmd: wire.CmdNotify, Payload: b})
}

// WatchOver opens a connection to rel, asks it to watch account, and calls
// onNotify for each confirmation until stop is called. The client side of
// CmdWatch; used while waiting for funds.
func WatchOver(rel *RelayInfo, account string, timeout time.Duration, onNotify func(Notification)) (stop func(), err error) {
	conn, err := DialRelay(rel, timeout)
	if err != nil {
		return nil, err
	}
	w := newConnWriter(conn, true)
	if err := w.write(&wire.Cell{Cmd: wire.CmdWatch, Payload: []byte(account)}); err != nil {
		w.stop()
		conn.Close()
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		defer conn.Close()
		defer w.stop()
		r := bufio.NewReader(conn)
		for {
			conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
			cell, err := wire.ReadCell(r)
			if err != nil {
				return
			}
			if cell.Cmd == wire.CmdNotify {
				var n Notification
				if json.Unmarshal(cell.Payload, &n) == nil {
					onNotify(n)
				}
			}
			select {
			case <-done:
				return
			default:
			}
		}
	}()
	return func() { close(done); conn.Close() }, nil
}
