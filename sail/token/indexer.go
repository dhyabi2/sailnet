package token

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"

	"github.com/dhyabi2/sail/nano"
)

// Indexer replays Sailnet registry ops from the public Nano ledger.
//
// Relays announce themselves by sending 1-raw blocks to the treasury (anchor)
// account with a SAIL-encoded representative. Discovery therefore needs no
// operator list and no server anyone has to trust: the registry is whatever
// the public ledger says it is.
//
// Reading it is on the critical path of every client start, so it is done in
// a handful of batched calls rather than one round trip per relay. The
// treasury's own chain already names every send it ever received, and a
// block read gives that send's author, its position in their chain and the
// op in its representative field. So the whole registry is:
//
//	account_history(treasury)     — the receives, and the sends still pending
//	blocks_info(those receives)   — each one's source send hash
//	blocks_info(those sends)      — author, height, time and op, batched 100 at a time
//
// which is about six calls for the whole network instead of two per relay.
// Walking each relay's chain separately cost a minute of a user's time on a
// network of forty-five relays and would only have got worse as it grew.
type Indexer struct {
	Client   *nano.Client
	Treasury string

	// CacheFile, when set, remembers what has already been read. A block on
	// the Nano ledger is immutable, so a block read once never needs reading
	// again: a restart asks only about sends it has not seen. Losing or
	// corrupting the file costs one full read, nothing more.
	CacheFile string
}

// ledgerCache holds facts that cannot change: which send a receive pocketed,
// and what a send block says. Everything in it is public ledger data that is
// already in the relay list beside it.
type ledgerCache struct {
	Receives map[string]string     `json:"receives"` // receive hash → the send it pocketed
	Blocks   map[string]cachedSend `json:"blocks"`   // send hash → what that send says
}

type cachedSend struct {
	Account string `json:"a"`
	Height  string `json:"h"`
	Time    string `json:"t"`
	Rep     string `json:"r"`
}

func (ix *Indexer) loadCache() *ledgerCache {
	c := &ledgerCache{Receives: map[string]string{}, Blocks: map[string]cachedSend{}}
	if ix.CacheFile == "" {
		return c
	}
	data, err := os.ReadFile(ix.CacheFile)
	if err != nil {
		return c
	}
	var got ledgerCache
	if json.Unmarshal(data, &got) != nil {
		return c // unreadable: read the ledger afresh, and overwrite it below
	}
	if got.Receives != nil {
		c.Receives = got.Receives
	}
	if got.Blocks != nil {
		c.Blocks = got.Blocks
	}
	return c
}

func (ix *Indexer) saveCache(c *ledgerCache) {
	if ix.CacheFile == "" {
		return
	}
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	tmp := ix.CacheFile + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, ix.CacheFile) // best effort: a failed save costs one full read
	}
}

// NewIndexer creates an indexer rooted at the treasury/anchor account.
func NewIndexer(c *nano.Client, treasury string) *Indexer {
	return &Indexer{Client: c, Treasury: treasury}
}

