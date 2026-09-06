package relay

import (
	"context"
	"encoding/json"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dhyabi2/sail/nano"
	"github.com/dhyabi2/sail/token"
)

// Faucet hands out the registration amount (one anchor's worth of XNO) to
// new wallets: enough for a first circuit or a relay's REGISTER block. It is
// served on the relay's HTTPS listener at /faucet and reached through the
// website endpoint, which forwards the caller's public IP under a shared
// secret. Limits: PerIP claims per public IP per day, one claim per account
// per day. Every refusal names the amount, so a client that cannot be paid
// can tell its user exactly what to send.
type Faucet struct {
	Key    *nano.Key
	Nano   *nano.Client
	State  *nano.ChainState
	Amount *big.Int
	PerIP  int

	// A separate, larger grant so that someone opening the app for the first
	// time can simply use the network instead of having to buy XNO first. It
	// has its own wallet, so the money for trials can run out without taking
	// the registration grant with it, and its own per-address limit.
	TrialKey    *nano.Key
	TrialState  *nano.ChainState
	TrialAmount *big.Int
	TrialPerIP  int
	Secret      string // header X-Faucet-Secret from the forwarder; proves X-Forwarded-For
	File        string // counters survive restarts

	mu   sync.Mutex
	once sync.Once
	st   faucetState
}

type faucetState struct {
	Day      string           `json:"day"`
	IPs      map[string]int   `json:"ips"`
	Accounts map[string]int64 `json:"accounts"` // account → unix time of last claim
}

type faucetReply struct {
	OK          bool   `json:"ok"`
	Hash        string `json:"hash,omitempty"`
	Amount      string `json:"amount"` // XNO, always present: what a wallet needs
	Error       string `json:"error,omitempty"`
	RetryAfterS int    `json:"retryAfterSeconds,omitempty"`
}

func (f *Faucet) load() {
	f.once.Do(func() {
		f.st = faucetState{IPs: map[string]int{}, Accounts: map[string]int64{}}
		if f.File != "" {
			if data, err := os.ReadFile(f.File); err == nil {
				json.Unmarshal(data, &f.st)
			}
		}
		if f.st.IPs == nil {
			f.st.IPs = map[string]int{}
		}
		if f.st.Accounts == nil {
			f.st.Accounts = map[string]int64{}
		}
	})
}

func (f *Faucet) saveLocked() {
	if f.File == "" {
		return
	}
	data, _ := json.Marshal(f.st)
	os.WriteFile(f.File, data, 0o600)
}

// refused records a claim that was turned away. Somebody who opens an app
// and is told to fund a wallet by hand is the most expensive kind of user to
// lose, and until this existed a refusal left no trace anywhere: the wallet
// never opens on the ledger, so there was no way to tell "nobody asked" from
// "everybody asked and was turned away". The address is never logged — a
// relay does not keep a record of who talks to it.
func refused(trial bool, acct, why string) {
	kind := "faucet"
	if trial {
		kind = "trial"
	}
	log.Printf("%s: refused %s: %s", kind, short(acct), why)
}

func (f *Faucet) clientIP(r *http.Request) string {
	if f.Secret != "" && r.Header.Get("X-Faucet-Secret") == f.Secret {
		if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
			return strings.TrimSpace(strings.Split(xf, ",")[0])
		}
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip == "" {
		ip = r.RemoteAddr
	}
	return ip
}

