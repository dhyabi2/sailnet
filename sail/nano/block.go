package nano

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"

	"golang.org/x/crypto/blake2b"
)

// Block is a Nano state block.
type Block struct {
	Account        string
	Previous       [32]byte
	Representative [32]byte // raw public key (may be a SAIL op)
	Balance        *big.Int
	Link           [32]byte
	Signature      []byte
	Work           string
}

var statePreamble = func() [32]byte { var p [32]byte; p[31] = 6; return p }()

// Hash computes the state-block hash.
func (b *Block) Hash() ([32]byte, error) {
	pk, err := AddressToPubkey(b.Account)
	if err != nil {
		return [32]byte{}, err
	}
	h, _ := blake2b.New256(nil)
	h.Write(statePreamble[:])
	h.Write(pk[:])
	h.Write(b.Previous[:])
	h.Write(b.Representative[:])
	var bal [16]byte
	if b.Balance.Sign() < 0 || b.Balance.BitLen() > 128 {
		return [32]byte{}, errors.New("balance out of range")
	}
	b.Balance.FillBytes(bal[:])
	h.Write(bal[:])
	h.Write(b.Link[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

// JSON renders the block for the `process` RPC.
func (b *Block) JSON() map[string]any {
	return map[string]any{
		"type":           "state",
		"account":        b.Account,
		"previous":       strings.ToUpper(hex.EncodeToString(b.Previous[:])),
		"representative": PubkeyToAddress(b.Representative),
		"balance":        b.Balance.String(),
		"link":           strings.ToUpper(hex.EncodeToString(b.Link[:])),
		"signature":      strings.ToUpper(hex.EncodeToString(b.Signature)),
		"work":           b.Work,
	}
}

// Account wraps a key with a client to build and publish blocks.
type Account struct {
	Key    *Key
	Client *Client
	State  *ChainState // optional cache: lets the account sign blocks while no node is reachable
}

// PublishError reports a block that was signed (and its hash and content are
// valid) but could not be handed to a Nano node. A relay that receives the
// block as payment publishes it itself.
type PublishError struct{ Err error }

func (e *PublishError) Error() string { return "publish: " + e.Err.Error() }
func (e *PublishError) Unwrap() error { return e.Err }

func hexTo32(s string) ([32]byte, error) {
	var out [32]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return out, fmt.Errorf("bad hash %q", s)
	}
	copy(out[:], b)
	return out, nil
}

// frontier returns (previous, balance, representative, opened).
func (a *Account) frontier(ctx context.Context) ([32]byte, *big.Int, [32]byte, bool, error) {
	info, ok, err := a.Client.AccountInfo(ctx, a.Key.Address)
	if err != nil {
		// No node reachable: fall back to what we recorded after our last block.
		if prev, bal, rep, opened, cached := a.State.Get(); cached {
			return prev, bal, rep, opened, nil
		}
		return [32]byte{}, nil, [32]byte{}, false, err
	}
	if !ok {
		return [32]byte{}, new(big.Int), [32]byte{}, false, nil
	}
	prev, err := hexTo32(info.Frontier)
	if err != nil {
		return [32]byte{}, nil, [32]byte{}, false, err
	}
	bal, _ := new(big.Int).SetString(info.Balance, 10)
	rep, err := AddressToPubkey(info.Representative)
	if err != nil {
		return [32]byte{}, nil, [32]byte{}, false, err
	}
	a.State.Set(prev, bal, rep, true)
	return prev, bal, rep, true, nil
}

// work obtains PoW for root: the cache first (filled ahead of time by
// PrecomputeWork), then the RPC, then the CPU as a last resort.
func (a *Account) work(ctx context.Context, root [32]byte, threshold uint64) (string, error) {
	if w, ok := CachedWork(root, threshold); ok {
		return w, nil
	}
	rootHex := strings.ToUpper(hex.EncodeToString(root[:]))
	if w, err := a.Client.WorkGenerate(ctx, rootHex, threshold); err == nil && ValidWork(root, w, threshold) {
		RememberWork(root, w)
		return w, nil
	}
	if !AllowCPUWork {
		return "", errors.New("proof-of-work service unavailable; retry later")
	}
	w := GenerateWorkCPU(root, threshold)
	RememberWork(root, w)
	return w, nil
}

// AllowCPUWork lets an account brute-force proof-of-work on the local CPU
// when no work service answers. Clients keep it on (a phone has nothing else
// to do); a relay turns it off, because minutes of a saturated core would
// stall every circuit through it, and its sends all have retry loops.
var AllowCPUWork = true

// One block at a time per account: concurrent sends from the same account
// would race on the frontier and fork.
var acctLocks sync.Map

// Lock serialises block publishing for this account; returns the unlock func.
func (a *Account) Lock() func() {
	m, _ := acctLocks.LoadOrStore(a.Key.Address, &sync.Mutex{})
	m.(*sync.Mutex).Lock()
	return m.(*sync.Mutex).Unlock
}

// signAndPublish fills work + signature and processes the block.
func (a *Account) signAndPublish(ctx context.Context, b *Block, subtype string, root [32]byte, threshold uint64) (string, error) {
	h, err := b.Hash()
	if err != nil {
		return "", err
	}
	b.Signature = a.Key.Sign(h[:])
	if b.Work, err = a.work(ctx, root, threshold); err != nil {
		return "", err
	}
	hash := strings.ToUpper(hex.EncodeToString(h[:]))
	_, err = a.Client.Process(ctx, b, subtype)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "old block") {
			// A previous attempt reached the network (the first endpoint timed out
			// after accepting): the block is ours and it is in. Not a failure.
			a.State.Set(h, b.Balance, b.Representative, true)
			return hash, nil
		}
		if subtype == "send" && !isSemantic(err) {
			// Signed and valid; the caller may hand the block to a relay to publish.
			a.State.Set(h, b.Balance, b.Representative, true)
			return hash, &PublishError{err}
		}
		return "", err
	}
	a.State.Set(h, b.Balance, b.Representative, true)
	a.PrecomputeWork(h) // the next block's root is this block's hash
	return hash, nil
}

