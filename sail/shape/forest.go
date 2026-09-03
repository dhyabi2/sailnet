package shape

import (
	"encoding/json"
	"math"
	"math/rand"
	"os"
	"sort"
)

// A small CART random forest. The published TLS-in-TLS detectors use random
// forests or gradient boosting over exactly this kind of length-sequence
// feature vector, so this is the model the shaping is tuned against. Keeping
// it in the repository means the shaping numbers can be reproduced
// with `sailtrace` and nothing else.

type node struct {
	Feat  int     `json:"f"`
	Thr   float64 `json:"t"`
	L     *node   `json:"l,omitempty"`
	R     *node   `json:"r,omitempty"`
	Score float64 `json:"s"` // leaf: fraction of positives
}

type Forest struct {
	Trees    []*node `json:"trees"`
	Features int     `json:"features"`
}

type Sample struct {
	X []float64
	Y int // 1 = tunnel, 0 = ordinary HTTPS
}

// TrainForest grows nTrees trees of at most depth on bootstrap samples,
// choosing each split from a random sqrt(d) subset of the features.
func TrainForest(data []Sample, nTrees, depth, minLeaf int, rng *rand.Rand) *Forest {
	if len(data) == 0 {
		return &Forest{}
	}
	d := len(data[0].X)
	mtry := int(math.Sqrt(float64(d)))
	if mtry < 2 {
		mtry = 2
	}
	if mtry > d {
		mtry = d
	}
	f := &Forest{Features: d}
	for i := 0; i < nTrees; i++ {
		boot := make([]Sample, len(data))
		for j := range boot {
			boot[j] = data[rng.Intn(len(data))]
		}
		f.Trees = append(f.Trees, grow(boot, depth, minLeaf, mtry, rng))
	}
	return f
}

func grow(s []Sample, depth, minLeaf, mtry int, rng *rand.Rand) *node {
	pos := 0
	for _, x := range s {
		pos += x.Y
	}
	leaf := &node{Score: float64(pos) / float64(len(s)), Feat: -1}
	if depth == 0 || len(s) < 2*minLeaf || pos == 0 || pos == len(s) {
		return leaf
	}
	bestGain, bestFeat, bestThr := 0.0, -1, 0.0
	parent := gini(pos, len(s))
	// mtry features drawn without replacement, as a random forest does.
	perm := rng.Perm(len(s[0].X))
	for t := 0; t < mtry && t < len(perm); t++ {
		fi := perm[t]
		vals := make([]float64, 0, len(s))
		for _, x := range s {
			vals = append(vals, x.X[fi])
		}
		sort.Float64s(vals)
		for _, q := range []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9} {
			thr := vals[int(q*float64(len(vals)-1))]
			lp, ln, rp, rn := 0, 0, 0, 0
			for _, x := range s {
				if x.X[fi] <= thr {
					ln++
					lp += x.Y
				} else {
					rn++
					rp += x.Y
				}
			}
			if ln < minLeaf || rn < minLeaf {
				continue
			}
			g := parent - (float64(ln)*gini(lp, ln)+float64(rn)*gini(rp, rn))/float64(len(s))
			if g > bestGain {
				bestGain, bestFeat, bestThr = g, fi, thr
			}
		}
	}
	if bestFeat < 0 {
		return leaf
	}
	var l, r []Sample
	for _, x := range s {
		if x.X[bestFeat] <= bestThr {
			l = append(l, x)
		} else {
			r = append(r, x)
		}
	}
	return &node{Feat: bestFeat, Thr: bestThr, L: grow(l, depth-1, minLeaf, mtry, rng), R: grow(r, depth-1, minLeaf, mtry, rng)}
}

func gini(pos, n int) float64 {
	if n == 0 {
		return 0
	}
	p := float64(pos) / float64(n)
	return 2 * p * (1 - p)
}

// Score is the fraction of trees voting "tunnel", in [0,1].
func (f *Forest) Score(x []float64) float64 {
	if len(f.Trees) == 0 {
		return 0
	}
	s := 0.0
	for _, t := range f.Trees {
		n := t
		for n.Feat >= 0 {
			if x[n.Feat] <= n.Thr {
				n = n.L
			} else {
				n = n.R
			}
		}
		s += n.Score
	}
	return s / float64(len(f.Trees))
}

func (f *Forest) SaveJSON(path string) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func LoadForest(path string) (*Forest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Forest
	return &f, json.Unmarshal(b, &f)
}

// Metrics reports what a censor cares about: how many tunnels it catches at a
// false-positive rate it can afford on ordinary traffic.
type Metrics struct {
	AUC        float64 `json:"auc"`
	TPRat1pct  float64 `json:"tpr_at_fpr_1pct"`
	TPRat01pct float64 `json:"tpr_at_fpr_0.1pct"`
	Thr1pct    float64 `json:"thr_1pct"`
	RuleTPR    float64 `json:"rule_tpr"`
	RuleFPR    float64 `json:"rule_fpr"`
	NPos       int     `json:"n_pos"`
	NNeg       int     `json:"n_neg"`
}

// Evaluate scores positives (tunnel traces) and negatives (ordinary HTTPS).
func Evaluate(f *Forest, pos, neg []Sample) Metrics {
	ps := make([]float64, len(pos))
	for i, s := range pos {
		ps[i] = f.Score(s.X)
	}
	ns := make([]float64, len(neg))
	for i, s := range neg {
		ns[i] = f.Score(s.X)
	}
	m := Metrics{NPos: len(pos), NNeg: len(neg)}
	m.AUC = auc(ps, ns)
	m.Thr1pct, m.TPRat1pct = tprAt(ps, ns, 0.01)
	_, m.TPRat01pct = tprAt(ps, ns, 0.001)
	return m
}

// tprAt finds the lowest threshold whose false-positive rate is at most fpr,
// and the true-positive rate there.
func tprAt(pos, neg []float64, fpr float64) (thr, tpr float64) {
	s := append([]float64{}, neg...)
	sort.Float64s(s)
	k := int(math.Ceil((1 - fpr) * float64(len(s))))
	if k >= len(s) {
		k = len(s) - 1
	}
	if k < 0 {
		return 1, 0
	}
	thr = s[k]
	hit := 0
	for _, p := range pos {
		if p > thr {
			hit++
		}
	}
	if len(pos) == 0 {
		return thr, 0
	}
	return thr, float64(hit) / float64(len(pos))
}

func auc(pos, neg []float64) float64 {
	if len(pos) == 0 || len(neg) == 0 {
		return 0
	}
	wins := 0.0
	for _, p := range pos {
		for _, n := range neg {
			if p > n {
				wins++
			} else if p == n {
				wins += 0.5
			}
		}
	}
	return wins / float64(len(pos)*len(neg))
}

// newRand is a small helper so tests do not import math/rand themselves.
func newRand(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }
