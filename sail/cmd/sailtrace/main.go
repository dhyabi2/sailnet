// sailtrace captures real record-level traces through the tunnel, trains the
// published TLS-in-TLS classifier on them, and tunes the shaping parameters
// against it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/dhyabi2/sail/shape"
	"github.com/dhyabi2/sail/shape/lab"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "capture":
		capture(os.Args[2:])
	case "profile":
		profile(os.Args[2:])
	case "train":
		train(os.Args[2:])
	case "eval":
		eval(os.Args[2:])
	case "tune":
		tune(os.Args[2:])
	case "summary":
		summary(os.Args[2:])
	case "origin":
		addr := ":8444"
		if len(os.Args) > 2 {
			addr = os.Args[2]
		}
		die(lab.ServeOrigin(addr))
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `sailtrace <command>

  capture -kind direct|tunnel -out FILE [-shape P.json] [-rounds N] [-label L]
        record TLS records of ordinary HTTPS, or of real web pages fetched
        through a local three-hop circuit (a genuine TLS-in-TLS session)
  profile -in direct.jsonl -out profile.json [-k 12]
        build the ordinary-HTTPS record model the front window imitates
  train  -pos FILE[,FILE] -neg FILE -out forest.json [-holdout 0.3]
        train the classifier and report its accuracy on held-out traces
  eval   -forest forest.json -pos FILE -neg FILE
        score traces with an existing classifier
  tune   -neg direct.jsonl -profile profile.json -out params.json [-rounds 6] [-iters 8]
        search shaping parameters, recapturing and retraining every round
  summary -in FILE
        record-size and overhead summary of a trace file`)
	os.Exit(2)
}

func sites(rounds int) []string {
	var out []string
	for i := 0; i < rounds; i++ {
		s := append([]string{}, lab.Sites...)
		rand.Shuffle(len(s), func(a, b int) { s[a], s[b] = s[b], s[a] })
		out = append(out, s...)
	}
	return out
}

func capture(args []string) {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	kind := fs.String("kind", "direct", "direct or tunnel")
	out := fs.String("out", "", "trace file")
	shapeFile := fs.String("shape", "", "shaping parameters (tunnel only)")
	label := fs.String("label", "", "label written into the traces")
	rounds := fs.Int("rounds", 1, "passes over the site list")
	hops := fs.Int("hops", 3, "circuit length")
	origin := fs.String("origin", "", "host:port of a running `sailtrace origin` (default: start one on loopback)")
	socks := fs.String("socks", "127.0.0.1:1080", "socks kind: the running client's SOCKS5 port")
	status := fs.String("status", "127.0.0.1:1090", "socks kind: the running client's status endpoint")
	fs.Parse(args)
	if *out == "" && *kind != "socks" {
		fs.Usage()
		os.Exit(2)
	}
	par := "default"
	if *shapeFile != "" {
		if err := shape.Load(*shapeFile); err != nil {
			die(err)
		}
		par = *shapeFile
	}
	if *kind == "socks" {
		// The running client writes the traces (SAIL_TRACE); we only drive it.
		o := lab.RemoteOrigin(*origin)
		die(lab.CaptureSocks(o, *socks, *status, *rounds))
		fmt.Printf("%d flows through %s\n", *rounds, *socks)
		return
	}
	sink, err := shape.Create(*out)
	die(err)
	defer sink.Close()
	lb := *label
	if lb == "" {
		lb = *kind
	}
	switch *kind {
	case "direct":
		die(lab.CaptureDirect(sites(*rounds), sink, par))
	case "local-direct":
		o, err := openOrigin(*origin)
		die(err)
		defer o.Close()
		die(lab.CaptureLocalDirect(o, *rounds, sink, par))
	case "local-tunnel":
		o, err := openOrigin(*origin)
		die(err)
		defer o.Close()
		n, err := lab.StartNet(*hops)
		die(err)
		defer n.Close()
		if err := lab.CaptureLocalTunnel(n, o, *rounds, sink, lb, par); err != nil {
			fmt.Fprintln(os.Stderr, "note:", err)
		}
	case "direct-bulk":
		die(lab.CaptureBulkDirect(*rounds, sink, par))
	case "tunnel-bulk":
		n, err := lab.StartNet(*hops)
		die(err)
		defer n.Close()
		if err := lab.CaptureBulkTunnel(n, *rounds, sink, lb, par); err != nil {
			fmt.Fprintln(os.Stderr, "note:", err)
		}
	case "direct-session":
		die(lab.CaptureDirectSession(sites(*rounds), sink, par))
	case "tunnel-session":
		n, err := lab.StartNet(*hops)
		die(err)
		defer n.Close()
		if err := lab.CaptureTunnelSession(n, sites(1), sink, lb, par, *rounds*len(lab.Sites)/12); err != nil {
			fmt.Fprintln(os.Stderr, "note:", err)
		}
	default:
		n, err := lab.StartNet(*hops)
		die(err)
		defer n.Close()
		if err := lab.CaptureTunnel(n, sites(*rounds), sink, lb, par); err != nil {
			fmt.Fprintln(os.Stderr, "note:", err)
		}
	}
	sink.Close()
	ts, err := shape.ReadTraces(*out)
	die(err)
	fmt.Printf("%s: %d traces\n", *out, len(ts))
}

