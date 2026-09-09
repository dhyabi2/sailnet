package client

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/dhyabi2/sail/token"
)

// The cost ledger: what this wallet paid, to which relay, and what it got.
//
// Pay-per-use only feels fair when the meter is visible. This records every
// anchor the client buys (relay, XNO, the price at the time, the bytes that
// bought) and, as circuits run, how much of each anchor was used. It lives
// on the device only — never gossiped, never uploaded — and says nothing
// the wallet's own ledger history does not already say, just in one place
// and in bytes as well as XNO.

type costEntry struct {
	At     int64  `json:"at"` // unix seconds, when the anchor was paid
	Relay  string `json:"relay"`
	Tag    string `json:"tag"`  // payment tag (block hash), upper hex
	Raw    string `json:"raw"`  // XNO paid, raw units, decimal
	Rate   string `json:"rate"` // raw XNO per MiB at the time, decimal
	Bought int64  `json:"bought"`
	Used   int64  `json:"used"`
}

type costLedger struct {
	mu      sync.Mutex
	path    string
	Entries []costEntry `json:"entries"`
}

func loadCosts(path string) *costLedger {
	l := &costLedger{path: path}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, l)
	}
	return l
}

func (l *costLedger) saveLocked() {
	if l.path == "" {
		return
	}
	b, _ := json.Marshal(l)
	os.WriteFile(l.path, b, 0o600)
}

// paid records an anchor bought at relay for amount (raw) at rateRaw per MiB.
func (l *costLedger) paid(relay, tag string, amount, rateRaw *big.Int, bought int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Entries = append(l.Entries, costEntry{At: time.Now().Unix(), Relay: relay, Tag: tag, Raw: amount.String(), Rate: rateRaw.String(), Bought: bought})
	if len(l.Entries) > 2000 { // a year of daily use; older lines are gone, not archived
		l.Entries = l.Entries[len(l.Entries)-2000:]
	}
	l.saveLocked()
}

// used records that remaining bytes are left of the anchor with this tag.
// Usage only grows: a stale reading never lowers it.
func (l *costLedger) used(tag string, remaining int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.Entries) - 1; i >= 0; i-- {
		e := &l.Entries[i]
		if e.Tag != tag {
			continue
		}
		if u := e.Bought - remaining; u > e.Used && u >= 0 {
			e.Used = u
			l.saveLocked()
		}
		return
	}
}

// Summary is what the app and `sailnode costs` show.
type CostSummary struct {
	Days       int     `json:"days"`
	Anchors    int     `json:"anchors"`
	XNO        string  `json:"xno"` // paid in the window
	BoughtMiB  float64 `json:"boughtMiB"`
	UsedMiB    float64 `json:"usedMiB"`
	AvgXNOMiB  string  `json:"avgXnoPerMiB"` // paid ÷ bought
	BestRelay  string  `json:"bestRelay,omitempty"`
	BestRate   string  `json:"bestRate,omitempty"`
	WorstRelay string  `json:"worstRelay,omitempty"`
	WorstRate  string  `json:"worstRate,omitempty"`
}

func (l *costLedger) summary(window time.Duration) CostSummary {
	l.mu.Lock()
	defer l.mu.Unlock()
	since := time.Now().Add(-window).Unix()
	s := CostSummary{Days: int(window.Hours() / 24)}
	paid := new(big.Int)
	var bought, used int64
	var best, worst *big.Int
	for _, e := range l.Entries {
		if e.At < since {
			continue
		}
		amt, ok := new(big.Int).SetString(e.Raw, 10)
		if !ok {
			continue
		}
		rate, ok := new(big.Int).SetString(e.Rate, 10)
		if !ok {
			continue
		}
		s.Anchors++
		paid.Add(paid, amt)
		bought += e.Bought
		used += e.Used
		if best == nil || rate.Cmp(best) < 0 {
			best, s.BestRelay = rate, e.Relay
		}
		if worst == nil || rate.Cmp(worst) > 0 {
			worst, s.WorstRelay = rate, e.Relay
		}
	}
	s.XNO = token.FormatXNO(paid)
	s.BoughtMiB = float64(bought) / (1 << 20)
	s.UsedMiB = float64(used) / (1 << 20)
	if bought > 0 {
		avg := new(big.Int).Div(new(big.Int).Mul(paid, big.NewInt(1<<20)), big.NewInt(bought))
		s.AvgXNOMiB = token.FormatXNO(avg)
	} else {
		s.AvgXNOMiB = "0"
	}
	if best != nil {
		s.BestRate, s.WorstRate = token.FormatXNO(best), token.FormatXNO(worst)
	}
	return s
}

// entries returns a copy of the ledger, newest first.
func (l *costLedger) entries() []costEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := append([]costEntry(nil), l.Entries...)
	sort.Slice(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out
}

func (m *manager) costs() *costLedger {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.costLedger == nil {
		m.costLedger = loadCosts(filepath.Join(dataDir(), "costs.json"))
	}
	return m.costLedger
}

// sampleUsage asks the circuit how much of its anchor is left and records it.
func (m *manager) sampleUsage(c interface {
	QueryQuota(time.Duration) (int64, error)
}, tag [32]byte) {
	if tag == ([32]byte{}) {
		return
	}
	if q, err := c.QueryQuota(6 * time.Second); err == nil {
		m.costs().used(hexUpper(tag[:]), q)
	}
}

func hexUpper(b []byte) string {
	const digits = "0123456789ABCDEF"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2], out[i*2+1] = digits[c>>4], digits[c&15]
	}
	return string(out)
}

var _ = hex.EncodeToString

// RunCosts prints the ledger: `sailnode costs [-days 7] [-all]`.
func RunCosts(args []string) {
	days, all := 7, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-days", "--days":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &days)
				i++
			}
		case "-all", "--all":
			all = true
		}
	}
	l := loadCosts(filepath.Join(dataDir(), "costs.json"))
	s := l.summary(time.Duration(days) * 24 * time.Hour)
	fmt.Printf("last %d days: %d anchors, %s XNO paid, %.1f MiB bought, %.1f MiB used, avg %s XNO/MiB\n", s.Days, s.Anchors, s.XNO, s.BoughtMiB, s.UsedMiB, s.AvgXNOMiB)
	if s.BestRelay != "" {
		fmt.Printf("cheapest %s at %s XNO/MiB; dearest %s at %s XNO/MiB\n", short(s.BestRelay), s.BestRate, short(s.WorstRelay), s.WorstRate)
	}
	if !all {
		return
	}
	fmt.Println()
	for _, e := range l.entries() {
		amt, _ := new(big.Int).SetString(e.Raw, 10)
		rate, _ := new(big.Int).SetString(e.Rate, 10)
		fmt.Printf("%s  %-14s  %10s XNO  %8s XNO/MiB  bought %6.1f MiB  used %6.1f MiB  tag %s\n",
			time.Unix(e.At, 0).Format("2006-01-02 15:04"), short(e.Relay), token.FormatXNO(amt), token.FormatXNO(rate), float64(e.Bought)/(1<<20), float64(e.Used)/(1<<20), e.Tag[:8])
	}
}
