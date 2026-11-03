package relay

import (
	"math/big"
	"sort"
	"time"

	"github.com/dhyabi2/sail/token"
)

// NextHopCapMultiple is how far over the market a next hop may price itself
// before this relay stops prepaying it: five times the live median, the same
// cap clients use when they choose a path (client.priceCap).
const NextHopCapMultiple = 5

// nextHopCap is the highest price, in REGISTER rate units, this relay will
// prepay a pool at. MaxNextRate overrides it; otherwise it is five times the
// median price of peers proven to be alive, and 0 (no cap) when there is no
// market to measure.
//
// Why a relay needs its own cap: a pool top-up is sized at the peer's price
// (poolRaw), so a peer that registers at forty times the going rate is paid
// forty times as much for the same bytes, by a relay that earns the going rate
// for them. On 2026-10-28 three such relays emptied every wallet that prepaid
// them, and their operators' payouts stopped.
func (s *Server) nextHopCap() uint32 {
	if s.MaxNextRate > 0 {
		return s.MaxNextRate
	}
	if s.Registry == nil {
		return 0
	}
	var all, live []int
	for _, rel := range s.Registry.All() {
		if rel.MinRate == 0 || (s.Key != nil && rel.Account == s.Key.Address) {
			continue
		}
		all = append(all, int(rel.MinRate))
		if time.Since(s.Registry.LastSeen(rel.Account)) < 3*time.Hour || s.reachedWithin(rel.Account, 3*time.Hour) {
			live = append(live, int(rel.MinRate))
		}
	}
	rates := live
	if len(rates) < 4 { // hops + 1, as path selection and Float do
		rates = all
	}
	if len(rates) == 0 {
		return 0
	}
	sort.Ints(rates)
	c := uint64(rates[len(rates)/2]) * NextHopCapMultiple
	if c > 1<<32-1 {
		c = 1<<32 - 1
	}
	return uint32(c)
}

// overPriced reports whether rel asks more than this relay will prepay.
func (s *Server) overPriced(rel *RelayInfo) (bool, uint32) {
	if rel == nil {
		return false, 0
	}
	c := s.nextHopCap()
	return c > 0 && rel.MinRate > c, c
}

// capTopUp limits one top-up to half of what the relay can spend, but never
// below one base pool: a single peer can no longer take the whole wallet in
// one refill, and a peer carrying our traffic is still always paid a pool.
func capTopUp(amount, base, avail *big.Int) *big.Int {
	half := new(big.Int).Rsh(avail, 1)
	limit := half
	if base != nil && base.Cmp(limit) > 0 {
		limit = base
	}
	if amount.Cmp(limit) > 0 {
		amount = new(big.Int).Set(limit)
	}
	if amount.Cmp(avail) > 0 {
		amount = new(big.Int).Set(avail)
	}
	return amount
}

func rateXNO(rate uint32) string { return formatXNO(token.RateToRaw(rate)) }
