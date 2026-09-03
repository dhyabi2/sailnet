package shape

import (
	"math/rand"
	"sort"
)

// PRec is one record in a profile prefix: direction, bytes on the wire, and
// the gap from the previous record.
type PRec struct {
	Up  bool    `json:"up"`
	N   int     `json:"n"`
	Gap float64 `json:"gap"` // milliseconds since the previous record
}

// A Profile is what ordinary HTTPS looks like on the wire, measured rather
// than imagined. Prefixes are the first K records of real single-layer TLS
// connections; Sizes is the record-size distribution of their steady state.
// The tunnel replays a sampled prefix at the start of every connection, so
// the records a censor's classifier reads first are drawn from real HTTPS
// instead of tracing the inner handshake.
type Profile struct {
	K        int      `json:"k"`
	Prefixes [][]PRec `json:"prefixes"`
	Sizes    []int    `json:"sizes"` // sorted sample of steady-state record sizes
}

// BuildProfile summarises traces (which must be ordinary HTTPS captures) into
// a profile of at most maxPrefix prefixes of k records and maxSizes sizes.
func BuildProfile(traces []*Trace, k, maxPrefix, maxSizes int) *Profile {
	p := &Profile{K: k}
	var sizes []int
	for _, t := range traces {
		var pre []PRec
		prev := 0.0
		for i, r := range t.Recs {
			if i < k {
				pre = append(pre, PRec{Up: r.Up, N: r.N, Gap: r.Ms - prev})
				prev = r.Ms
			} else {
				sizes = append(sizes, r.N)
			}
		}
		if len(pre) == k {
			p.Prefixes = append(p.Prefixes, pre)
		}
	}
	rand.Shuffle(len(p.Prefixes), func(i, j int) { p.Prefixes[i], p.Prefixes[j] = p.Prefixes[j], p.Prefixes[i] })
	if len(p.Prefixes) > maxPrefix {
		p.Prefixes = p.Prefixes[:maxPrefix]
	}
	rand.Shuffle(len(sizes), func(i, j int) { sizes[i], sizes[j] = sizes[j], sizes[i] })
	if len(sizes) > maxSizes {
		sizes = sizes[:maxSizes]
	}
	sort.Ints(sizes)
	p.Sizes = sizes
	return p
}

// SamplePrefix returns the up-direction records of a random real prefix. Each
// side of the tunnel shapes only what it sends, so the up and down marginals
// are real HTTPS even though the interleaving is approximated.
func (p *Profile) SamplePrefix(up bool) []PRec {
	if p == nil || len(p.Prefixes) == 0 {
		return nil
	}
	pre := p.Prefixes[rand.Intn(len(p.Prefixes))]
	out := make([]PRec, 0, len(pre))
	gap := 0.0
	for _, r := range pre {
		gap += r.Gap
		if r.Up == up {
			out = append(out, PRec{Up: up, N: r.N, Gap: gap})
			gap = 0
		}
	}
	return out
}

// SampleSize draws one steady-state record size.
func (p *Profile) SampleSize() int {
	if p == nil || len(p.Sizes) == 0 {
		return 0
	}
	return p.Sizes[rand.Intn(len(p.Sizes))]
}
