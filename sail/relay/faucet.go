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
	Secret string // header X-Faucet-Secret from the forwarder; proves X-Forwarded-For
	File   string // counters survive restarts

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
	day := time.Now().UTC().Format("2006-01-02")
	f.mu.Lock()
	if f.st.Day != day {
		f.st.Day, f.st.IPs = day, map[string]int{}
	}
	if f.st.IPs[ip] >= f.PerIP {
		f.mu.Unlock()
		answer(429, faucetReply{Error: "this network address has used its " + itoa(f.PerIP) + " faucet claims for today; send the registration amount yourself or try tomorrow", RetryAfterS: secondsToMidnight()})
		return
	}
	if last := f.st.Accounts[acct]; time.Since(time.Unix(last, 0)) < 24*time.Hour {
		f.mu.Unlock()
		answer(429, faucetReply{Error: "this wallet already received the registration amount today", RetryAfterS: int(24*time.Hour/time.Second) - int(time.Since(time.Unix(last, 0)).Seconds())})
		return
	}
	pay := new(big.Int).Set(f.Amount)
	claims := 1
	if req.Node {
		claims = 4
		pay.Mul(pay, big.NewInt(4))
	}
	if f.st.IPs[ip]+claims > f.PerIP {
		f.mu.Unlock()
		answer(429, faucetReply{Error: "this network address has used its " + itoa(f.PerIP) + " faucet claims for today; send the registration amount yourself or try tomorrow", RetryAfterS: secondsToMidnight()})
		return
	}
	f.st.IPs[ip] += claims // counted even if the send fails: no free retries against a dry faucet
	f.st.Accounts[acct] = time.Now().Unix()
	f.saveLocked()
	f.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	a := &nano.Account{Key: f.Key, Client: f.Nano, State: f.State}
	a.ReceiveAll(ctx)
	h, err := a.Send(ctx, acct, pay, nil)
	if err != nil {
		msg := err.Error()
		if strings.Contains(strings.ToLower(msg), "insufficient") {
			msg = "faucet wallet is empty"
		}
		log.Printf("faucet: send failed: %v", err)
		f.mu.Lock()
		f.st.IPs[ip] -= claims
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
