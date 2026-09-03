package relay

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	mathrand "math/rand"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dhyabi2/sail/nano"
	"github.com/dhyabi2/sail/token"
)

// Reward epochs.
//
// Every relay owes a levy of LevyPercent of what it earned in an epoch (one
// UTC day) to the other relays. There is no pool and no payer of last resort:
// each relay computes the same table from the public Nano ledger and sends
// its owed shares directly, as sends whose representative encodes
// OpReward(epoch), so anyone can replay and verify. The ledger is the only
// measurement; every node computes, pays and enforces.
//
//   - SeniorityShare of the levy goes to relays in proportion to their age,
//     where age is the number of consecutive epochs (up to AgeCap) in which
//     they actually earned. Registering early and idling earns nothing.
//   - The rest goes in proportion to performance: the sum over payers of the
//     square root of what each payer paid. A hundred small clients count far
//     more than one wallet paying itself.
//   - Washing volume through your own relay costs LevyPercent and returns at
//     most the performance share of that, so self-dealing loses money.
//   - A relay that does not pay (Paid < LevyOwed·MinPaidPercent) in the epoch
//     after earning is excluded from path selection by every client and relay.

const (
	LevyPercent    = 10
	SeniorityShare = 60 // of the levy; the rest is performance
	AgeCap         = 180
	EpochSeconds   = 86400
	MinPaidPercent = 95 // tolerance for rounding and RPC timestamp skew at epoch edges
	// Anti-manipulation parameters:
	// an epoch counts toward age only with real earnings from several payers;
	// tiny payers do not count toward performance and large ones are capped;
	// and no relay can receive more than RewardCapPercent of its own levy, so
	// moving money through your own relays always loses money.
	MinEpochEarnXNO  = 0.01  // XNO earned in an epoch for it to count toward age
	MinPayers        = 3     // distinct payers above MinPayerXNO for an epoch to count
	MinPayerXNO      = 0.001 // payers below this are ignored for age and performance
	PayerCapXNO      = 0.1   // performance credit per payer is capped at sqrt(this)
	RewardCapPercent = 80    // a relay receives at most this share of its own levy
)

// EpochRelay is one relay's row in an epoch table.
type EpochRelay struct {
	Account  string
	Earned   *big.Int            // raw received from others in the epoch (levy payouts excluded)
	Payers   map[string]*big.Int // source account → raw
	Age      int                 // consecutive earning epochs ending with this one (capped)
	Perf     float64             // Σ sqrt(XNO from each payer)
	LevyOwed *big.Int            // Earned × LevyPercent / 100
	Paid     *big.Int            // raw sent with OpReward(epoch) in the following epoch, matched per recipient
	PaidTo   map[string]*big.Int // recipient → raw sent with OpReward(epoch)
	Eligible bool                // this epoch counts toward age
}

// EpochTable is the deterministic result for one epoch.
type EpochTable struct {
	Epoch   uint32
	Relays  map[string]*EpochRelay
	Payouts map[string]map[string]*big.Int // from → to → raw owed
}

// EpochOf returns the epoch number for a unix time.
func EpochOf(unix int64) uint32 { return uint32(unix / EpochSeconds) }

// CurrentEpoch is today's epoch (UTC).
func CurrentEpoch() uint32 { return EpochOf(time.Now().Unix()) }

// RewardOp encodes the representative for a levy payout of epoch e.
func RewardOp(e uint32) [32]byte {
	var aux [12]byte
	binary.BigEndian.PutUint32(aux[:4], e)
	rep, _ := token.Encode(token.Op{Code: token.OpReward, Aux: aux})
	return rep
}

// rewardEpoch decodes an OpReward representative; ok=false if it is not one.
func rewardEpoch(rep string) (uint32, bool) {
	pub, err := nano.AddressToPubkey(rep)
	if err != nil {
		return 0, false
	}
	op, err := token.Decode(pub)
	if err != nil || op.Code != token.OpReward {
		return 0, false
	}
	return binary.BigEndian.Uint32(op.Aux[:4]), true
}

