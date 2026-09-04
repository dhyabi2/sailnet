package mobile

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Per-flow metering for the app's Activity screen. Every TCP or UDP flow the
// tunnel carries is recorded by its source port and destination; the Android
// side asks the system which app owns the source port and sums the bytes
// per app. Nothing leaves the phone.

type flowStat struct {
	Proto    string `json:"proto"`
	SrcIP    string `json:"srcIp"`
	SrcPort  int    `json:"srcPort"`
	DstIP    string `json:"dstIp"`
	DstPort  int    `json:"dstPort"`
	Up       int64  `json:"up"`
	Down     int64  `json:"down"`
	Started  int64  `json:"started"` // unix seconds
	Last     int64  `json:"last"`
	Open     bool   `json:"open"`
	up, down atomic.Int64
	last     atomic.Int64
	open     atomic.Bool
}

var (
	flowsMu sync.Mutex
	flows   = map[string]*flowStat{}
)

// trackFlow registers a flow and returns its counters.
func trackFlow(proto, srcIP string, srcPort int, dstIP string, dstPort int) *flowStat {
	key := fmt.Sprintf("%s:%s:%d>%s:%d", proto, srcIP, srcPort, dstIP, dstPort)
	f := &flowStat{Proto: proto, SrcIP: srcIP, SrcPort: srcPort, DstIP: dstIP, DstPort: dstPort, Started: time.Now().Unix()}
	f.last.Store(time.Now().Unix())
	f.open.Store(true)
	flowsMu.Lock()
	flows[key] = f
	if len(flows) > 2000 { // forget the oldest closed flows
		type kv struct {
			k string
			t int64
		}
		var old []kv
		for k, v := range flows {
			if !v.open.Load() {
				old = append(old, kv{k, v.last.Load()})
			}
		}
		sort.Slice(old, func(i, j int) bool { return old[i].t < old[j].t })
		for i := 0; i < len(old)/2; i++ {
			delete(flows, old[i].k)
		}
	}
	flowsMu.Unlock()
	return f
}

func (f *flowStat) addUp(n int)   { f.up.Add(int64(n)); f.last.Store(time.Now().Unix()) }
func (f *flowStat) addDown(n int) { f.down.Add(int64(n)); f.last.Store(time.Now().Unix()) }
func (f *flowStat) close()        { f.open.Store(false) }

// meter counts bytes written through it.
type meter struct {
	w  io.Writer
	on func(int)
}

func (m meter) Write(p []byte) (int, error) {
	n, err := m.w.Write(p)
	if n > 0 {
		m.on(n)
	}
	return n, err
}

// Flows returns the flows seen in the last ten minutes as a JSON array,
// newest activity first.
func Flows() string {
	cut := time.Now().Add(-10 * time.Minute).Unix()
	flowsMu.Lock()
	out := make([]*flowStat, 0, len(flows))
	for _, f := range flows {
		if f.last.Load() >= cut {
			f.Up, f.Down, f.Last, f.Open = f.up.Load(), f.down.Load(), f.last.Load(), f.open.Load()
			out = append(out, f)
		}
	}
	flowsMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Last > out[j].Last })
	b, _ := json.Marshal(out)
	return string(b)
}