// openOrigin uses a remote origin when given, else starts one on loopback.
func openOrigin(addr string) (*lab.Origin, error) {
	if addr != "" {
		return lab.RemoteOrigin(addr), nil
	}
	return lab.StartOrigin()
}

func profile(args []string) {
	fs := flag.NewFlagSet("profile", flag.ExitOnError)
	in := fs.String("in", "", "ordinary HTTPS traces")
	out := fs.String("out", "profile.json", "profile file")
	k := fs.Int("k", 12, "records in a prefix")
	fs.Parse(args)
	ts, err := shape.ReadTraces(*in)
	die(err)
	// The front window imitates application data, so drop the outer handshake.
	var app []*shape.Trace
	for _, t := range ts {
		c := &shape.Trace{Label: t.Label, Site: t.Site}
		for _, r := range t.Recs {
			if r.Type == 23 {
				c.Recs = append(c.Recs, r)
			}
		}
		if len(c.Recs) >= *k {
			app = append(app, c)
		}
	}
	p := shape.BuildProfile(app, *k, 400, 4000)
	b, _ := json.MarshalIndent(p, "", " ")
	die(os.WriteFile(*out, b, 0o644))
	fmt.Printf("%s: %d prefixes of %d records, %d steady-state sizes\n", *out, len(p.Prefixes), p.K, len(p.Sizes))
}

var minRecords = 0

func samples(files string, y int) []shape.Sample {
	var out []shape.Sample
	for _, f := range strings.Split(files, ",") {
		if f == "" {
			continue
		}
		ts, err := shape.ReadTraces(f)
		die(err)
		for _, t := range ts {
			if appRecords(t) < minRecords {
				continue
			}
			out = append(out, shape.Sample{X: shape.Features(t), Y: y})
		}
	}
	return out
}

func appRecords(t *shape.Trace) int {
	n := 0
	for _, r := range t.Recs {
		if r.Type == 23 {
			n++
		}
	}
	return n
}

func split(s []shape.Sample, holdout float64, rng *rand.Rand) (tr, te []shape.Sample) {
	rng.Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] })
	n := int(float64(len(s)) * holdout)
	return s[n:], s[:n]
}

func train(args []string) {
	fs := flag.NewFlagSet("train", flag.ExitOnError)
	pos := fs.String("pos", "", "tunnel traces (comma separated)")
	neg := fs.String("neg", "", "ordinary HTTPS traces")
	out := fs.String("out", "forest.json", "classifier file")
	holdout := fs.Float64("holdout", 0.3, "fraction held out for the report")
	fs.Parse(args)
	rng := rand.New(rand.NewSource(1))
	p, n := samples(*pos, 1), samples(*neg, 0)
	ptr, pte := split(p, *holdout, rng)
	ntr, nte := split(n, *holdout, rng)
	f := shape.TrainForest(append(append([]shape.Sample{}, ptr...), ntr...), 120, 8, 3, rng)
	die(f.SaveJSON(*out))
	m := shape.Evaluate(f, pte, nte)
	m.RuleTPR, m.RuleFPR = ruleRates(*pos, *neg)
	report("held out", m)
}

func ruleRates(pos, neg string) (tpr, fpr float64) {
	count := func(files string) (hit, n int) {
		for _, f := range strings.Split(files, ",") {
			if f == "" {
				continue
			}
			ts, err := shape.ReadTraces(f)
			die(err)
			for _, t := range ts {
				n++
				if shape.RuleDetect(t) {
					hit++
				}
			}
		}
		return
	}
	ph, pn := count(pos)
	nh, nn := count(neg)
	if pn > 0 {
		tpr = float64(ph) / float64(pn)
	}
	if nn > 0 {
		fpr = float64(nh) / float64(nn)
	}
	return
}

