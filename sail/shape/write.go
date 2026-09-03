package shape

import (
	"crypto/rand"
	"encoding/binary"
	"math/big"
	"sync"
	"time"
)

// ChunkWriter is one side of a tunnel connection. WriteChunk sends p as a
// single TLS record; WireSize says how many bytes on the wire a chunk of n
// payload bytes becomes (TLS header, AEAD tag and any framing the caller adds).
type ChunkWriter interface {
	WriteChunk(p []byte) error
	WireSize(n int) int
}

// A Shaper decides record boundaries and cover traffic for one connection.
// The first Params.FrontK records replay a prefix sampled from the profile of
// ordinary HTTPS; after that records are cut at sizes drawn from the same
// measurement, or at random when no profile is loaded.
type Shaper struct {
	mu      sync.Mutex
	p       *Params
	front   []PRec
	fi      int
	budget  int
	queue   []byte
	started time.Time
	Padding int // padding bytes spent, for the overhead report
	Data    int // payload bytes carried
}

// NewShaper starts a shaper for one connection. up is true on the client side.
func NewShaper(up bool) *Shaper {
	p := Get()
	s := &Shaper{p: p, budget: p.FrontBudget, started: time.Now()}
	if p.FrontK > 0 && p.Profile != nil {
		s.front = p.Profile.SamplePrefix(up)
		if len(s.front) > p.FrontK {
			s.front = s.front[:p.FrontK]
		}
	}
	return s
}

// Params returns the parameters this shaper was created with.
func (s *Shaper) Params() *Params { return s.p }

// Write sends payload, cutting it into records. pad returns one padding cell,
// used when the profile calls for a bigger record than there is data.
func (s *Shaper) Write(w ChunkWriter, payload []byte, pad func() []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Data += len(payload)
	s.queue = append(s.queue, payload...)
	// Front window: replay real HTTPS record sizes (and gaps) exactly.
	for s.fi < len(s.front) {
		want := s.front[s.fi].N
		chunk := want - (w.WireSize(0))
		if chunk < 1 {
			s.fi++
			continue
		}
		for len(s.queue) < chunk {
			if s.budget <= 0 {
				s.front = nil // budget spent: fall through to the ordinary cutter
				goto steady
			}
			c := pad()
			s.budget -= len(c)
			s.Padding += len(c)
			s.queue = append(s.queue, c...)
		}
		if s.p.FrontTiming {
			if g := s.front[s.fi].Gap; g > 0 && g < 2000 {
				s.mu.Unlock()
				time.Sleep(time.Duration(g * float64(time.Millisecond)))
				s.mu.Lock()
			}
		}
		if err := w.WriteChunk(s.queue[:chunk]); err != nil {
			return err
		}
		s.queue = s.queue[chunk:]
		s.fi++
	}
steady:
	return s.drain(w, true)
}

// drain writes the queue as records. With hold set, a remainder smaller than
// MinRecord after a full-size record stays queued for the next burst: sixteen
// cells are 16384 bytes and a full record carries 16380, so without this
// every full record was followed by a 4-byte one. Flush drains everything.
func (s *Shaper) drain(w ChunkWriter, hold bool) error {
	for len(s.queue) > 0 {
		n := s.cut(w, len(s.queue))
		if hold && n < len(s.queue) && len(s.queue)-n < s.p.MinRecord {
			if err := w.WriteChunk(s.queue[:n]); err != nil {
				return err
			}
			s.queue = s.queue[n:]
			return nil
		}
		if err := w.WriteChunk(s.queue[:n]); err != nil {
			return err
		}
		s.queue = s.queue[n:]
	}
	return nil
}

// Pending is the number of bytes held back for the next burst.
func (s *Shaper) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

// Flush writes anything held back, as its own record.
func (s *Shaper) Flush(w ChunkWriter) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drain(w, false)
}

// cut picks the payload length of the next steady-state record: the whole
// burst if it fits, else full-size records like any web server writing a
// response. Random cutting and size sampling were tried first and measured
// worse: every cut leaves a remainder, and the remainders made 43 % of the
// tunnel's records smaller than 200 bytes against 12 % for ordinary HTTPS.
func (s *Shaper) cut(w ChunkWriter, avail int) int {
	max := s.p.MaxRecord
	if max <= 0 || max > 16384 {
		max = 16384
	}
	// A TLS record holds 16384 bytes of plaintext including the WebSocket
	// frame header; a chunk that ignores the header becomes a full record
	// plus a 4-byte one, which is how "-16406 -26" pairs showed up in the
	// traces.
	max -= w.WireSize(max) - max - 22
	if avail <= max {
		return avail
	}
	return max
}

func randInt(n int) int {
	if n <= 1 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		var b [2]byte
		rand.Read(b[:])
		return int(binary.BigEndian.Uint16(b[:])) % n
	}
	return int(v.Int64())
}

// Chance returns true with probability p.
func Chance(p float64) bool {
	if p <= 0 {
		return false
	}
	if p >= 1 {
		return true
	}
	return float64(randInt(1<<20)) < p*float64(1<<20)
}
