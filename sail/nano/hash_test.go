package nano

import (
	"context"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"
)

// Compares our local state-block hash with the node's block_hash RPC.
func TestBlockHashMatchesNode(t *testing.T) {
	seed := make([]byte, 32)
	seed[0] = 7
	k, _ := DeriveKey(seed, 0)
	link, _ := AddressToPubkey("nano_1cexacexqrr51coik9eeqzebmfej7g6chrd1eygmzm9ad86qek84pnhpda1t")
	var prev [32]byte
	prev[3] = 9
	b := &Block{Account: k.Address, Previous: prev, Representative: k.Public, Balance: big.NewInt(123456789), Link: link}
	h, err := b.Hash()
	if err != nil {
		t.Fatal(err)
	}
	b.Signature = k.Sign(h[:])
	if !Verify(k.Public, h[:], b.Signature) {
		t.Fatal("self-verify failed")
	}
	b.Work = "0000000000000000"
	var v struct{ Hash string }
	c := &Client{URLs: []string{FallbackRPC}, HTTP: NewClient().HTTP}
	if err := c.Call(context.Background(), map[string]any{"action": "block_hash", "json_block": true, "block": b.JSON()}, &v); err != nil {
		t.Skip("rpc unavailable:", err)
	}
	if !strings.EqualFold(v.Hash, hex.EncodeToString(h[:])) {
		t.Fatalf("hash mismatch: node %s local %x", v.Hash, h)
	}
	t.Log("hash ok", v.Hash)
	// work check: receive-difficulty CPU work must validate locally
	w := GenerateWorkCPU(h, ReceiveThreshold)
	if !ValidWork(h, w, ReceiveThreshold) {
		t.Fatal("cpu work invalid")
	}
}
