// sail — wallet CLI for Sailnet (payments are plain XNO sends).
//
//	sail wallet new|show                 local wallet (~/.sail/wallet.json or $SAIL_WALLET)
//	sail receive                         pocket receivable XNO
//	sail send <to> <xno>                 plain XNO transfer
//	sail pay <relay> <xno>               pay a relay; prints the payment tag (block hash)
//	sail register <cc> <asn> <rate> <flags>   REGISTER this wallet as a relay (rate = XNO/MiB)
//	sail relays                          relay registry from the ledger
//	sail rewards [epoch]                 levy table for an epoch
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dhyabi2/sail/nano"
	"github.com/dhyabi2/sail/relay"
	"github.com/dhyabi2/sail/token"
)

// Treasury is the Sailnet registry anchor account (relays REGISTER by sending 1 raw to it).
const Treasury = "nano_1cexacexqrr51coik9eeqzebmfej7g6chrd1eygmzm9ad86qek84pnhpda1t"

type walletFile struct {
	Seed    string `json:"seed"`
	Index   uint32 `json:"index"`
	Address string `json:"address"`
}

func walletPath() string {
	if p := os.Getenv("SAIL_WALLET"); p != "" {
		return p
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".sail", "wallet.json")
}

func loadWallet() (*nano.Key, error) {
	var wf walletFile
	data, err := os.ReadFile(walletPath())
	if err != nil {
		return nil, fmt.Errorf("no wallet at %s (run: sail wallet new)", walletPath())
	}
	if err := json.Unmarshal(data, &wf); err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(wf.Seed)
	if err != nil {
		return nil, err
	}
	return nano.DeriveKey(seed, wf.Index)
}

func die(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println(strings.TrimSpace(usage))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	c := nano.NewClient()
	if k := os.Getenv("NANO_RPC_KEY"); k != "" {
		c.APIKey = k
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "wallet":
		if len(args) > 0 && args[0] == "new" {
			if _, err := os.Stat(walletPath()); err == nil {
				die(fmt.Errorf("wallet already exists at %s", walletPath()))
			}
			seed, err := nano.NewSeed()
			die(err)
			k, err := nano.DeriveKey(seed, 0)
			die(err)
			os.MkdirAll(filepath.Dir(walletPath()), 0o700)
			data, _ := json.MarshalIndent(walletFile{Seed: hex.EncodeToString(seed), Address: k.Address}, "", "  ")
			die(os.WriteFile(walletPath(), data, 0o600))
			fmt.Println("created", walletPath())
			fmt.Println(k.Address)
			return
		}
		k, err := loadWallet()
		die(err)
		info, ok, err := c.AccountInfo(ctx, k.Address)
		die(err)
		fmt.Println("address:", k.Address)
		if ok {
			bal, _ := new(big.Int).SetString(info.Balance, 10)
			fmt.Printf("xno balance: %s (raw %s)\n", token.FormatXNO(bal), info.Balance)
		} else {
			fmt.Println("xno balance: unopened (send XNO to it, then `sail receive`)")
		}
		rs, _ := c.Receivables(ctx, k.Address, 100)
		fmt.Println("receivable sends:", len(rs))
	case "receive":
		k, err := loadWallet()
		die(err)
		n, err := (&nano.Account{Key: k, Client: c}).ReceiveAll(ctx)
		fmt.Println("received", n)
		die(err)
	case "send", "xno-send", "pay":
		if len(args) < 2 {
			fmt.Println(strings.TrimSpace(usage))
			os.Exit(2)
		}
		k, err := loadWallet()
		die(err)
		raw, err := token.ParseXNO(args[1])
		die(err)
		h, err := (&nano.Account{Key: k, Client: c}).Send(ctx, args[0], raw, nil)
		die(err)
		if os.Args[1] == "pay" {
			fmt.Printf("paid %s XNO to %s\npayment tag %s\n", args[1], args[0], h)
		} else {
			fmt.Println("XNO sent:", h)
		}
	case "register":
		if len(args) < 4 {
			fmt.Println(strings.TrimSpace(usage))
			os.Exit(2)
		}
		k, err := loadWallet()
		die(err)
		asn, _ := strconv.ParseUint(args[1], 10, 32)
		rate, err := token.RateFromXNO(args[2])
		die(err)
		flags, _ := strconv.ParseUint(args[3], 10, 16)
		rep, err := token.Encode(token.Op{Code: token.OpRegister, Aux: token.RegisterAux(strings.ToUpper(args[0]), uint32(asn), rate, uint16(flags))})
		die(err)
		h, err := (&nano.Account{Key: k, Client: c}).Send(ctx, Treasury, big.NewInt(1), &rep)
		die(err)
		fmt.Println("REGISTER sent:", h)
	case "rewards": // sail rewards [epoch]: the levy table every node computes (default: last settled epoch)
		e := relay.CurrentEpoch() - 2
		if len(os.Args) > 2 {
			v, err := strconv.ParseUint(os.Args[2], 10, 32)
			die(err)
			e = uint32(v)
		}
		t, err := relay.ComputeEpoch(ctx, c, Treasury, e)
		die(err)
		fmt.Print(t.String())
	case "relays":
		reg := &relay.Registry{Client: c, Treasury: Treasury}
		die(reg.Refresh(ctx))
		for _, r := range reg.All() {
			fmt.Printf("%s  %s  cc=%s asn=%d rate=%s XNO/MiB flags=%d\n", r.Account, r.Desc.Addr(), r.Country, r.ASN, token.FormatXNO(token.RateToRaw(r.MinRate)), r.Flags)
		}
		st, err := token.NewIndexer(c, Treasury).Run(ctx)
		die(err)
		fmt.Printf("registry root %s (%d relays)\n", st.Root(), len(st.Relays))
	default:
		fmt.Println(strings.TrimSpace(usage))
	}
}

const usage = `
sail wallet new|show
sail receive
sail send <to> <xno>
sail pay <relay> <xno>          (prints the payment tag = block hash)
sail register <cc> <asn> <rate-xno-per-MiB> <flags>   (flags: 1 public, 2 exit, 4 home)
sail relays
`
