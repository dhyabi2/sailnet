package client

import (
	"context"
	"log"
	"math/big"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/dhyabi2/sail/nano"
	"github.com/dhyabi2/sail/relay"
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

// AnchorXNO is the least a client ever prepays an entry relay to open a
// circuit: the floor under the amount, not the amount itself. What is
// actually paid is whatever buys AnchorBytes at the entry relay's own
// published price (see AnchorFor), because an anchor is a purchase of
// service and the price of service is not ours to fix.
//
// It used to be the whole story, a single number compiled into every app.
// That worked only for as long as the price never moved: a relay charging
// ten times more sold a tenth as much for the same anchor, the circuit ran
// out in seconds, and the app spent its session paying anchors instead of
// carrying traffic. Relays grant a minimum quota to cover exactly that case
// for apps already installed (Server.MinCredit); this is the fix on our side.
const AnchorXNO = "0.0005"

// AnchorBytes is the service one anchor buys: enough to open a page, see the
// tunnel work and reach the first top-up, which is then sized from the
// circuit's measured rate.
const AnchorBytes = 10 << 20

// AnchorFor is what to prepay a relay charging rate (in REGISTER rate units)
// for one anchor, never below AnchorXNO.
func AnchorFor(rate uint32) *big.Int {
	floor, err := token.ParseXNO(AnchorXNO)
	if err != nil || floor == nil {
		floor = new(big.Int)
	}
	if rate == 0 {
		return floor
	}
	amount := relay.RawFor(AnchorBytes, token.RateToRaw(rate))
	if amount.Cmp(floor) < 0 {
		return floor
	}
	return amount
}

// NetworkRate is the median price relays are asking, read from the registry
// cache on disk, or 0 when nothing is cached yet. It is what an app can know
// about the price before it has built anything.
func NetworkRate() uint32 {
	reg := &relay.Registry{CacheFile: filepath.Join(dataDir(), "registry.json")}
	if reg.LoadCache() == 0 {
		return 0
	}
	var rates []int
	for _, r := range reg.All() {
		if r.MinRate > 0 && !r.Unlisted {
			rates = append(rates, int(r.MinRate))
		}
	}
	if len(rates) == 0 {
		return 0
	}
	sort.Ints(rates)
	return uint32(rates[len(rates)/2])
}

// RequiredXNO is what a wallet must hold before connecting can work: one
// anchor at the going rate. Before any relay list has been seen it is the
// floor, which is also what the faucet pays, so a first run is never blocked
// by a price nobody has told us yet.
func RequiredXNO() *big.Int { return AnchorFor(NetworkRate()) }

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
			f = Funding{Required: token.FormatXNO(RequiredXNO()), NeedsFunds: true, Err: "could not check the wallet"}
		}
		fundMu.Lock()
		lastFund, lastFundAt = f, time.Now()
		fundMu.Unlock()
	}()

	required := RequiredXNO()
	f.Required = token.FormatXNO(required)
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
		f.Faucet = "the free trial is unavailable right now; send " + f.Required + " XNO to the address above to connect"
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
		f.Faucet = msg + "; send " + f.Required + " XNO to the address above to connect"
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
	anchor := RequiredXNO()
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
