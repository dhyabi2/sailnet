package nano

import (
	"context"
	"encoding/binary"
	"encoding/hex"

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

// cpuWorkSlot admits one CPU proof-of-work search at a time, process-wide.
// A search used to take every core, and on a relay several sends can reach
// it at once (REGISTER and DESCRIPTOR, a repricing that outlived the next
// one): the box was pinned for as long as they ran, and an operator switches
// off a relay that does that. Now they queue for one core, and a send whose
// context ends while it waits leaves the queue.
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

// GenerateWorkCPU brute-forces a nonce on one core, once the CPU work slot is
// free. Returns 16-hex work, or ctx's error if ctx ends first.
func GenerateWorkCPU(ctx context.Context, root [32]byte, threshold uint64) (string, error) {
	if err := takeCPUSlot(ctx); err != nil {
		return "", err
	}
	defer func() { <-cpuWorkSlot }()
	return searchWork(ctx, root, threshold)
}

// searchWork is the search itself; the caller holds the CPU work slot. ctx is
// checked every 64Ki nonces, a few milliseconds of hashing.
func searchWork(ctx context.Context, root [32]byte, threshold uint64) (string, error) {
	h, _ := blake2b.New(8, nil)
	var n [8]byte
	for nonce := uint64(0x1000); ; nonce++ {
		if nonce&0xffff == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		h.Reset()
		binary.LittleEndian.PutUint64(n[:], nonce)
		h.Write(n[:])
		h.Write(root[:])
		if binary.LittleEndian.Uint64(h.Sum(nil)) >= threshold {
			var out [8]byte
			binary.BigEndian.PutUint64(out[:], nonce)
			return hex.EncodeToString(out[:]), nil
		}
	}
}