// isSemantic reports whether the node understood the request and rejected
// the block (fork, old block, bad work): not a transport failure.
func isSemantic(err error) bool {
	var re *RPCError
	return errors.As(err, &re)
}

// Send publishes a send of `amount` raw to `to`, with an explicit representative
// (pass the current one, or a SAIL op-encoded key). Returns block hash.
func (a *Account) Send(ctx context.Context, to string, amount *big.Int, rep *[32]byte) (string, error) {
	defer a.Lock()()
	prev, bal, curRep, opened, err := a.frontier(ctx)
	if err != nil {
		return "", err
	}
	if !opened {
		return "", errors.New("account not opened (receive some XNO first)")
	}
	if bal.Cmp(amount) < 0 {
		return "", fmt.Errorf("insufficient XNO: have %s raw, need %s", bal, amount)
	}
	link, err := AddressToPubkey(to)
	if err != nil {
		return "", err
	}
	if rep == nil {
		rep = &curRep
		if TaggedRep(curRep) {
			// The last block carried a Sailnet op in its representative; a plain
			// send must not inherit it, or it would read as that op forever.
			neutral := a.Key.Public
			rep = &neutral
		}
	}
	b := &Block{Account: a.Key.Address, Previous: prev, Representative: *rep, Balance: new(big.Int).Sub(bal, amount), Link: link}
	return a.signAndPublish(ctx, b, "send", prev, SendThreshold)
}

// TaggedRep reports whether a representative field carries a Sailnet op
// ("SA" magic + version byte) rather than a real representative.
func TaggedRep(rep [32]byte) bool { return rep[0] == 0x53 && rep[1] == 0x41 && rep[2] == 1 }

// SendBlock is Send with the current representative, returning the signed block too.
func (a *Account) SendBlock(ctx context.Context, to string, amount *big.Int) (string, *Block, error) {
	defer a.Lock()()
	prev, bal, rep, opened, err := a.frontier(ctx)
	if err != nil {
		return "", nil, err
	}
	if !opened {
		return "", nil, errors.New("account not opened (receive some XNO first)")
	}
	if bal.Cmp(amount) < 0 {
		return "", nil, fmt.Errorf("insufficient XNO: have %s raw, need %s", bal, amount)
	}
	link, err := AddressToPubkey(to)
	if err != nil {
		return "", nil, err
	}
	if TaggedRep(rep) {
		rep = a.Key.Public // never let a payment inherit an op-tagged representative
	}
	b := &Block{Account: a.Key.Address, Previous: prev, Representative: rep, Balance: new(big.Int).Sub(bal, amount), Link: link}
	h, err := a.signAndPublish(ctx, b, "send", prev, SendThreshold)
	return h, b, err // on *PublishError h and b are still valid: the relay publishes
}

