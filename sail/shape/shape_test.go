package shape

import (
	"bytes"
	"crypto/rand"
	"net"
	"testing"
	"time"
)

// fake ChunkWriter that records the wire size of every record.
type recW struct {
	sizes []int
	buf   bytes.Buffer
}

func (r *recW) WriteChunk(p []byte) error {
	r.sizes = append(r.sizes, r.WireSize(len(p)))
	r.buf.Write(p)
	return nil
}
func (r *recW) WireSize(n int) int { return n + 22 }

func TestFrontWindowReplaysProfileSizes(t *testing.T) {
	pro := &Profile{K: 3, Prefixes: [][]PRec{{
		{Up: true, N: 300}, {Up: false, N: 4000}, {Up: true, N: 1200},
	}}, Sizes: []int{500}}
	p := Default()
	p.Profile = pro
	p.FrontK = 3
	Set(p)
	defer Set(Default())

	s := NewShaper(true)
	w := &recW{}
	if err := s.Write(w, bytes.Repeat([]byte("x"), 50), PaddingSample); err != nil {
		t.Fatal(err)
	}
	if len(w.sizes) == 0 {
		t.Fatal("nothing written")
	}
	// The client only shapes its own direction, so it replays 300 and 1200.
	if w.sizes[0] != 300 {
		t.Fatalf("first record %d, want the profile's 300", w.sizes[0])
	}
	if len(w.sizes) < 2 || w.sizes[1] != 1200 {
		t.Fatalf("records %v, want the second to be the profile's 1200", w.sizes)
	}
	if s.Padding == 0 {
		t.Fatal("50 bytes of payload cannot fill 1500 bytes of records without padding")
	}
}

func TestShaperCarriesEveryByte(t *testing.T) {
	p := Default()
	p.FrontK = 0
	Set(p)
	defer Set(Default())
	s := NewShaper(false)
	w := &recW{}
	payload := make([]byte, 40000)
	rand.Read(payload)
	if err := s.Write(w, payload, PaddingSample); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w.buf.Bytes(), payload) {
		t.Fatal("shaping changed the byte stream")
	}
	if len(w.sizes) < 2 {
		t.Fatalf("40 KiB went out as %d records; the cutter did nothing", len(w.sizes))
	}
}

// PaddingSample stands in for wire.PaddingCell in tests (shape must not
// import wire).
func PaddingSample() []byte { return make([]byte, 1024) }

func TestTapParsesRecords(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	sink := &Sink{}
	tap := NewTap(a, nil, "test", "", "")
	_ = sink
	go func() {
		// two application-data records of 100 and 200 payload bytes
		for _, n := range []int{100, 200} {
			hdr := []byte{23, 3, 3, byte(n >> 8), byte(n)}
			tap.Write(append(hdr, make([]byte, n)...))
		}
	}()
	buf := make([]byte, 4096)
	total := 0
	b.SetReadDeadline(time.Now().Add(2 * time.Second))
	for total < 310 {
		n, err := b.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		total += n
	}
	for i := 0; i < 100; i++ { // the writer records after Write returns
		tap.rec.mu.Lock()
		n := len(tap.rec.recs)
		tap.rec.mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	tap.rec.mu.Lock()
	defer tap.rec.mu.Unlock()
	if len(tap.rec.recs) != 2 {
		t.Fatalf("saw %d records, want 2: %v", len(tap.rec.recs), tap.rec.recs)
	}
	if tap.rec.recs[0].N != 105 || tap.rec.recs[1].N != 205 {
		t.Fatalf("record sizes %d,%d want 105,205", tap.rec.recs[0].N, tap.rec.recs[1].N)
	}
	if !tap.rec.recs[0].Up {
		t.Fatal("writes must be recorded as the client direction")
	}
}

func TestHandshakeRuleFindsEncapsulatedHandshake(t *testing.T) {
	// The shape Xue et al. describe: inner ClientHello, inner certificate
	// burst, inner Finished.
	tr := &Trace{Recs: []Record{
		{Ms: 0, Up: true, Type: 23, N: 517},
		{Ms: 40, Up: false, Type: 23, N: 4000},
		{Ms: 41, Up: false, Type: 23, N: 1200},
		{Ms: 45, Up: true, Type: 23, N: 80},
	}}
	if !RuleDetect(tr) {
		t.Fatal("the published rule must fire on a textbook encapsulated handshake")
	}
	plain := &Trace{Recs: []Record{
		{Ms: 0, Up: true, Type: 23, N: 600},
		{Ms: 30, Up: false, Type: 23, N: 900},
		{Ms: 31, Up: true, Type: 23, N: 700},
	}}
	if RuleDetect(plain) {
		t.Fatal("the rule fired on an ordinary request/response exchange")
	}
}

func TestForestSeparatesSeparableData(t *testing.T) {
	var data []Sample
	for i := 0; i < 200; i++ {
		data = append(data, Sample{X: []float64{float64(i), 1}, Y: 0})
		data = append(data, Sample{X: []float64{float64(i + 1000), 1}, Y: 1})
	}
	f := TrainForest(data, 40, 6, 2, newRand(1))
	if f.Score([]float64{5, 1}) > 0.2 || f.Score([]float64{1005, 1}) < 0.8 {
		t.Fatalf("forest did not learn a trivial split: %v %v", f.Score([]float64{5, 1}), f.Score([]float64{1005, 1}))
	}
}
