package token

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dhyabi2/sail/nano"
)

// The batched read of the registry and the original per-account walk must
// return the same registry, block for block. This asks the live ledger and
// compares them, which takes minutes because the old path is slow, so it
// runs only when asked:
//
//	SAIL_LIVE_LEDGER=1 go test ./token/ -run Matches -v -timeout 20m
//
// Recorded result, 2026-09-06, 47 relays on the ledger:
// batched 6.0s, per-account 5m40s, identical registry fingerprint.
func TestBatchedIndexerMatchesTheOldWalk(t *testing.T) {
	if os.Getenv("SAIL_LIVE_LEDGER") == "" {
		t.Skip("set SAIL_LIVE_LEDGER=1 to compare against the live ledger")
	}
	const treasury = "nano_1cexacexqrr51coik9eeqzebmfej7g6chrd1eygmzm9ad86qek84pnhpda1t"
	c := nano.NewClient()
	ix := &Indexer{Client: c, Treasury: treasury}
	ctx := context.Background()

	t0 := time.Now()
	fast, err := ix.Run(ctx)
	if err != nil {
		t.Fatalf("batched: %v", err)
	}
	fastTook := time.Since(t0)

	hist, err := c.History(ctx, treasury)
	if err != nil {
		t.Fatal(err)
	}
	senders := map[string]bool{}
	for _, b := range hist {
		if (b.Subtype == "receive" || b.Subtype == "open" || b.Type == "receive" || b.Type == "open") && b.Account != "" {
			senders[b.Account] = true
		}
	}
	if rs, err := c.Receivables(ctx, treasury, 1000); err == nil {
		for _, r := range rs {
			senders[r.Source] = true
		}
	}
	t1 := time.Now()
	slow, err := ix.runPerAccount(ctx, senders)
	if err != nil {
		t.Fatal(err)
	}
	slowTook := time.Since(t1)
	t.Logf("batched %d relays in %s; per-account %d relays in %s (%.0fx)",
		len(fast.Relays), fastTook.Round(time.Millisecond),
		len(slow.Relays), slowTook.Round(time.Millisecond),
		float64(slowTook)/float64(fastTook))

	// The batched read ran first, so anything published since may only have
	// reached the per-account one.
	sameRegistry(t, fast.Relays, slow.Relays, "batched", "per-account")
}

// sameRegistry demands two reads of the live ledger describe the same
// network. What a relay registered — its country, network, price, flags and
// descriptor — cannot change without a new block, so those must match
// exactly. Its height and last-seen time can legitimately advance between
// two reads seconds apart, because relays publish heartbeats while the test
// runs; they may move forward, never backward.
func sameRegistry(t *testing.T, want, got map[string]*Relay, wantName, gotName string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("relay count differs: %s %d, %s %d", wantName, len(want), gotName, len(got))
	}
	for acct, a := range want {
		b := got[acct]
		if b == nil {
			t.Fatalf("%s missing from the %s result", acct, gotName)
		}
		if a.Country != b.Country || a.ASN != b.ASN || a.MinRate != b.MinRate ||
			a.Flags != b.Flags || a.Descriptor != b.Descriptor {
			t.Errorf("%s registered differently:\n  %s %+v\n  %s %+v", acct, wantName, a, gotName, b)
		}
		if b.Height < a.Height {
			t.Errorf("%s went backwards: %s height %d, %s height %d", acct, wantName, a.Height, gotName, b.Height)
		}
		if b.LastSeen < a.LastSeen {
			t.Errorf("%s went backwards: %s last seen %d, %s last seen %d", acct, wantName, a.LastSeen, gotName, b.LastSeen)
		}
	}
}

// The ledger cache must not change the answer, only the time it takes. A
// block is immutable, so a second read that trusts the cache has to produce
// exactly the registry the first one built from scratch.
func TestLedgerCacheChangesNothingButSpeed(t *testing.T) {
	if os.Getenv("SAIL_LIVE_LEDGER") == "" {
		t.Skip("set SAIL_LIVE_LEDGER=1 to compare against the live ledger")
	}
	const treasury = "nano_1cexacexqrr51coik9eeqzebmfej7g6chrd1eygmzm9ad86qek84pnhpda1t"
	ctx := context.Background()

	cold, err := (&Indexer{Client: nano.NewClient(), Treasury: treasury}).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ix := &Indexer{Client: nano.NewClient(), Treasury: treasury, CacheFile: filepath.Join(t.TempDir(), "ledger-cache.json")}
	t0 := time.Now()
	if _, err := ix.Run(ctx); err != nil { // fills the cache
		t.Fatal(err)
	}
	fill := time.Since(t0)
	t1 := time.Now()
	warm, err := ix.Run(ctx) // uses it
	if err != nil {
		t.Fatal(err)
	}
	reuse := time.Since(t1)
	t.Logf("first read %s, cached read %s", fill.Round(time.Millisecond), reuse.Round(time.Millisecond))

	sameRegistry(t, cold.Relays, warm.Relays, "uncached", "cached")
	// Timing is not asserted: this talks to a shared public endpoint whose
	// latency varies by seconds from call to call, so a threshold here would
	// fail for reasons that have nothing to do with the cache. What the
	// cache must do is not change the answer; the measured effect on a quiet
	// endpoint is 2.7s uncached against 0.6-0.8s cached.
	_, _ = fill, reuse
}
