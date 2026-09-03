// Package shape holds Sailnet's traffic-shaping parameters, the wire tap that
// records real record-level traces, and the TLS-in-TLS classifier the shaping
// is tuned against. Everything here is measurable: the measurement tool `sailtrace` reproduces
// the numbers this package is tuned with.
package shape

import (
	"encoding/json"
	"os"
	"sync/atomic"
	"time"
)

// Params are every knob the tunnel writer has. They are loaded once at start
// (SAIL_SHAPE=<file>) and swapped atomically by the tuner.
type Params struct {
	// Coalesce is the quiet gap that ends a burst: the writer keeps
	// gathering cells while they keep arriving within this interval of each
	// other, so a stream of cells leaves as a few large records instead of
	// one record each. On a 100 KB/s path cells are 10 ms apart, so a
	// window shorter than that never gathers anything.
	Coalesce time.Duration `json:"coalesce"`
	// MaxDelay caps how long the first cell of a burst may wait, whatever
	// the arrival rhythm. FlushBytes ends a burst early once this much is
	// queued (one large TLS record's worth).
	MaxDelay   time.Duration `json:"max_delay"`
	FlushBytes int           `json:"flush_bytes"`
	// IdleGap: a burst that follows at least this much quiet counts as the
	// start of a new activity period and may be prefixed with cover.
	IdleGap time.Duration `json:"idle_gap"`
	// PadAfterIdle is the probability of prefixing such a burst with a padding cell.
	PadAfterIdle float64 `json:"pad_after_idle"`
	// PadTail is the probability of appending a padding cell to any burst.
	PadTail float64 `json:"pad_tail"`
	// MinRecord/MaxRecord bound the random record cutter outside the front window.
	MinRecord int `json:"min_record"`
	MaxRecord int `json:"max_record"`

	// FrontK is the number of records at the start of a tunnel connection
	// whose size and timing are drawn from Profile instead of from the data.
	// This is the defence against encapsulated-handshake detection: the first
	// records of a Sailnet connection are a sample from the measured
	// distribution of ordinary HTTPS, not a picture of the inner handshake.
	FrontK int `json:"front_k"`
	// FrontBudget caps the padding bytes the front window may spend.
	FrontBudget int `json:"front_budget"`
	// FrontTiming replays inter-record gaps from the profile when true.
	FrontTiming bool `json:"front_timing"`
	// Profile is the empirical model of ordinary HTTPS the front window imitates.
	Profile *Profile `json:"profile,omitempty"`
	// Note names the configuration in reports.
	Note string `json:"note,omitempty"`
}

// Default is the shipped configuration. The front-window values are the ones
// were measured on the live network; the rest are the pre-measurement defaults.
func Default() Params {
	return Params{
		Coalesce:     1500 * time.Microsecond,
		MaxDelay:     20 * time.Millisecond,
		FlushBytes:   16 * 1024,
		IdleGap:      400 * time.Millisecond,
		PadAfterIdle: 0.5,
		PadTail:      0.05,
		MinRecord:    100,
		MaxRecord:    16384,
		FrontK:       0, // set by Load/tuning; 0 keeps the old behaviour
		FrontBudget:  16384,
		FrontTiming:  false,
	}
}

var current atomic.Pointer[Params]

// Get returns the parameters in force.
func Get() *Params {
	if p := current.Load(); p != nil {
		return p
	}
	d := Default()
	current.CompareAndSwap(nil, &d)
	return current.Load()
}

// Set installs p for every connection opened from now on.
func Set(p Params) { current.Store(&p) }

// Load reads parameters from a JSON file and installs them.
func Load(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	p := Default()
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	Set(p)
	return nil
}

// Save writes p as JSON.
func Save(path string, p Params) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// LoadEnv installs SAIL_SHAPE's file if it is set. Errors are returned so a
// relay can refuse to start with a broken configuration.
func LoadEnv() error {
	if p := os.Getenv("SAIL_SHAPE"); p != "" {
		return Load(p)
	}
	return nil
}
