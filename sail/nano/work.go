package nano

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"runtime"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/blake2b"
)

// Difficulty thresholds (Nano v21+ epoch 2).
const (
	SendThreshold    uint64 = 0xfffffff800000000
	ReceiveThreshold uint64 = 0xfffffe0000000000
)

// WorkValue computes the 8-byte blake2b(nonce_le || root) value as uint64.
func WorkValue(root [32]byte, nonce uint64) uint64 {
	h, _ := blake2b.New(8, nil)
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], nonce)
	h.Write(n[:])
	h.Write(root[:])
	return binary.LittleEndian.Uint64(h.Sum(nil))
}

// ValidWork reports whether hex work satisfies threshold for root.
func ValidWork(root [32]byte, workHex string, threshold uint64) bool {
	b, err := hex.DecodeString(workHex)
	if err != nil || len(b) != 8 {
		return false
	}
	return WorkValue(root, binary.BigEndian.Uint64(b)) >= threshold
}

// cpuWorkSlot admits one relay-mode CPU proof-of-work search at a time,
// process-wide. A search used to take every core, and on a relay several
// sends can reach it at once (REGISTER and DESCRIPTOR, a repricing that
// outlived the next one): the box was pinned for as long as they ran, and an
// operator switches off a relay that does that. In relay mode (AllowCPUWork
// off, an account with AllowCPU) they queue for one core, and a send whose
// context ends while it waits leaves the queue. Clients (AllowCPUWork on)
// never touch it: their CPU work takes every core and is not serialised.
var cpuWorkSlot = make(chan struct{}, 1)

// takeCPUSlot waits for the CPU work slot; release it with <-cpuWorkSlot.
func takeCPUSlot(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case cpuWorkSlot <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil { // both were ready and select chose the slot
		<-cpuWorkSlot
		return err
	}
	return nil
}

// cpuWorkers is how many cores a CPU search may use: all of them for a
// client, one on a relay.
func cpuWorkers() int {
	if AllowCPUWork {
		return runtime.NumCPU()
	}
	return 1
}

// GenerateWorkCPU brute-forces a nonce on the local CPU: on every core when
// AllowCPUWork is on (a client), else on one core once the process-wide CPU
// work slot is free (a relay). Returns 16-hex work, or ctx's error if ctx
// ends first; a search cut off by ctx resumes where it stopped next time.
func GenerateWorkCPU(ctx context.Context, root [32]byte, threshold uint64) (string, error) {
	if AllowCPUWork {
		return searchWork(ctx, root, threshold, cpuWorkers())
	}
	if err := takeCPUSlot(ctx); err != nil {
		return "", err
	}
	defer func() { <-cpuWorkSlot }()
	// Another send may have found work for this root while we queued.
	if w, ok := CachedWork(root, threshold); ok {
		return w, nil
	}
	return searchWork(ctx, root, threshold, cpuWorkers())
}

// workProgress remembers, per root and threshold, the nonce below which a
// search cut off by its context found nothing, so the next search for that
// root starts there instead of at the beginning: a retry always makes
// progress, even for a root whose solution lies further than one deadline's
// worth of hashing. Entries go when work for the root is found or remembered.
var workProgress = struct {
	sync.Mutex
	m map[workProgressKey]uint64
}{m: map[workProgressKey]uint64{}}

type workProgressKey struct {
	root      [32]byte
	threshold uint64
}

const (
	workProgressMax = 64
	workStart       = uint64(0x1000)  // first nonce tried
	workChunk       = uint64(1 << 15) // nonces per worker between context checks
)

func loadWorkProgress(k workProgressKey) uint64 {
	workProgress.Lock()
	defer workProgress.Unlock()
	if n, ok := workProgress.m[k]; ok {
		return n
	}
	return workStart
}

func saveWorkProgress(k workProgressKey, next uint64) {
	workProgress.Lock()
	defer workProgress.Unlock()
	if next <= workProgress.m[k] {
		return // a concurrent search for the same root got further
	}
	if _, ok := workProgress.m[k]; !ok && len(workProgress.m) >= workProgressMax {
		workProgress.m = map[workProgressKey]uint64{}
	}
	workProgress.m[k] = next
}

// dropWorkProgress forgets every search position for root.
func dropWorkProgress(root [32]byte) {
	workProgress.Lock()
	defer workProgress.Unlock()
	for k := range workProgress.m {
		if k.root == root {
			delete(workProgress.m, k)
		}
	}
}

// searchWork is the search itself, on workers goroutines (in relay mode the
// caller holds the CPU work slot and passes 1). It scans in rounds of
// workers*workChunk nonces from where the last search for root stopped,
// checking ctx between rounds (a few milliseconds of hashing); a search ended
// by ctx records the start of the round it was in, so the next one resumes.
func searchWork(ctx context.Context, root [32]byte, threshold uint64, workers int) (string, error) {
	if workers < 1 {
		workers = 1
	}
	key := workProgressKey{root, threshold}
	base := loadWorkProgress(key)
	for {
		if err := ctx.Err(); err != nil {
			saveWorkProgress(key, base)
			return "", err
		}
		var found atomic.Uint64
		var done atomic.Bool
		if workers == 1 {
			scanWork(root, threshold, base, &found, &done)
		} else {
			var wg sync.WaitGroup
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func(start uint64) {
					defer wg.Done()
					scanWork(root, threshold, start, &found, &done)
				}(base + uint64(w)*workChunk)
			}
			wg.Wait()
		}
		if done.Load() {
			dropWorkProgress(root)
			var out [8]byte
			binary.BigEndian.PutUint64(out[:], found.Load())
			return hex.EncodeToString(out[:]), nil
		}
		base += uint64(workers) * workChunk
	}
}

// scanWork tries nonces [start, start+workChunk) and stops early once done is
// set by another worker.
func scanWork(root [32]byte, threshold, start uint64, found *atomic.Uint64, done *atomic.Bool) {
	h, _ := blake2b.New(8, nil)
	var n [8]byte
	var sum [8]byte
	for nonce := start; nonce < start+workChunk; nonce++ {
		if nonce&0x3ff == 0 && done.Load() {
			return
		}
		h.Reset()
		binary.LittleEndian.PutUint64(n[:], nonce)
		h.Write(n[:])
		h.Write(root[:])
		if binary.LittleEndian.Uint64(h.Sum(sum[:0])) >= threshold {
			if done.CompareAndSwap(false, true) {
				found.Store(nonce)
			}
			return
		}
	}
}
