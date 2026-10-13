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
	Rep      string `json:"representative"`
	Opened   bool   `json:"opened"`
	Height   uint64 `json:"height,omitempty"` // blocks in the account chain at this frontier; 0 = never learned
	At       int64  `json:"at,omitempty"`     // when the state was recorded; absent in a file an older build wrote
}

// LoadChainState reads the cache at path (missing file = empty state). A
// file an older build wrote carries no time, but that build rewrote it on
// every block, so its modification time says when the state was recorded.
func LoadChainState(path string) *ChainState {
	s := &ChainState{Path: path}
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &s.v)
		if s.v.At == 0 {
			if fi, err := os.Stat(path); err == nil {
				// A write never leaves an mtime at or before the epoch;
				// restore tools and tar do. Such a file is as old as a
				// file can be, so it is aged as the oldest known time.
				if s.v.At = fi.ModTime().Unix(); s.v.At <= 0 {
					s.v.At = 1
				}
			}
		}
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
	s.record(frontier, bal, rep, opened, 0, true)
}

// SetFromLedger records the state account_info reported, at the height it
// reported (0 = it gave none, so the height is unknown again). Nothing was
// published, so nothing is added: a ledger read never grows the chain. A
// read that agrees with the cached frontier never lowers the height already
// known for it either: the height at a frontier cannot shrink, so a lower
// count is a node that answered wrongly, and believing it would let a node
// lagging behind that frontier roll the cache back.
func (s *ChainState) SetFromLedger(frontier [32]byte, bal *big.Int, rep [32]byte, opened bool, height uint64) {
	if s == nil {
		return
	}
	s.record(frontier, bal, rep, opened, height, false)
}

// record writes the state; grow takes the height from the one it replaces,
// one block longer when it is known.
func (s *ChainState) record(frontier [32]byte, bal *big.Int, rep [32]byte, opened bool, height uint64, grow bool) {
	front := strings.ToUpper(hex.EncodeToString(frontier[:]))
	s.mu.Lock()
	if grow {
		if height = s.v.Height; height > 0 {
			height++
		}
	} else if strings.EqualFold(s.v.Frontier, front) && s.v.Height > height {
		height = s.v.Height
	}
	s.v = chainStateFile{Frontier: front, Balance: bal.String(), Rep: strings.ToUpper(hex.EncodeToString(rep[:])), Opened: opened, Height: height, At: time.Now().Unix()}
	data, _ := json.MarshalIndent(s.v, "", "  ")
	s.mu.Unlock()
	if s.Path != "" {
		os.WriteFile(s.Path, data, 0o600)
	}
}

// SetHeight records the chain height the frontier is at, as account_info
// reported it. It only ever raises the height: the frontier has not moved,
// and its height cannot shrink, so a lower count is a node's mistake.
func (s *ChainState) SetHeight(h uint64) {
	if s == nil || h == 0 {
		return
	}
	s.mu.Lock()
	if h <= s.v.Height {
		s.mu.Unlock()
		return
	}
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

// WrittenAt is when the state was recorded (for a file an older build wrote,
// its modification time). Zero means it is not known.
func (s *ChainState) WrittenAt() int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.v.At
}
