package nano

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"sync"
)

// ChainState is the wallet's own view of its account: frontier, balance and
// representative. It is what the account needs to sign its next block, so
// with it cached a client can pay a relay without asking any Nano node —
// nothing on the local network sees a Nano RPC call. The relay publishes
// the block. Kept on disk; refreshed whenever the ledger is reachable.
type ChainState struct {
	Path string
	mu   sync.Mutex
	v    chainStateFile
}

type chainStateFile struct {
	Frontier string `json:"frontier"`
	Balance  string `json:"balance"`
	Rep      string `json:"representative"`
	Opened   bool   `json:"opened"`
}

// LoadChainState reads the cache at path (missing file = empty state).
func LoadChainState(path string) *ChainState {
	s := &ChainState{Path: path}
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &s.v)
	}
	return s
}

// Get returns the cached frontier, balance, representative and whether the
// account is known to be opened; ok is false when nothing is cached.
func (s *ChainState) Get() (prev [32]byte, bal *big.Int, rep [32]byte, opened, ok bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.v.Frontier == "" {
		return
	}
	prev, _ = hexTo32(s.v.Frontier)
	rep, _ = hexTo32(s.v.Rep)
	bal, _ = new(big.Int).SetString(s.v.Balance, 10)
	if bal == nil {
		bal = new(big.Int)
	}
	return prev, bal, rep, s.v.Opened, true
}

// Set records the state after a block (its hash becomes the frontier).
func (s *ChainState) Set(frontier [32]byte, bal *big.Int, rep [32]byte, opened bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.v = chainStateFile{Frontier: strings.ToUpper(hex.EncodeToString(frontier[:])), Balance: bal.String(), Rep: strings.ToUpper(hex.EncodeToString(rep[:])), Opened: opened}
	data, _ := json.MarshalIndent(s.v, "", "  ")
	s.mu.Unlock()
	if s.Path != "" {
		os.WriteFile(s.Path, data, 0o600)
	}
}