func (f *Faucet) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.load()
	amount := token.FormatXNO(f.Amount)
	answer := func(code int, rep faucetReply) {
		if rep.Amount == "" {
			rep.Amount = amount
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(rep)
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "content-type")
		w.WriteHeader(204)
		return
	}
	if r.Method != http.MethodPost {
		answer(405, faucetReply{Error: "POST {\"account\":\"nano_...\"}; the faucet pays the registration amount once per account per day"})
		return
	}
	var req struct {
		Account string `json:"account"`
		Node    bool   `json:"node"` // a relay asking for its float: four claims' worth, so it can open pools at once
		Kind    string `json:"kind"` // "trial" for a first-run client; anything else is the registration grant
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		answer(400, faucetReply{Error: "bad request body"})
		return
	}
	acct := strings.TrimSpace(req.Account)
	if _, err := nano.AddressToPubkey(acct); err != nil || !strings.HasPrefix(acct, "nano_") {
		answer(400, faucetReply{Error: "not a nano_ address"})
		return
	}
	ip := f.clientIP(r)
	// Which faucet is being asked decides everything below: the two keep
	// separate wallets, separate per-IP counters and separate limits. They
	// used to share the registration counter for the first check, so an
	// address that had asked for enough node registrations was refused a
	// free trial as well — two unrelated things throttling each other.
	trial := req.Kind == "trial" && f.TrialKey != nil && f.TrialAmount != nil
	pay := new(big.Int).Set(f.Amount)
	claims, limit, key, ipKey := 1, f.PerIP, f.Key, ip
	state := f.State
	if trial {
		pay = new(big.Int).Set(f.TrialAmount)
		limit, key, ipKey, state = f.TrialPerIP, f.TrialKey, "trial|"+ip, f.TrialState
	} else if req.Node {
		claims = 4
		pay.Mul(pay, big.NewInt(4))
	}

	day := time.Now().UTC().Format("2006-01-02")
	f.mu.Lock()
	if f.st.Day != day {
		f.st.Day, f.st.IPs = day, map[string]int{}
	}
	if last := f.st.Accounts[acct]; time.Since(time.Unix(last, 0)) < 24*time.Hour {
		f.mu.Unlock()
		refused(trial, acct, "this wallet already claimed within the last day")
		answer(429, faucetReply{Error: "this wallet already received the registration amount today", RetryAfterS: int(24*time.Hour/time.Second) - int(time.Since(time.Unix(last, 0)).Seconds())})
		return
	}
	if f.st.IPs[ipKey]+claims > limit {
		f.mu.Unlock()
		refused(trial, acct, "this address has used its "+itoa(limit)+" claims for today")
		if trial {
			answer(429, faucetReply{Amount: token.FormatXNO(pay), Error: "this network address has had its " + itoa(limit) + " free trials; fund the wallet to keep going"})
			return
		}
		answer(429, faucetReply{Error: "this network address has used its " + itoa(limit) + " faucet claims for today; send the registration amount yourself or try tomorrow", RetryAfterS: secondsToMidnight()})
		return
	}
	f.st.IPs[ipKey] += claims // counted even if the send fails: no free retries against a dry faucet
	f.st.Accounts[acct] = time.Now().Unix()
	f.saveLocked()
	f.mu.Unlock()

	if f.Nano == nil || key == nil {
		// Misconfigured rather than out of money. Say so plainly instead of
		// dying inside the handler: an app is waiting on this answer.
		f.mu.Lock()
		f.st.IPs[ipKey] -= claims
		delete(f.st.Accounts, acct)
		f.saveLocked()
		f.mu.Unlock()
		log.Printf("faucet: asked for a grant it is not configured to pay")
		answer(503, faucetReply{Amount: token.FormatXNO(pay), Error: "this faucet is not configured; the amount must be sent to the wallet by hand"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	a := &nano.Account{Key: key, Client: f.Nano, State: state}
	a.ReceiveAll(ctx)
	h, err := a.Send(ctx, acct, pay, nil)
	if err != nil {
		msg := err.Error()
		if strings.Contains(strings.ToLower(msg), "insufficient") {
			msg = "faucet wallet is empty"
		}
		log.Printf("faucet: send failed: %v", err)
		f.mu.Lock()
		f.st.IPs[ipKey] -= claims
		delete(f.st.Accounts, acct)
		f.saveLocked()
		f.mu.Unlock()
		answer(503, faucetReply{Error: "faucet unavailable (" + msg + "): the registration amount of " + amount + " XNO must be sent to the wallet by hand"})
		return
	}
	log.Printf("faucet: %s XNO → %s (%s)", token.FormatXNO(pay), short(acct), h[:8])
	answer(200, faucetReply{OK: true, Hash: h, Amount: token.FormatXNO(pay)})
}

func itoa(n int) string { return big.NewInt(int64(n)).String() }

func secondsToMidnight() int {
	now := time.Now().UTC()
	next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	return int(next.Sub(now).Seconds())
}
