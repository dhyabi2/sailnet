package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dhyabi2/sail/nano"
)

// FaucetURL pays the registration amount to a new wallet, once per account
// per day and ten times per public IP per day.
const FaucetURL = "https://www.sailnet.space/api/faucet"

// FaucetReply is the faucet's answer. Amount is always set: what a wallet
// needs when the faucet cannot pay, so the message can say so.
type FaucetReply struct {
	OK     bool   `json:"ok"`
	Hash   string `json:"hash"`
	Amount string `json:"amount"`
	Error  string `json:"error"`
}

// ClaimFaucet asks the faucet for the registration amount, through the
// given HTTP client (a relay uses a direct one; a client uses its stealth
// transport, which carries the request to its entry relay). A refusal comes
// back as an error that names the amount to send by hand.
func ClaimFaucet(ctx context.Context, hc *http.Client, account string) (*FaucetReply, error) {
	return claimFaucet(ctx, hc, account, "trial")
}

// ClaimFaucetNode is ClaimFaucet for a relay: it asks for four claims'
// worth in one go (0.002 XNO), the float a new relay needs to open pools
// to its peers before its own earnings arrive.
func ClaimFaucetNode(ctx context.Context, hc *http.Client, account string) (*FaucetReply, error) {
	return claimFaucet(ctx, hc, account, "node")
}

func claimFaucet(ctx context.Context, hc *http.Client, account string, kind string) (*FaucetReply, error) {
	body, _ := json.Marshal(map[string]any{"action": "faucet", "account": account, "node": kind == "node", "kind": kind})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, FaucetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("faucet unreachable (%v): send the registration amount to %s by hand", redactErr(err), account)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var fr FaucetReply
	if json.Unmarshal(data, &fr) != nil {
		return nil, fmt.Errorf("faucet answered HTTP %d without JSON: send the registration amount to %s by hand", resp.StatusCode, account)
	}
	if !fr.OK {
		amt := fr.Amount
		if amt == "" {
			amt = "0.0005"
		}
		return &fr, fmt.Errorf("faucet refused: %s (required registration amount: %s XNO to %s)", strings.TrimSpace(fr.Error), amt, account)
	}
	return &fr, nil
}

func redactErr(err error) string {
	return strings.ReplaceAll(err.Error(), FaucetURL, "faucet")
}

// FundFromFaucet claims the registration amount for key and waits for it
// to land, then pockets it. Used by a relay whose wallet is not opened yet.
func FundFromFaucet(ctx context.Context, hc *http.Client, nc *nano.Client, key *nano.Key) error {
	acct := &nano.Account{Key: key, Client: nc, State: chainState(key)}
	if n, err := acct.ReceiveAll(ctx); err == nil && n > 0 {
		return nil // something was already on its way (an earlier claim, or a hand payment)
	}
	fr, err := ClaimFaucetNode(ctx, hc, key.Address)
	if err != nil {
		if n, rerr := acct.ReceiveAll(ctx); rerr == nil && n > 0 {
			return nil // refused because a claim was already pending: it is in now
		}
		return err
	}
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if n, err := acct.ReceiveAll(ctx); err == nil && n > 0 {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("faucet paid %s XNO (%s) but it has not arrived yet; the node keeps waiting", fr.Amount, fr.Hash[:8])
}
