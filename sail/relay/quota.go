package relay

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"

	"golang.org/x/crypto/blake2b"
)

// Quota tracks prepaid bytes per payment tag. A tag is the hash of the XNO
// send that paid for it (or a pool tag between relays). Each tag has an
// owner: the account that signed the payment block. Only that key can open
// circuits on the tag, so a tag read off the public ledger is worthless to
// anyone else. On disk the WAL keys tags by blake2b(tag): a seized relay
// yields no list of payments. Consumption is written once per MiB.
type Quota struct {
	mu       sync.Mutex
	credit   map[string]int64
	consumed map[string]int64
	owner    map[string]string // hashed tag → owner public key (hex), "" for legacy/static tags
	pending  map[string]int64  // consumed bytes not yet written to the WAL
	wal      *os.File
	MinRate  *big.Int // raw per MiB this relay charges
}

// NewQuota loads state from walPath ("" = memory only).
func NewQuota(walPath string, minRate *big.Int) (*Quota, error) {
	q := &Quota{credit: map[string]int64{}, consumed: map[string]int64{}, owner: map[string]string{}, pending: map[string]int64{}, MinRate: minRate}
	if walPath == "" {
		return q, nil
	}
	f, err := os.OpenFile(walPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		var n int64
		fmt.Sscanf(fields[2], "%d", &n)
		switch fields[0] {
		case "credit":
			q.credit[fields[1]] += n
			if len(fields) >= 4 {
				q.owner[fields[1]] = fields[3]
			}
		case "consume":
			q.consumed[fields[1]] += n
		}
	}
	q.wal = f
	return q, nil
}

// key hashes a tag: the WAL never holds a payment hash in the clear.
func key(tag string) string {
	h := blake2b.Sum256([]byte("sailnet-quota" + strings.ToUpper(tag)))
	return hex.EncodeToString(h[:16])
}

func (q *Quota) logLine(kind, k string, n int64, owner string) {
	if q.wal == nil {
		return
	}
	if owner != "" {
		fmt.Fprintf(q.wal, "%s %s %d %s\n", kind, k, n, owner)
	} else {
		fmt.Fprintf(q.wal, "%s %s %d\n", kind, k, n)
	}
}

// Credit adds bytes for a tag once and records its owner (hex public key of
// the account that signed the payment; "" for static test tags).
func (q *Quota) Credit(tag string, bytes int64, owner string) bool {
	k := key(tag)
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, seen := q.credit[k]; seen {
		return false
	}
	q.credit[k] = bytes
	q.owner[k] = owner
	q.logLine("credit", k, bytes, owner)
	return true
}

// Add credits more bytes to an existing tag (a top-up) and returns the
// remaining quota. Unknown tags are not created here: a top-up must land on
// a circuit that was paid for in the first place.
func (q *Quota) Add(tag string, bytes int64) (int64, bool) {
	k := key(tag)
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, seen := q.credit[k]; !seen {
		return 0, false
	}
	q.credit[k] += bytes
	q.logLine("credit", k, bytes, q.owner[k])
	return q.credit[k] - q.consumed[k], true
}

// Total is the bytes ever credited to a tag.
func (q *Quota) Total(tag string) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.credit[key(tag)]
}

// Known reports whether a tag has ever been credited.
func (q *Quota) Known(tag string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.credit[key(tag)]
	return ok
}

// Owner returns the hex public key that may use a tag ("" if unrestricted).
func (q *Quota) Owner(tag string) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.owner[key(tag)]
}

// Consume charges bytes; returns remaining (negative = exhausted). The WAL
// gets one line per MiB consumed, not one per cell.
func (q *Quota) Consume(tag string, n int64) int64 {
	k := key(tag)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.consumed[k] += n
	if n > 0 {
		q.pending[k] += n
		if q.pending[k] >= 1<<20 {
			q.logLine("consume", k, q.pending[k], "")
			q.pending[k] = 0
		}
	}
	return q.credit[k] - q.consumed[k]
}

// Remaining returns unconsumed bytes for a tag.
// SetMinRate changes the price (raw per MiB) used for new credits.
func (q *Quota) SetMinRate(raw *big.Int) {
	q.mu.Lock()
	q.MinRate = raw
	q.mu.Unlock()
}

func (q *Quota) Remaining(tag string) int64 {
	k := key(tag)
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.credit[k] - q.consumed[k]
}

// Flush writes pending consumption to the WAL (call on shutdown or a timer).
func (q *Quota) Flush() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for k, n := range q.pending {
		if n > 0 {
			q.logLine("consume", k, n, "")
		}
		delete(q.pending, k)
	}
}

// BytesFor converts an XNO amount (raw) at a rate (raw per MiB) to bytes.
func BytesFor(raw *big.Int, ratePerMiB *big.Int) int64 {
	if raw == nil || ratePerMiB == nil || ratePerMiB.Sign() <= 0 {
		return 0
	}
	b := new(big.Int).Mul(raw, big.NewInt(1<<20))
	b.Quo(b, ratePerMiB)
	if !b.IsInt64() {
		return 1 << 62
	}
	return b.Int64()
}