func report(what string, m shape.Metrics) {
	fmt.Printf("%s: %d tunnel / %d ordinary traces\n", what, m.NPos, m.NNeg)
	fmt.Printf("  forest AUC              %.3f\n", m.AUC)
	fmt.Printf("  detection at 1%% FPR     %.1f%%\n", 100*m.TPRat1pct)
	fmt.Printf("  detection at 0.1%% FPR   %.1f%%\n", 100*m.TPRat01pct)
	fmt.Printf("  handshake rule TPR/FPR  %.1f%% / %.1f%%\n", 100*m.RuleTPR, 100*m.RuleFPR)
}

func eval(args []string) {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	forest := fs.String("forest", "forest.json", "classifier")
	pos := fs.String("pos", "", "tunnel traces")
	neg := fs.String("neg", "", "ordinary HTTPS traces")
	fs.Parse(args)
	f, err := shape.LoadForest(*forest)
	die(err)
	m := shape.Evaluate(f, samples(*pos, 1), samples(*neg, 0))
	m.RuleTPR, m.RuleFPR = ruleRates(*pos, *neg)
	report(*pos, m)
}

func summary(args []string) {
	fs := flag.NewFlagSet("summary", flag.ExitOnError)
	in := fs.String("in", "", "trace file")
	fs.Parse(args)
	ts, err := shape.ReadTraces(*in)
	die(err)
	var sizes []int
	up, down, n := 0, 0, 0
	for _, t := range ts {
		u, d := t.Bytes()
		up += u
		down += d
		for _, r := range t.Recs {
			if r.Type == 23 {
				sizes = append(sizes, r.N)
				n++
			}
		}
	}
	sort.Ints(sizes)
	q := func(p float64) int {
		if len(sizes) == 0 {
			return 0
		}
		return sizes[int(p*float64(len(sizes)-1))]
	}
	fmt.Printf("%s: %d traces, %d app records, up %d B, down %d B\n", *in, len(ts), n, up, down)
	fmt.Printf("  record size p10/p50/p90/p99: %d / %d / %d / %d\n", q(.1), q(.5), q(.9), q(.99))
}

