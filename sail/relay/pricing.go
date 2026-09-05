package relay

import (
	"encoding/json"
	"os"
	"time"
)

// Pricing adjusts a relay's price to demand, once per window (ten days by
// default), from the bytes it relayed:
//
//   - usage fell by more than a fifth against the previous window: price
//     down 10 percent, so the relay stays the cheapest choice for users;
//   - usage grew by more than a fifth: price up 3 percent, a small step;
//   - otherwise unchanged.
//
// The price never leaves [Min, Max]. Nothing here can fail loudly: a
// missing or corrupt state file starts a fresh window, and a write error is
// ignored (the next window recomputes from live counters). Now is
// injectable so the ten-day path is tested in seconds.
type Pricing struct {
	File string
	Days int
	Min  uint32
	Max  uint32
	Now  func() time.Time

	st pricingState
}

type pricingState struct {
	Rate        uint32    `json:"rate"`
	Start       uint32    `json:"start"` // the price the operator configured; a change to it resets the price
	WindowStart time.Time `json:"windowStart"`
	StartBytes  int64     `json:"startBytes"`
	PrevBytes   int64     `json:"prevBytes"` // relayed in the previous full window
	Windows     int       `json:"windows"`   // completed windows so far
}

// Load restores the saved state or starts a window now at rate.
func (p *Pricing) Load(rate uint32, bytesNow int64) uint32 {
	if p.Now == nil {
		p.Now = time.Now
	}
	if p.Days <= 0 {
		p.Days = 10
	}
	if p.File != "" {
		if data, err := os.ReadFile(p.File); err == nil {
			var st pricingState
			if json.Unmarshal(data, &st) == nil && st.Rate > 0 && !st.WindowStart.IsZero() && st.Start == rate {
				p.st = st
				// Counters restart with the process: measure from here, and
				// let the current window run its remaining time.
				p.st.StartBytes = bytesNow
				return p.clamp(p.st.Rate)
			}
			// st.Start != rate: the operator changed the configured price.
			// Their choice wins over anything demand did to the old one, and
			// a fresh window starts from it.
		}
	}
	p.st = pricingState{Rate: p.clamp(rate), Start: rate, WindowStart: p.Now(), StartBytes: bytesNow}
	p.save()
	return p.st.Rate
}

// Tick is called periodically with the relay's total relayed bytes. It
// returns the rate to charge and whether it just changed.
func (p *Pricing) Tick(bytesNow int64) (rate uint32, changed bool) {
	if p.Now == nil {
		p.Now = time.Now
	}
	if p.st.WindowStart.IsZero() {
		return p.Load(p.st.Rate, bytesNow), false
	}
	if p.Now().Sub(p.st.WindowStart) < time.Duration(p.Days)*24*time.Hour {
		return p.st.Rate, false
	}
	cur := bytesNow - p.st.StartBytes
	if cur < 0 {
		cur = 0
	}
	old := p.st.Rate
	if p.st.Windows > 0 && p.st.PrevBytes > 0 {
		switch {
		case float64(cur) < 0.8*float64(p.st.PrevBytes):
			p.st.Rate = p.clamp(uint32(float64(old) * 0.9))
		case float64(cur) > 1.2*float64(p.st.PrevBytes):
			p.st.Rate = p.clamp(uint32(float64(old)*1.03) + 1)
		}
	}
	p.st.PrevBytes = cur
	p.st.StartBytes = bytesNow
	p.st.WindowStart = p.Now()
	p.st.Windows++
	p.save()
	return p.st.Rate, p.st.Rate != old
}

func (p *Pricing) clamp(r uint32) uint32 {
	if p.Min > 0 && r < p.Min {
		r = p.Min
	}
	if p.Max > 0 && r > p.Max {
		r = p.Max
	}
	if r == 0 {
		r = 1
	}
	return r
}

func (p *Pricing) save() {
	if p.File == "" {
		return
	}
	data, err := json.Marshal(p.st)
	if err != nil {
		return
	}
	os.WriteFile(p.File, data, 0o600) // best effort: a failed save costs one window of memory at most
}
