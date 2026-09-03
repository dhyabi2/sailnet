package nano

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Budget is a global RPC spend limiter and meter. Public Nano RPCs ban bursts
// and cap free use at a few thousand calls per day, so every call passes
// through a token bucket (default 1 call / 2 s sustained, burst 8) and is
// counted per action. Counters persist to a small JSON file so a relay can
// report calls/day and stay under its provider's free tier for a whole month.
type Budget struct {
	mu       sync.Mutex
	tokens   float64
	last     time.Time
	Rate     float64 // tokens per second
	Burst    float64
	counts   map[string]*int64
	total    atomic.Int64
	since    time.Time
	path     string
	Deferred atomic.Int64 // calls that waited for a token
}

// DefaultBudget is used by every Client unless overridden.
var DefaultBudget = NewBudget(0.3, 10)

// NewBudget creates a limiter at rate calls/second with the given burst.
func NewBudget(rate, burst float64) *Budget {
	return &Budget{tokens: burst, last: time.Now(), Rate: rate, Burst: burst, counts: map[string]*int64{}, since: time.Now()}
}

// Persist loads previous counters from path and saves every minute.
func (b *Budget) Persist(path string) {
	b.path = path
	var saved struct {
		Since  time.Time        `json:"since"`
		Total  int64            `json:"total"`
		Counts map[string]int64 `json:"counts"`
	}
	if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &saved) == nil {
		b.mu.Lock()
		b.since = saved.Since
		b.total.Store(saved.Total)
		for k, v := range saved.Counts {
			n := v
			b.counts[k] = &n
		}
		b.mu.Unlock()
	}
	go func() {
		for {
			time.Sleep(time.Minute)
			b.save()
		}
	}()
}

func (b *Budget) save() {
	if b.path == "" {
		return
	}
	s := b.Snapshot()
	data, _ := json.MarshalIndent(s, "", "  ")
	os.WriteFile(b.path, data, 0o600)
}

// Wait blocks until a token is available (or ctx ends) and records the call.
func (b *Budget) Wait(ctx context.Context, action string) error {
	for {
		b.mu.Lock()
		now := time.Now()
		b.tokens += now.Sub(b.last).Seconds() * b.Rate
		if b.tokens > b.Burst {
			b.tokens = b.Burst
		}
		b.last = now
		if b.tokens >= 1 {
			b.tokens--
			c := b.counts[action]
			if c == nil {
				c = new(int64)
				b.counts[action] = c
			}
			*c++
			b.mu.Unlock()
			b.total.Add(1)
			return nil
		}
		wait := time.Duration((1 - b.tokens) / b.Rate * float64(time.Second))
		b.mu.Unlock()
		b.Deferred.Add(1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// Snapshot is the meter's report.
type Snapshot struct {
	Since     time.Time        `json:"since"`
	Total     int64            `json:"total"`
	PerDay    float64          `json:"per_day"`
	Counts    map[string]int64 `json:"counts"`
	Deferred  int64            `json:"deferred"`
	UpdatedAt time.Time        `json:"updated_at"`
}

// Snapshot returns current counters.
func (b *Budget) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := Snapshot{Since: b.since, Total: b.total.Load(), Counts: map[string]int64{}, Deferred: b.Deferred.Load(), UpdatedAt: time.Now()}
	for k, v := range b.counts {
		s.Counts[k] = *v
	}
	if d := time.Since(b.since).Hours() / 24; d > 0 {
		s.PerDay = float64(s.Total) / d
	}
	return s
}

// LogSummary prints a one-line budget report.
func (b *Budget) LogSummary(prefix string) {
	s := b.Snapshot()
	log.Printf("%s rpc calls total=%d (%.0f/day) deferred=%d since %s by action %v", prefix, s.Total, s.PerDay, s.Deferred, s.Since.Format("2006-01-02"), s.Counts)
}