// ComputeEpoch builds the table for epoch e from the ledger: relays from the
// registry, their receives bucketed by epoch, their reward-tagged sends in
// epoch e+1. Needs one history call per relay plus blocks_info batches.
func ComputeEpoch(ctx context.Context, nc *nano.Client, treasury string, e uint32) (*EpochTable, error) {
	st, err := token.NewIndexer(nc, treasury).Run(ctx)
	if err != nil {
		return nil, err
	}
	t := &EpochTable{Epoch: e, Relays: map[string]*EpochRelay{}, Payouts: map[string]map[string]*big.Int{}}
	isRelay := map[string]bool{}
	for acct := range st.Relays {
		isRelay[acct] = true
	}
	for acct := range st.Relays {
		row := &EpochRelay{Account: acct, Earned: new(big.Int), Payers: map[string]*big.Int{}, LevyOwed: new(big.Int), Paid: new(big.Int), PaidTo: map[string]*big.Int{}}
		hist, err := nc.History(ctx, acct)
		if err != nil {
			log.Printf("rewards: history %s: %v (relay skipped this epoch)", short(acct), err)
			continue // one relay's unreadable history must not stop the whole table
		}
		// Which receives are levy payouts? Their source sends carry OpReward.
		var recvHashes []string
		for _, b := range hist {
			be := EpochOf(ts(b))
			if (b.Subtype == "receive" || b.Subtype == "open") && b.Link != "" && be+AgeCap >= e && be <= e+1 {
				recvHashes = append(recvHashes, b.Link) // only the window that can matter: bounded work per relay
			}
		}
		var sendHashes []string
		for _, b := range hist {
			if b.Subtype == "send" && (EpochOf(ts(b)) == e+1 || EpochOf(ts(b)) == e+2) {
				sendHashes = append(sendHashes, b.Hash)
			}
		}
		infos := map[string]nano.BlockInfo{}
		for _, batch := range [][]string{recvHashes, sendHashes} {
			for i := 0; i < len(batch); i += 200 {
				j := i + 200
				if j > len(batch) {
					j = len(batch)
				}
				bi, err := nc.BlocksInfo(ctx, batch[i:j])
				if err != nil {
					return nil, err // transient RPC failure: retry the whole table later
				}
				for k, v := range bi {
					infos[k] = v
				}
			}
		}
		payersBy := map[uint32]map[string]*big.Int{} // epoch → payer → raw (confirmed, non-levy, non-self)
		for _, b := range hist {
			switch b.Subtype {
			case "receive", "open":
				src, ok := infos[b.Link]
				if !ok || src.Confirmed != "true" {
					continue // outside the window, or not confirmed: does not count
				}
				if _, isReward := rewardEpoch(src.Contents.Representative); isReward {
					continue // levy income is not earnings
				}
				if b.Account == acct {
					continue
				}
				amt, ok := new(big.Int).SetString(b.Amount, 10)
				if !ok {
					continue
				}
				be := EpochOf(ts(b))
				if payersBy[be] == nil {
					payersBy[be] = map[string]*big.Int{}
				}
				if payersBy[be][b.Account] == nil {
					payersBy[be][b.Account] = new(big.Int)
				}
				payersBy[be][b.Account].Add(payersBy[be][b.Account], amt)
			case "send":
				be := EpochOf(ts(b))
				if be != e+1 && be != e+2 {
					continue
				}
				if be == e+2 && ts(b)%EpochSeconds > 2*3600 {
					continue // a two-hour grace after the payout epoch absorbs timestamp skew
				}
				if pe, ok := rewardEpoch(infos[b.Hash].Contents.Representative); ok && pe == e {
					if amt, ok := new(big.Int).SetString(b.Amount, 10); ok && b.Account != acct {
						if row.PaidTo[b.Account] == nil {
							row.PaidTo[b.Account] = new(big.Int)
						}
						row.PaidTo[b.Account].Add(row.PaidTo[b.Account], amt)
					}
				}
			}
		}
		row.Payers = payersBy[e]
		if row.Payers == nil {
			row.Payers = map[string]*big.Int{}
		}
		for _, v := range row.Payers {
			row.Earned.Add(row.Earned, v)
		}
		eligible := func(ep uint32) bool { // real service: enough income from enough distinct payers
			ps := payersBy[ep]
			total, n := 0.0, 0
			for _, v := range ps {
				x := xno(v)
				if x >= MinPayerXNO {
					n++
				}
				total += x
			}
			return total >= MinEpochEarnXNO && n >= MinPayers
		}
		row.Eligible = eligible(e)
		for a := e; row.Age < AgeCap && eligible(a); a-- {
			row.Age++
			if a == 0 {
				break
			}
		}
		for _, v := range row.Payers {
			if x := xno(v); x >= MinPayerXNO {
				row.Perf += math.Min(math.Sqrt(x), math.Sqrt(PayerCapXNO))
			}
		}
		row.LevyOwed.Mul(row.Earned, big.NewInt(LevyPercent))
		row.LevyOwed.Div(row.LevyOwed, big.NewInt(100))
		t.Relays[acct] = row
	}
	t.computePayouts()
	return t, nil
}

