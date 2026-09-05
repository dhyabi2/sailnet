package client

import (
	"context"
	"log"
	"math/big"
	"sync"
	"time"

	"github.com/dhyabi2/sail/nano"
	"github.com/dhyabi2/sail/token"
)

// Funding is what an app needs to know before it offers to connect: whether
// this wallet can pay for a circuit, and if not, what is being done about it.
type Funding struct {
	Address    string `json:"address"`
	Balance    string `json:"balance"`    // XNO, "" when the ledger could not be read
	NeedsFunds bool   `json:"needsFunds"` // true while the wallet cannot pay for a circuit
	Required   string `json:"required"`   // XNO needed to connect
	Faucet     string `json:"faucet"`     // what the free trial did, for the screen
	Err        string `json:"error"`      // why the balance is unknown, if it is
}

// AnchorXNO is what a client prepays an entry relay to open a circuit. It is
// the amount a wallet must hold before connecting is possible at all.
const AnchorXNO = "0.0005"

var (
	fundMu     sync.Mutex
	lastFund   Funding
	lastFundAt time.Time
	faucetTry  time.Time // shared by every caller, so one start makes one claim
)

// faucetAllowed rate-limits ourselves before the faucet has to. The startup
// check and the empty-wallet watch both want to claim, and asking twice for
// the same wallet within seconds only earns a refusal worth nobody's time.
func faucetAllowed() bool {
	fundMu.Lock()
	defer fundMu.Unlock()
	if time.Since(faucetTry) < 10*time.Minute {
		return false
	}
	faucetTry = time.Now()
	return true
}

// EnsureFunded reads the wallet's balance and, when it cannot pay for a
// circuit, asks the faucet for the free trial.
//
// It runs before anything else an app does, and without starting a tunnel,
// so a new user never meets a Connect button that cannot work: either the
// wallet is funded and connecting is offered, or the app says plainly that
// it is waiting for XNO and shows where to send it. Nothing here is manual.
//
// Every failure is answered, never raised: no ledger, no faucet, a faucet
// talking nonsense — all of it leaves the app on the "waiting for funds"
// screen rather than in an error state, because that is the truth of the
// situation and the wallet may still be funded by hand.
func EnsureFunded(ctx context.Context) (f Funding) {
	defer func() {
		if r := recover(); r != nil {
			f = Funding{Required: AnchorXNO, NeedsFunds: true, Err: "could not check the wallet"}
		}
		fundMu.Lock()
		lastFund, lastFundAt = f, time.Now()
		fundMu.Unlock()
	}()

	f.Required = AnchorXNO
	key := EnsureWallet()
	f.Address = key.Address
	nc := newNano()
	acct := &nano.Account{Key: key, Client: nc, State: chainState(key)}

	// Anything sent to this wallet and not yet pocketed is money it has.
	if n, err := acct.ReceiveAll(ctx); err == nil && n > 0 {
		log.Printf("received %d pending payment(s)", n)
	}
	readBalance(ctx, nc, key, &f)
	if !f.NeedsFunds {
		return f
	}

	// It cannot pay: ask for the free trial. A refusal is normal (already
	// claimed today, or from this address) and is reported, not treated as
	// a failure of the app.
	if !faucetAllowed() {
		f.Faucet = "waiting for the free trial to arrive"
		return f
	}
	if r, err := ClaimFaucet(ctx, nc.HTTP, key.Address); err != nil {
		f.Faucet = "the free trial is unavailable right now; send " + AnchorXNO + " XNO to the address above to connect"
		log.Printf("faucet: %v", err)
	} else if r != nil && r.OK {
		f.Faucet = "free trial on its way; connecting as soon as it arrives"
		// Give it a moment to land, then look again, so a first-time user
		// usually goes straight from opening the app to being able to connect.
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(2 * time.Second)
			if n, err := acct.ReceiveAll(ctx); err == nil && n > 0 {
				log.Printf("received %d pending payment(s)", n)
			}
			if readBalance(ctx, nc, key, &f); !f.NeedsFunds {
				f.Faucet = "free trial received"
				return f
			}
		}
	} else {
		msg := "the free trial was declined"
		if r != nil && r.Error != "" {
			msg = r.Error
		}
		f.Faucet = msg + "; send " + AnchorXNO + " XNO to the address above to connect"
	}
	return f
}

// readBalance fills in the balance and whether it is enough to connect.
func readBalance(ctx context.Context, nc *nano.Client, key *nano.Key, f *Funding) {
	if info, ok, err := nc.AccountInfo(ctx, key.Address); err != nil {
		f.Err = "could not reach the ledger to read the balance"
	} else if ok {
		cacheAccountInfo(chainState(key), info)
	}
	anchor, err := token.ParseXNO(AnchorXNO)
	if err != nil {
		anchor = new(big.Int)
	}
	if _, bal, _, _, cached := chainState(key).Get(); cached {
		f.Balance = token.FormatXNO(bal)
		f.NeedsFunds = anchor != nil && bal.Cmp(anchor) < 0
		f.Err = ""
		return
	}
	// Nothing known: an unopened account has no balance, which is the same
	// as not being able to pay.
	f.Balance, f.NeedsFunds = "0", true
}

// LastFunding returns the most recent funding check without doing any work,
// so a screen can be redrawn without asking the ledger again.
func LastFunding() (Funding, time.Time) {
	fundMu.Lock()
	defer fundMu.Unlock()
	return lastFund, lastFundAt
}