func die(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

var _ = time.Second

// candidate configurations the search walks through. The front window is the
// interesting axis: it decides how many records at the start of a connection
// are drawn from the ordinary-HTTPS profile instead of from the inner
// handshake. The rest are the pre-existing knobs.
func candidates(pro *shape.Profile) []shape.Params {
	base := shape.Default()
	base.Profile = pro
	var out []shape.Params
	add := func(name string, f func(p *shape.Params)) {
		p := base
		f(&p)
		p.Note = name
		out = append(out, p)
	}
	add("full records, 2ms delay cap", func(p *shape.Params) { p.FrontK = 0; p.MaxDelay = 2 * time.Millisecond })
	add("quiet 30ms, cap 250ms, no idle cover", func(p *shape.Params) {
		p.FrontK = 0
		p.Coalesce = 30 * time.Millisecond
		p.MaxDelay = 250 * time.Millisecond
		p.PadAfterIdle = 0
		p.PadTail = 0
	})
	add("quiet 60ms, cap 400ms, front 8", func(p *shape.Params) {
		p.FrontK = 8
		p.Coalesce = 60 * time.Millisecond
		p.MaxDelay = 400 * time.Millisecond
		p.PadAfterIdle = 0.15
		p.PadTail = 0.02
	})
	return out
}

func tune(args []string) {
	fs := flag.NewFlagSet("tune", flag.ExitOnError)
	neg := fs.String("neg", "", "ordinary HTTPS traces (the negative class)")
	profileFile := fs.String("profile", "profile.json", "ordinary-HTTPS profile")
	out := fs.String("out", "params.json", "best parameters")
	dir := fs.String("dir", "traces", "where per-candidate traces are written")
	rounds := fs.Int("rounds", 2, "passes over the site list per candidate")
	hops := fs.Int("hops", 3, "circuit length")
	session := fs.Bool("session", true, "matched workload against one origin: capture the negative class with `capture -kind local-direct`")
	origin := fs.String("origin", "", "host:port of a running `sailtrace origin` (default: loopback)")
	apply := fs.String("apply", "", "live mode: command run as `<cmd> <params.json>` before each candidate; it must install the parameters on the relays and restart the local client with SAIL_TRACE=<dir>/live.jsonl")
	socks := fs.String("socks", "127.0.0.1:1080", "live mode: the client's SOCKS5 port")
	status := fs.String("status", "127.0.0.1:1090", "live mode: the client's status endpoint")
	minRecs := fs.Int("min", 20, "ignore traces with fewer application records (RTT probes, failed flows)")
	fs.Parse(args)
	minRecords = *minRecs
	b, err := os.ReadFile(*profileFile)
	die(err)
	var pro shape.Profile
	die(json.Unmarshal(b, &pro))
	negS := samples(*neg, 0)
	if len(negS) < 10 {
		die(fmt.Errorf("need at least 10 ordinary-HTTPS traces, have %d", len(negS)))
	}
	die(os.MkdirAll(*dir, 0o755))
	var n *lab.Net
	if *apply == "" {
		n, err = lab.StartNet(*hops)
		die(err)
		defer n.Close()
	}
	o, err := openOrigin(*origin)
	die(err)
	defer o.Close()

	type result struct {
		P shape.Params
		M shape.Metrics
		O float64 // padding as a fraction of payload
		F string
	}
	var results []result
	for i, p := range candidates(&pro) {
		shape.Set(p)
		f := fmt.Sprintf("%s/cand%02d.jsonl", *dir, i)
		if *apply != "" {
			// Live mode: the relays and the local client take the parameters
			// from a file; the client records the traces itself.
			pf := fmt.Sprintf("%s/cand%02d.params.json", *dir, i)
			die(shape.Save(pf, p))
			live := *dir + "/live.jsonl"
			os.Remove(live)
			cmd := exec.Command(*apply, pf)
			cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
			if err := cmd.Run(); err != nil {
				fmt.Printf("%-38s apply failed: %v\n", p.Note, err)
				continue
			}
			if err := lab.CaptureSocks(o, *socks, *status, *rounds); err != nil {
				fmt.Fprintln(os.Stderr, "note:", err)
			}
			// Restart the client once more so its trace file is flushed.
			exec.Command(*apply, pf, "flush").Run()
			os.Rename(live, f)
		} else {
			sink, err := shape.Create(f)
			die(err)
			var cerr error
			if *session {
				cerr = lab.CaptureLocalTunnel(n, o, *rounds, sink, "tunnel", p.Note)
			} else {
				cerr = lab.CaptureTunnel(n, sites(*rounds), sink, "tunnel", p.Note)
			}
			if cerr != nil {
				fmt.Fprintln(os.Stderr, "note:", cerr)
			}
			sink.Close()
		}
		posS := samples(f, 1)
		if len(posS) < 5 {
			fmt.Printf("%-38s captured only %d traces, skipped\n", p.Note, len(posS))
			continue
		}
		// The censor retrains on this exact shaping: the classifier is fitted
		// to the candidate's own traces, not to an older variant. Anything
		// less would flatter the result.
		rng := rand.New(rand.NewSource(7))
		ptr, pte := split(posS, 0.3, rng)
		ntr, nte := split(append([]shape.Sample{}, negS...), 0.3, rng)
		fo := shape.TrainForest(append(append([]shape.Sample{}, ptr...), ntr...), 120, 8, 3, rng)
		m := shape.Evaluate(fo, pte, nte)
		m.RuleTPR, m.RuleFPR = ruleRates(f, *neg)
		over := overhead(f)
		results = append(results, result{P: p, M: m, O: over, F: f})
		fmt.Printf("%-38s AUC %.3f  TPR@1%%FPR %5.1f%%  rule %5.1f%%\n",
			p.Note, m.AUC, 100*m.TPRat1pct, 100*m.RuleTPR)
		_ = over
	}
	if len(results) == 0 {
		die(fmt.Errorf("no candidate produced traces"))
	}
	sort.Slice(results, func(i, j int) bool {
		if a, b := results[i].M.TPRat1pct, results[j].M.TPRat1pct; a != b {
			return a < b
		}
		return results[i].O < results[j].O
	})
	fmt.Println("up/down is the client's bytes sent over bytes received; ordinary HTTPS on this workload:", fmt.Sprintf("%.3f", overhead(*neg)))
	best := results[0]
	die(shape.Save(*out, best.P))
	fmt.Printf("\nbest: %s -> %s\n", best.P.Note, *out)
	report("best candidate", best.M)
}

// overhead is padding bytes as a fraction of the client's payload, measured
// from the traces themselves: everything the tunnel sent above what the same
// pages cost through an untunnelled fetch is cover plus protocol.
func overhead(file string) float64 {
	ts, err := shape.ReadTraces(file)
	die(err)
	up, down := 0, 0
	for _, t := range ts {
		u, d := t.Bytes()
		up += u
		down += d
	}
	if down == 0 {
		return 0
	}
	return float64(up) / float64(down)
}