// computePayouts fills Payouts: each relay's levy split among the others by
// age and performance, then capped so that no relay receives more than
// RewardCapPercent of its own levy (the excess is redistributed among relays
// still under their cap, then the rest is simply kept by the payer). With
// the cap, money an attacker moves through its own relays can never come
// back in full: every XNO of wash traffic loses at least 20 % of its levy.
func (t *EpochTable) computePayouts() {
	t.computeShares()
	// cap: reward(R) ≤ RewardCapPercent% of Levy(R)
	for iter := 0; iter < 5; iter++ {
		over := false
		for to, r := range t.Relays {
			capRaw := new(big.Int).Mul(r.LevyOwed, big.NewInt(RewardCapPercent))
			capRaw.Div(capRaw, big.NewInt(100))
			got := new(big.Int)
			for _, m := range t.Payouts {
				if v := m[to]; v != nil {
					got.Add(got, v)
				}
			}
			if got.Cmp(capRaw) <= 0 {
				continue
			}
			over = true
			// scale this recipient's share down proportionally in every payer's table
			for _, m := range t.Payouts {
				if v := m[to]; v != nil {
					nv := new(big.Int).Mul(v, capRaw)
					nv.Div(nv, got)
					m[to] = nv
				}
			}
		}
		if !over {
			break
		}
	}
	for from, m := range t.Payouts {
		for to, v := range m {
			if v.Cmp(dust) < 0 {
				delete(m, to)
			}
		}
		if len(m) == 0 {
			delete(t.Payouts, from)
		}
	}
	// Paid: per-recipient match against what is owed, so a levy send to an
	// unrelated wallet (or to oneself) counts for nothing.
	for from, f := range t.Relays {
		f.Paid = new(big.Int)
		for to, owed := range t.Payouts[from] {
			if sent := f.PaidTo[to]; sent != nil {
				if sent.Cmp(owed) < 0 {
					f.Paid.Add(f.Paid, sent)
				} else {
					f.Paid.Add(f.Paid, owed)
				}
			}
		}
	}
}

// computeShares is the uncapped split of every relay's levy.
func (t *EpochTable) computeShares() {
	var accts []string
	for a := range t.Relays {
		accts = append(accts, a)
	}
	sort.Strings(accts)
	for _, from := range accts {
		f := t.Relays[from]
		if f.LevyOwed.Sign() == 0 {
			continue
		}
		var ageSum, perfSum float64
		for _, to := range accts {
			if to == from {
				continue
			}
			r := t.Relays[to]
			ageSum += float64(r.Age)
			perfSum += r.Perf
		}
		levy := new(big.Float).SetInt(f.LevyOwed)
		out := map[string]*big.Int{}
		for _, to := range accts {
			if to == from {
				continue
			}
			r := t.Relays[to]
			share := 0.0
			if ageSum > 0 {
				share += float64(SeniorityShare) / 100 * float64(r.Age) / ageSum
			}
			if perfSum > 0 {
				share += float64(100-SeniorityShare) / 100 * r.Perf / perfSum
			}
			if share <= 0 {
				continue
			}
			amt, _ := new(big.Float).Mul(levy, big.NewFloat(share)).Int(nil)
			if amt.Cmp(dust) < 0 {
				continue
			}
			out[to] = amt
		}
		if len(out) > 0 {
			t.Payouts[from] = out
		}
	}
}

// dust: payouts below 0.000001 XNO are not sent.
var dust = func() *big.Int { d, _ := new(big.Int).SetString("1000000000000000000000000", 10); return d }()

// Compliant reports whether relay acct paid its levy for this epoch.
func (t *EpochTable) Compliant(acct string) bool {
	r := t.Relays[acct]
	if r == nil {
		return true // unknown relays are judged by other means
	}
	owed := new(big.Int)
	for _, v := range t.Payouts[acct] {
		owed.Add(owed, v)
	}
	if owed.Cmp(dust) < 0 {
		return true
	}
	need := new(big.Int).Mul(owed, big.NewInt(MinPaidPercent))
	need.Div(need, big.NewInt(100))
	return r.Paid.Cmp(need) >= 0
}

// String renders the table for inspection.
func (t *EpochTable) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "epoch %d (%s UTC)\n", t.Epoch, time.Unix(int64(t.Epoch)*EpochSeconds, 0).UTC().Format("2006-01-02"))
	var accts []string
	for a := range t.Relays {
		accts = append(accts, a)
	}
	sort.Strings(accts)
	for _, a := range accts {
		r := t.Relays[a]
		fmt.Fprintf(&sb, "%s  earned %.6f XNO from %d payer(s)  age %d%s  perf %.4f  levy %.6f  paid %.6f  %s\n", short(a), xno(r.Earned), len(r.Payers), r.Age, map[bool]string{true: "", false: " (epoch not eligible)"}[r.Eligible], r.Perf, xno(r.LevyOwed), xno(r.Paid), map[bool]string{true: "ok", false: "UNPAID"}[t.Compliant(a)])
		for to, amt := range t.Payouts[a] {
			fmt.Fprintf(&sb, "    → %s %.6f XNO\n", short(to), xno(amt))
		}
	}
	return sb.String()
}

