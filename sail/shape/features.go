package shape

import "math"

// The feature set follows the published TLS-in-TLS literature. Two ideas
// carry almost all of the signal there:
//
//   1. The encapsulated handshake (Xue et al., USENIX Security 2024,
//      "Fingerprinting Obfuscated Proxy Traffic with Encapsulated TLS
//      Handshakes"): inside a proxy connection the client's inner TLS
//      handshake shows through as a medium client record (the inner
//      ClientHello, ~500 B), a multi-kilobyte server burst (the inner
//      certificate) and a small client record (the inner Finished), in that
//      order, before any bulk data.
//   2. Record-length sequences (the Trojan/Xray detectors and the obfs4
//      length-distribution work): the first ~20 record sizes and their
//      direction pattern separate tunnelled from ordinary sessions even when
//      the handshake rule misses.
//
// Both are computed here from records only. Nothing needs the keys.

const FeatN = 20 // records the sequence features look at

// FeatureNames documents the vector, in order.
var FeatureNames = featureNames()

func featureNames() []string {
	n := make([]string, 0, 3*FeatN+24)
	for i := 0; i < FeatN; i++ {
		n = append(n, "size", "dir", "gap")
	}
	return append(n,
		"nrecs", "up_bytes", "down_bytes", "up_recs", "down_recs", "down_up_ratio",
		"dir_changes", "burst1_up", "burst1_down", "burst2_up", "burst2_down",
		"mean_up", "std_up", "mean_down", "std_down", "max_down",
		"frac_small", "frac_large", "t_first_down", "t_span",
		"hs_ch_size", "hs_cert_bytes", "hs_fin_size", "hs_rule")
}

// Features turns a trace into the classifier's input vector. Only application
// data records are used: the outer handshake is identical for a browser and
// for the tunnel by construction, so a censor gains nothing from it.
func Features(t *Trace) []float64 {
	var app []Record
	for _, r := range t.Recs {
		if r.Type == 23 {
			app = append(app, r)
		}
	}
	f := make([]float64, 0, 3*FeatN+24)
	var prev float64
	for i := 0; i < FeatN; i++ {
		if i < len(app) {
			d := -1.0
			if app[i].Up {
				d = 1
			}
			f = append(f, float64(app[i].N), d, app[i].Ms-prev)
			prev = app[i].Ms
		} else {
			f = append(f, 0, 0, 0)
		}
	}
	var upB, downB, upN, downN, changes, small, large float64
	var upS, downS []float64
	tFirstDown := -1.0
	last := 0
	for _, r := range app {
		if r.Up {
			upB += float64(r.N)
			upN++
			upS = append(upS, float64(r.N))
			if last == -1 {
				changes++
			}
			last = 1
		} else {
			downB += float64(r.N)
			downN++
			downS = append(downS, float64(r.N))
			if tFirstDown < 0 {
				tFirstDown = r.Ms
			}
			if last == 1 {
				changes++
			}
			last = -1
		}
		if r.N < 200 {
			small++
		}
		if r.N > 8000 {
			large++
		}
	}
	bursts := burstSizes(app)
	get := func(i int) float64 {
		if i < len(bursts) {
			return bursts[i]
		}
		return 0
	}
	span := 0.0
	if len(app) > 0 {
		span = app[len(app)-1].Ms
	}
	tot := float64(len(app))
	if tot == 0 {
		tot = 1
	}
	ch, cert, fin, rule := handshakeRule(app)
	f = append(f,
		float64(len(app)), upB, downB, upN, downN, ratio(downB, upB),
		changes, get(0), get(1), get(2), get(3),
		mean(upS), std(upS), mean(downS), std(downS), max(downS),
		small/tot, large/tot, tFirstDown, span,
		ch, cert, fin, rule)
	return f
}

// burstSizes returns the byte totals of consecutive same-direction runs,
// starting with the first client run.
func burstSizes(app []Record) []float64 {
	var out []float64
	cur := 0.0
	var dir int
	for _, r := range app {
		d := -1
		if r.Up {
			d = 1
		}
		if dir == 0 {
			dir = d
		}
		if d != dir {
			out = append(out, cur)
			cur = 0
			dir = d
		}
		cur += float64(r.N)
	}
	if cur > 0 {
		out = append(out, cur)
	}
	if len(out) > 0 && app[0].Up == false {
		out = append([]float64{0}, out...) // keep index 0 = first client burst
	}
	return out
}

// handshakeRule is the Xue et al. encapsulated-handshake detector. It returns
// the candidate inner ClientHello size, the bytes of the candidate inner
// certificate burst, the candidate inner Finished size, and 1 if the whole
// pattern is present in the first records of the connection.
func handshakeRule(app []Record) (ch, cert, fin, hit float64) {
	const window = 12
	n := len(app)
	if n > window {
		n = window
	}
	for i := 0; i < n; i++ {
		if !app[i].Up || app[i].N < 250 || app[i].N > 2200 {
			continue // not a plausible inner ClientHello
		}
		j := i + 1
		down := 0.0
		for ; j < len(app) && !app[j].Up; j++ {
			down += float64(app[j].N)
		}
		if down < 1500 || j >= len(app) {
			continue // no server burst big enough to be a certificate
		}
		up2 := float64(app[j].N)
		if up2 > 400 {
			continue // the inner Finished (plus early data) is small
		}
		if app[j].Ms-app[i].Ms > 3000 {
			continue
		}
		return float64(app[i].N), down, up2, 1
	}
	return 0, 0, 0, 0
}

// RuleDetect is the published rule on its own.
func RuleDetect(t *Trace) bool {
	var app []Record
	for _, r := range t.Recs {
		if r.Type == 23 {
			app = append(app, r)
		}
	}
	_, _, _, hit := handshakeRule(app)
	return hit == 1
}

func ratio(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func std(v []float64) float64 {
	if len(v) < 2 {
		return 0
	}
	m := mean(v)
	s := 0.0
	for _, x := range v {
		s += (x - m) * (x - m)
	}
	return math.Sqrt(s / float64(len(v)-1))
}

func max(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}