// Receive pockets one receivable send (hash) of `amount` raw.
func (a *Account) Receive(ctx context.Context, sendHash string, amount *big.Int) (string, error) {
	defer a.Lock()()
	prev, bal, rep, opened, err := a.frontier(ctx)
	if err != nil {
		return "", err
	}
	link, err := hexTo32(sendHash)
	if err != nil {
		return "", err
	}
	root := prev
	if !opened {
		rep = a.Key.Public // self-represent on open; wallets may change later
		root = a.Key.Public
	}
	b := &Block{Account: a.Key.Address, Previous: prev, Representative: rep, Balance: new(big.Int).Add(bal, amount), Link: link}
	subtype := "receive"
	if !opened {
		subtype = "open"
	}
	return a.signAndPublish(ctx, b, subtype, root, ReceiveThreshold)
}

// ReceiveAll pockets every receivable. Returns number received.
func (a *Account) ReceiveAll(ctx context.Context) (int, error) {
	rs, err := a.Client.Receivables(ctx, a.Key.Address, 100)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rs {
		if _, err := a.Receive(ctx, r.Hash, r.Amount); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ChangeRep publishes a change block (used by NOP to restore a real representative).
func (a *Account) ChangeRep(ctx context.Context, rep [32]byte) (string, error) {
	defer a.Lock()()
	prev, bal, _, opened, err := a.frontier(ctx)
	if err != nil {
		return "", err
	}
	if !opened {
		return "", errors.New("account not opened")
	}
	b := &Block{Account: a.Key.Address, Previous: prev, Representative: rep, Balance: bal}
	return a.signAndPublish(ctx, b, "change", prev, SendThreshold)
}

// SendRaw publishes a send of `amount` raw whose link is a raw 32-byte value
// (HodlGame data blocks: 1 raw to an op-encoded pseudo-account).
func (a *Account) SendRaw(ctx context.Context, link [32]byte, amount *big.Int) (string, error) {
	h, _, err := a.SendRawBlock(ctx, link, amount)
	return h, err
}

// SendRawBlock is SendRaw returning the signed block as well.
func (a *Account) SendRawBlock(ctx context.Context, link [32]byte, amount *big.Int) (string, *Block, error) {
	defer a.Lock()()
	prev, bal, rep, opened, err := a.frontier(ctx)
	if err != nil {
		return "", nil, err
	}
	if !opened {
		return "", nil, errors.New("account not opened (receive some XNO first)")
	}
	if bal.Cmp(amount) < 0 {
		return "", nil, fmt.Errorf("insufficient XNO: have %s raw, need %s", bal, amount)
	}
	b := &Block{Account: a.Key.Address, Previous: prev, Representative: rep, Balance: new(big.Int).Sub(bal, amount), Link: link}
	h, err := a.signAndPublish(ctx, b, "send", prev, SendThreshold)
	return h, b, err // on *PublishError h and b are still valid: the relay publishes
}

// BlockFromJSON parses a block in `process` JSON shape (what JSON() emits).
func BlockFromJSON(m map[string]any) (*Block, error) {
	str := func(k string) string { v, _ := m[k].(string); return v }
	b := &Block{Account: str("account"), Work: str("work")}
	var err error
	if b.Previous, err = hexTo32(str("previous")); err != nil {
		return nil, err
	}
	if b.Link, err = hexTo32(str("link")); err != nil {
		return nil, err
	}
	if b.Representative, err = AddressToPubkey(str("representative")); err != nil {
		return nil, err
	}
	bal, ok := new(big.Int).SetString(str("balance"), 10)
	if !ok {
		return nil, errors.New("bad balance")
	}
	b.Balance = bal
	if b.Signature, err = hex.DecodeString(str("signature")); err != nil || len(b.Signature) != 64 {
		return nil, errors.New("bad signature")
	}
	return b, nil
}

// VerifySigned checks the block's signature against its account key.
func (b *Block) VerifySigned() bool {
	pk, err := AddressToPubkey(b.Account)
	if err != nil {
		return false
	}
	h, err := b.Hash()
	if err != nil {
		return false
	}
	return Verify(pk, h[:], b.Signature)
}
