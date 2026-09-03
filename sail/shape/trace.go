package shape

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// A Record is one TLS record as a network observer sees it: the direction, the
// content type from the plaintext header, the total bytes on the wire and the
// offset from the first byte of the connection. This is exactly the view a DPI
// box has of an encrypted session, and the only input the classifier gets.
type Record struct {
	Ms   float64 `json:"ms"`
	Up   bool    `json:"up"` // client to server
	Type byte    `json:"ty"` // 22 handshake, 23 application data, 20 ccs
	N    int     `json:"n"`  // header + payload
}

// A Trace is one connection.
type Trace struct {
	Label  string   `json:"label"`  // "direct", "tunnel", "naive", ...
	Site   string   `json:"site"`   // what was fetched, for provenance
	Params string   `json:"params"` // shaping parameters in force, for provenance
	Recs   []Record `json:"recs"`
}

// Up/Down byte totals over the whole trace.
func (t *Trace) Bytes() (up, down int) {
	for _, r := range t.Recs {
		if r.Up {
			up += r.N
		} else {
			down += r.N
		}
	}
	return
}

// recorder collects records from both directions of one connection.
type recorder struct {
	mu    sync.Mutex
	t0    time.Time
	recs  []Record
	sink  *Sink
	label string
	site  string
	par   string
	done  bool
}

func (r *recorder) add(up bool, ty byte, n int) {
	r.mu.Lock()
	r.recs = append(r.recs, Record{Ms: float64(time.Since(r.t0).Microseconds()) / 1000, Up: up, Type: ty, N: n})
	r.mu.Unlock()
}

func (r *recorder) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return
	}
	r.done = true
	if r.sink != nil && len(r.recs) > 0 {
		r.sink.Write(&Trace{Label: r.label, Site: r.site, Params: r.par, Recs: r.recs})
	}
}

// splitter turns a byte stream into TLS records by reading the plaintext
// 5-byte headers. A censor does the same; nothing here needs the keys.
type splitter struct {
	rec  *recorder
	up   bool
	hdr  [5]byte
	have int
	need int
	ty   byte
}

func (s *splitter) feed(p []byte) {
	for len(p) > 0 {
		if s.need == 0 {
			n := copy(s.hdr[s.have:], p)
			s.have += n
			p = p[n:]
			if s.have < 5 {
				return
			}
			s.ty = s.hdr[0]
			s.need = int(binary.BigEndian.Uint16(s.hdr[3:5]))
			s.have = 0
			if s.need == 0 {
				s.rec.add(s.up, s.ty, 5)
			}
			continue
		}
		n := s.need
		if n > len(p) {
			n = len(p)
		}
		s.need -= n
		p = p[n:]
		if s.need == 0 {
			s.rec.add(s.up, s.ty, 5+int(binary.BigEndian.Uint16(s.hdr[3:5])))
		}
	}
}

// Tap wraps a TCP connection under TLS and records every record that crosses
// it. It changes no bytes: reads and writes pass straight through.
type Tap struct {
	net.Conn
	rec *recorder
	rs  *splitter
	ws  *splitter
	rmu sync.Mutex
	wmu sync.Mutex
}

// NewTap starts recording conn into sink. label/site/params are provenance
// written with the trace. Close flushes.
func NewTap(conn net.Conn, sink *Sink, label, site, params string) *Tap {
	r := &recorder{t0: time.Now(), sink: sink, label: label, site: site, par: params}
	return &Tap{Conn: conn, rec: r, rs: &splitter{rec: r, up: false}, ws: &splitter{rec: r, up: true}}
}

func (t *Tap) Read(p []byte) (int, error) {
	n, err := t.Conn.Read(p)
	if n > 0 {
		t.rmu.Lock()
		t.rs.feed(p[:n])
		t.rmu.Unlock()
	}
	if err != nil {
		t.rec.flush()
	}
	return n, err
}

func (t *Tap) Write(p []byte) (int, error) {
	n, err := t.Conn.Write(p)
	if n > 0 {
		t.wmu.Lock()
		t.ws.feed(p[:n])
		t.wmu.Unlock()
	}
	return n, err
}

func (t *Tap) Close() error {
	t.rec.flush()
	return t.Conn.Close()
}

// Sink appends traces to a JSON-lines file, one trace per line.
type Sink struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

func Create(path string) (*Sink, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &Sink{f: f, w: bufio.NewWriterSize(f, 1<<20)}, nil
}

func (s *Sink) Write(t *Trace) {
	b, err := json.Marshal(t)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.w.Write(b)
	s.w.WriteByte('\n')
	s.w.Flush() // a trace per connection is rare enough to land at once
	s.mu.Unlock()
}

func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.w.Flush()
	return s.f.Close()
}

// ReadTraces loads a JSON-lines trace file.
func ReadTraces(path string) ([]*Trace, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []*Trace
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var t Trace
		if err := json.Unmarshal(sc.Bytes(), &t); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, sc.Err()
}

// hook lets the relay dialler wrap its TCP connection when capture is on.
var hook atomic.Value

func init() { hook.Store((func(net.Conn, string) net.Conn)(nil)) }

// SetDialHook installs a wrapper applied to every raw relay connection. Used
// only by the capture tool; nil in production.
func SetDialHook(f func(net.Conn, string) net.Conn) { hook.Store(f) }

// WrapDial applies the installed hook, or returns c unchanged.
func WrapDial(c net.Conn, site string) net.Conn {
	if f, _ := hook.Load().(func(net.Conn, string) net.Conn); f != nil {
		return f(c, site)
	}
	return c
}

// TapRecords returns what a tap has recorded so far (tests and tools).
func TapRecords(t *Tap) []Record {
	t.rec.mu.Lock()
	defer t.rec.mu.Unlock()
	return append([]Record{}, t.rec.recs...)
}