// Run discovers relays and replays their registry ops.
func (ix *Indexer) Run(ctx context.Context) (*State, error) {
	hist, err := ix.Client.History(ctx, ix.Treasury)
	if err != nil {
		return nil, fmt.Errorf("history %s: %w", ix.Treasury, err)
	}

	// Every send to the treasury, whether it was pocketed or is still
	// waiting. A pocketed one is named by the link of the receive that took
	// it; some public RPCs drop that field from history, so the receives are
	// read back properly rather than trusted to carry it.
	var receives, sends []string
	senders := map[string]bool{}
	for _, b := range hist {
		switch {
		case b.Subtype == "receive" || b.Subtype == "open" || b.Type == "receive" || b.Type == "open":
			if b.Link != "" && b.Link != zeroHash {
				sends = append(sends, b.Link)
			} else if b.Hash != "" {
				receives = append(receives, b.Hash)
			}
			if b.Account != "" {
				senders[b.Account] = true
			}
		}
	}
	cache := ix.loadCache()
	dirty := false

	var askReceives []string
	for _, h := range receives {
		if send, ok := cache.Receives[h]; ok {
			sends = append(sends, send)
		} else {
			askReceives = append(askReceives, h)
		}
	}
	if len(askReceives) > 0 {
		infos, err := ix.Client.BlocksInfo(ctx, askReceives)
		if err != nil {
			return nil, fmt.Errorf("reading the treasury's receives: %w", err)
		}
		for h, bi := range infos {
			if l := bi.Contents.Link; l != "" && l != zeroHash {
				sends = append(sends, l)
				cache.Receives[h], dirty = l, true
			}
		}
	}
	if rs, err := ix.Client.Receivables(ctx, ix.Treasury, 1000); err == nil {
		for _, r := range rs {
			sends = append(sends, r.Hash)
			senders[r.Source] = true
		}
	}

	st := NewState(ix.Treasury)
	if len(sends) == 0 {
		return st, nil
	}
	var ask []string
	for _, h := range sends {
		if _, ok := cache.Blocks[h]; !ok {
			ask = append(ask, h)
		}
	}
	if len(ask) > 0 {
		infos, err := ix.Client.BlocksInfo(ctx, ask)
		if err != nil {
			return nil, fmt.Errorf("reading registry blocks: %w", err)
		}
		for h, bi := range infos {
			cache.Blocks[h] = cachedSend{bi.BlockAccount, bi.Height, bi.LocalTimestamp, bi.Contents.Representative}
			dirty = true
		}
	}
	if dirty {
		ix.saveCache(cache)
	}

	// One event per send that carries an op. Ordering matters only within a
	// single relay's chain (a DESCRIPTOR means nothing before its REGISTER),
	// so events are sorted by author and height — which also makes the
	// result the same on every client, whatever order the RPC answered in.
	events := make([]Event, 0, len(sends))
	for _, hash := range sends {
		bi, ok := cache.Blocks[hash]
		if !ok || bi.Account == "" || bi.Account == ix.Treasury {
			continue // not a send from a relay to the treasury
		}
		op, err := decodeRepAddr(bi.Rep)
		if err != nil {
			continue // an ordinary payment, not a registry op
		}
		h, _ := strconv.ParseUint(bi.Height, 10, 64)
		var ts int64
		fmt.Sscan(bi.Time, &ts)
		events = append(events, Event{
			Op: op, Sender: bi.Account, Recipient: ix.Treasury,
			SendHash: hash, SendHeight: h, Time: ts,
		})
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].Sender != events[j].Sender {
			return events[i].Sender < events[j].Sender
		}
		return events[i].SendHeight < events[j].SendHeight
	})
	for i := range events {
		_ = st.Apply(&events[i])
	}

	// A registry that came back empty while the treasury plainly has senders
	// means this RPC provider did not give us what we asked for. Rather than
	// tell the user the network is empty, fall back to the slow walk that
	// asks each account directly.
	if len(st.Relays) == 0 && len(senders) > 0 {
		return ix.runPerAccount(ctx, senders)
	}
	return st, nil
}

const zeroHash = "0000000000000000000000000000000000000000000000000000000000000000"

// runPerAccount is the original walk: one history call per sender. It is
// slow, and it is the fallback for an RPC provider whose blocks_info does
// not return what the batched path needs.
func (ix *Indexer) runPerAccount(ctx context.Context, senders map[string]bool) (*State, error) {
	st := NewState(ix.Treasury)
	accounts := make([]string, 0, len(senders))
	for a := range senders {
		accounts = append(accounts, a)
	}
	sort.Strings(accounts)
	for _, acct := range accounts {
		chain, err := ix.Client.History(ctx, acct)
		if err != nil {
			continue
		}
		var hashes []string
		for _, b := range chain {
			if b.Subtype == "send" && b.Account == ix.Treasury {
				hashes = append(hashes, b.Hash)
			}
		}
		if len(hashes) == 0 {
			continue
		}
		infos, err := ix.Client.BlocksInfo(ctx, hashes)
		if err != nil {
			continue
		}
		for _, b := range chain { // oldest first
			if b.Subtype != "send" || b.Account != ix.Treasury {
				continue
			}
			bi, ok := infos[b.Hash]
			if !ok {
				continue
			}
			op, err := decodeRepAddr(bi.Contents.Representative)
			if err != nil {
				continue
			}
			h, _ := strconv.ParseUint(b.Height, 10, 64)
			var ts int64
			fmt.Sscan(b.LocalTimestamp, &ts)
			_ = st.Apply(&Event{Op: op, Sender: acct, Recipient: b.Account, SendHash: b.Hash, SendHeight: h, Time: ts})
		}
	}
	return st, nil
}

func decodeRepAddr(addr string) (Op, error) {
	pk, err := nano.AddressToPubkey(addr)
	if err != nil {
		return Op{}, err
	}
	return Decode(pk)
}
