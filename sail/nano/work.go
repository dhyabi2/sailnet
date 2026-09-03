package nano

import (
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

// GenerateWorkCPU brute-forces a nonce on all cores. Returns 16-hex work.
func GenerateWorkCPU(root [32]byte, threshold uint64) string {
	var found atomic.Uint64
	var done atomic.Bool
	var wg sync.WaitGroup
	workers := runtime.NumCPU()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(start uint64) {
			defer wg.Done()
			h, _ := blake2b.New(8, nil)
			var n [8]byte
			for nonce := start; !done.Load(); nonce += uint64(workers) {
				h.Reset()
				binary.LittleEndian.PutUint64(n[:], nonce)
				h.Write(n[:])
				h.Write(root[:])
				if binary.LittleEndian.Uint64(h.Sum(nil)) >= threshold {
					if done.CompareAndSwap(false, true) {
						found.Store(nonce)
					}
					return
				}
			}
		}(uint64(w) + 0x1000)
	}
	wg.Wait()
	var out [8]byte
	binary.BigEndian.PutUint64(out[:], found.Load())
	return hex.EncodeToString(out[:])
}
