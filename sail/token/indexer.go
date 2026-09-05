package token

import (
	"context"
	"fmt"
	"strconv"

	"github.com/dhyabi2/sail/nano"
)

// Indexer replays Sailnet registry ops from the public Nano ledger.
//
// Relays announce themselves by sending 1-raw blocks to the treasury (anchor)
// account with a SAIL-encoded representative. Discovery therefore needs no
// operator list: read the anchor account's incoming sends (pocketed or still
// receivable), then replay each sender's own chain in height order.
type Indexer struct {
	Client   *nano.Client
	Treasury string
}

// NewIndexer creates an indexer rooted at the treasury/anchor account.
func NewIndexer(c *nano.Client, treasury string) *Indexer {
	return &Indexer{Client: c, Treasury: treasury}
}

// Run discovers relays and replays their registry ops.
func (ix *Indexer) Run(ctx context.Context) (*State, error) {
	st := NewState(ix.Treasury)
	senders := map[string]bool{}

	hist, err := ix.Client.History(ctx, ix.Treasury)
	if err != nil {
		return nil, fmt.Errorf("history %s: %w", ix.Treasury, err)
	}
	for _, b := range hist {
		if (b.Subtype == "receive" || b.Subtype == "open") && b.Account != "" {
			senders[b.Account] = true // source account of the pocketed send
		}
	}
	if rs, err := ix.Client.Receivables(ctx, ix.Treasury, 1000); err == nil {
		for _, r := range rs {
			senders[r.Source] = true
		}
	}

	for acct := range senders {
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
		infos, err := ix.Client.BlocksInfo(ctx, hashes) // representatives are not in history
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
