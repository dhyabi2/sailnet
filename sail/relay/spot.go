package relay

import (
	"log"
	"math/big"
	"sync"
	"time"

	"github.com/dhyabi2/sail/token"
)

// Spot price: a relay sells idle capacity cheaper, for a short window, by
// signing an offer into its own gossip record.
//
// The operator chooses the discount and the load below which it applies
// (--spot-discount, --spot-below, --capacity-mbps). Nothing is decreed and
// nobody is trusted: the offer is the relay's own signature over its own
// price, so a client can hold it to that price and a relay cannot deny it.
// "I am idle" is the relay's private reason for its own price; no one
// verifies it and no one needs to (RULES.md 4 does not engage). A window
// once signed is honoured to its end even if load rises: the relay credits
// payments at the spot price while the window stands. It is the fast
// version of the repricing relays already do every ten days.

const (
	spotWindow = 30 * time.Minute
	spotRenew  = 10 * time.Minute // sign a fresh window when this much is left and still idle
)

type spotState struct {
	mu     sync.Mutex
	rate   uint32
	until  int64
	last   int64 // BytesRelayed at the previous sample
	primed bool  // a previous sample exists
	loads  []float64
}

// spotDecision is the rule: below the load threshold, offer the discounted
// price; otherwise no offer. Pure, so it is tested on its own.
func spotDecision(loadPct, below, discount int, published uint32) (uint32, bool) {
	if discount <= 0 || discount >= 100 || below <= 0 || published == 0 || loadPct >= below {
		return 0, false
	}
	rate := published * uint32(100-discount) / 100
	if rate == 0 || rate >= published {
		return 0, false
	}
	return rate, true
}

// currentSpot is the standing offer, if any.
func (s *Server) currentSpot(now time.Time) (uint32, int64) {
	s.spot.mu.Lock()
	defer s.spot.mu.Unlock()
	if s.spot.rate == 0 || now.Unix() >= s.spot.until {
		return 0, 0
	}
	return s.spot.rate, s.spot.until
}

// selfWithSpot is our own record with the standing offer attached.
func (s *Server) selfWithSpot() *RelayInfo {
	ri := *s.Self
	ri.SpotRate, ri.SpotUntil = s.currentSpot(time.Now())
	return &ri
}

// rateNow is the price a payment arriving now is credited at.
func (s *Server) rateNow() *big.Int {
	if rate, _ := s.currentSpot(time.Now()); rate > 0 {
		return token.RateToRaw(rate)
	}
	return s.Quota.MinRate
}

// RunSpot samples load every interval and keeps the offer current.
func (s *Server) RunSpot(every time.Duration) {
	for {
		s.sampleSpot(time.Now(), every)
		time.Sleep(every)
	}
}

func (s *Server) sampleSpot(now time.Time, every time.Duration) {
	if s.Self == nil || s.Capacity <= 0 || s.SpotDiscount <= 0 {
		return
	}
	cur := s.Metrics.BytesRelayed.Load()
	s.spot.mu.Lock()
	defer s.spot.mu.Unlock()
	if s.spot.primed {
		load := float64(cur-s.spot.last) / (every.Seconds() * float64(s.Capacity))
		s.spot.loads = append(s.spot.loads, load)
		if len(s.spot.loads) > 5 {
			s.spot.loads = s.spot.loads[len(s.spot.loads)-5:]
		}
	}
	s.spot.last, s.spot.primed = cur, true
	if len(s.spot.loads) == 0 {
		return
	}
	var sum float64
	for _, l := range s.spot.loads {
		sum += l
	}
	loadPct := int(100 * sum / float64(len(s.spot.loads)))
	standing := s.spot.rate > 0 && now.Unix() < s.spot.until
	if standing && s.spot.until-now.Unix() > int64(spotRenew.Seconds()) {
		return // a window once signed is kept to its end
	}
	rate, ok := spotDecision(loadPct, s.SpotBelow, s.SpotDiscount, s.Self.MinRate)
	if !ok {
		if standing {
			return // let it run out
		}
		if s.spot.rate != 0 {
			log.Printf("spot: load %d%% — no offer", loadPct)
		}
		s.spot.rate, s.spot.until = 0, 0
		return
	}
	s.spot.rate, s.spot.until = rate, now.Add(spotWindow).Unix()
	log.Printf("spot: load %d%% below %d%% — offering %s XNO/MiB (published %s) until %s", loadPct, s.SpotBelow, token.FormatXNO(token.RateToRaw(rate)), token.FormatXNO(token.RateToRaw(s.Self.MinRate)), time.Unix(s.spot.until, 0).Format("15:04"))
}
