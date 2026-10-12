package nano

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"
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
	Rep      string `json:"rep"`
	Opened   bool   `json:"opened"`
	Height   uint64 `json:"height,omitempty"` // blocks in the account chain at this frontier; 0 = never learned
	At       int64  `json:"at,omitempty"`    // when the state was recorded; 0 = before it was recorded (older build)
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

// Set records the state after a block (its hash becomes the frontier). A
// block we published extends the account chain, so a height already known
// grows by one; a height never learned stays unknown. The time is stamped
// so a later reader can tell a just-written state from a stale one.
func (s *ChainState) Set(frontier [32]byte, bal *big.Int, rep [32]byte, opened bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	h := s.v.Height
	if h > 0 {
		h++
	}
	s.v = chainStateFile{Frontier: strings.ToUpper(hex.EncodeToString(frontier[:])), Balance: bal.String(), Rep: strings.ToUpper(hex.EncodeToString(rep[:])), Opened: opened, Height: h, At: time.Now().Unix()}
	data, _ := json.MarshalIndent(s.v, "", "  ")
	s.mu.Unlock()
	if s.Path != "" {
		os.WriteFile(s.Path, data, 0o600)
	}
}

// SetHeight records the chain height the frontier is at, as account_info
// reported it. It corrects the +1 Set assumed when the frontier came from
// the ledger rather than from a block we published ourselves.
func (s *ChainState) SetHeight(h uint64) {
	if s == nil || h == 0 {
		return
	}
	s.mu.Lock()
	s.v.Height = h
	data, _ := json.MarshalIndent(s.v, "", "  ")
	s.mu.Unlock()
	if s.Path != "" {
		os.WriteFile(s.Path, data, 0o600)
	}
}

// Height is the recorded chain length at the frontier; 0 = never learned.
func (s *ChainState) Height() uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.v.Height
}

// WrittenAt is when the state was recorded. The zero time means a cache
// written by an older build that kept no timestamp.
func (s *ChainState) WrittenAt() int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.v.At
}