func ts(b nano.HistoryBlock) int64 {
	v, _ := strconv.ParseInt(b.LocalTimestamp, 10, 64)
	return v
}

func xno(raw *big.Int) float64 {
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(raw), new(big.Float).SetInt(rawPerXNO)).Float64()
	return f
}

// PayLevy sends this relay's owed shares for epoch e, skipping what the
// ledger already shows as paid. Returns the number of sends made.
func PayLevy(ctx context.Context, nc *nano.Client, key *nano.Key, t *EpochTable) (int, error) {
	row := t.Relays[key.Address]
	if row == nil || row.LevyOwed.Sign() == 0 {
		return 0, nil
	}
	if t.Compliant(key.Address) {
		return 0, nil
	}
	acct := &nano.Account{Key: key, Client: nc}
	rep := RewardOp(t.Epoch)
	n := 0
	var tos []string
	for to := range t.Payouts[key.Address] {
		tos = append(tos, to)
	}
	sort.Strings(tos)
	for _, to := range tos {
		amt := new(big.Int).Set(t.Payouts[key.Address][to])
		if already := row.PaidTo[to]; already != nil {
			amt.Sub(amt, already)
		}
		if amt.Cmp(dust) < 0 {
			continue // this recipient is settled
		}
		if _, err := acct.Send(ctx, to, amt, &rep); err != nil {
			return n, fmt.Errorf("levy to %s: %w", short(to), err)
		}
		n++
	}
	return n, nil
}

// RunLevy is the relay's daily job: refresh compliance, then pay this relay's
// levy for the epoch that just closed. Idempotent: it pays only what the
// ledger does not yet show.
func (s *Server) RunLevy(pay bool) {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		if err := s.Registry.RefreshScores(ctx); err != nil {
			log.Printf("rewards: scores: %v", err)
		}
		if !pay {
			cancel()
			next := time.Unix(int64(CurrentEpoch()+1)*EpochSeconds, 0).Add(time.Duration(10+mathrand.Intn(300)) * time.Minute)
			time.Sleep(time.Until(next))
			continue
		}
		e := CurrentEpoch() - 1
		t, err := ComputeEpoch(ctx, s.Nano, s.Registry.Treasury, e)
		if err != nil {
			log.Printf("rewards: epoch %d: %v", e, err)
		} else if n, err := PayLevy(ctx, s.Nano, s.Key, t); err != nil {
			log.Printf("rewards: epoch %d: paid %d share(s), then: %v", e, n, err)
		} else if n > 0 {
			log.Printf("rewards: epoch %d: levy paid in %d share(s)", e, n)
		}
		cancel()
		// Next run: shortly after the next UTC day starts, plus a spread so relays do not all hit the RPC at once.
		next := time.Unix(int64(CurrentEpoch()+1)*EpochSeconds, 0).Add(time.Duration(10+mathrand.Intn(300)) * time.Minute) // random spread over five hours
		time.Sleep(time.Until(next))
	}
}

// Payout: an operator can have everything the node earns forwarded to an
// address of their choosing, keeping only an operating float on the node
// for pool prepayments and the levy. Sweeps are plain sends (no op in the
// representative), which the reward table ignores, so they change neither
// earnings nor compliance.

// SweepTo sends balance − keep to the payout address if that is above dust.
// It receives pending funds first. Returns the amount swept (nil if none).
func SweepTo(ctx context.Context, nc *nano.Client, key *nano.Key, to string, keep *big.Int) (*big.Int, error) {
	if _, err := nano.AddressToPubkey(to); err != nil {
		return nil, fmt.Errorf("payout address: %w", err)
	}
	if to == key.Address {
		return nil, errors.New("payout address is this node's own wallet")
	}
	acct := &nano.Account{Key: key, Client: nc}
	acct.ReceiveAll(ctx)
	info, ok, err := nc.AccountInfo(ctx, key.Address)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	bal, good := new(big.Int).SetString(info.Balance, 10)
	if !good {
		return nil, errors.New("bad balance from node")
	}
	amt := new(big.Int).Sub(bal, keep)
	if amt.Cmp(dust) <= 0 {
		return nil, nil
	}
	if _, err := acct.Send(ctx, to, amt, nil); err != nil {
		return nil, err
	}
	return amt, nil
}

// RunPayout sweeps every interval; safe to run alongside pools and the levy
// because the float is never touched.
func (s *Server) RunPayout(to string, keep *big.Int, every time.Duration) {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		amt, err := SweepTo(ctx, s.Nano, s.Key, to, keep)
		cancel()
		switch {
		case err != nil:
			log.Printf("payout: %v", err)
		case amt != nil:
			log.Printf("payout: %s XNO forwarded (float kept: %s XNO)", formatXNO(amt), formatXNO(keep))
		}
		time.Sleep(every)
	}
}
