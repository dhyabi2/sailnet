// bridgedb hands out bridge lines a few at a time and retires burned ones.
//
//	bridgedb serve   --listen :8080 --state /var/lib/bridgedb.json --admin <token>
//	bridgedb add     --state ... "sail-bridge:..."          (operator: add a bridge to the pool)
//	bridgedb invite  --state ... [--uses 20] [--bridges 2]  (operator: mint an invite code)
//	bridgedb status  --state ...
//
// HTTP API (put it behind a CDN so the service itself is not a blocking target):
//
//	POST /redeem  {"code":"..."}                     → {"bridges":["sail-bridge:..."]}
//	POST /report  {"code":"...","account":"nano_…"}  → {"ok":true}
//	GET  /health
//
// Bucketing: an invite belongs to a bucket (chosen when minted); every code
// in a bucket sees the same bridges, so one leaked invite exposes one
// bucket's bridges, not the pool. A bridge reported unreachable by three
// distinct invites within a day is retired and its buckets are re-filled
// from the spare pool.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dhyabi2/sail/relay"
)

type Bridge struct {
	Line     string    `json:"line"`
	Account  string    `json:"account"`
	Added    time.Time `json:"added"`
	Burned   bool      `json:"burned"`
	HandedTo int       `json:"handed_to"`
	Reports  []Report  `json:"reports,omitempty"`
}

type Report struct {
	Code string    `json:"code"`
	At   time.Time `json:"at"`
}

type Invite struct {
	Code    string    `json:"code"`
	Bucket  int       `json:"bucket"`
	Uses    int       `json:"uses"` // remaining
	Bridges int       `json:"bridges"`
	Created time.Time `json:"created"`
	LastUse time.Time `json:"last_use"`
}

type State struct {
	mu       sync.Mutex
	path     string
	Bridges  map[string]*Bridge `json:"bridges"` // account → bridge
	Invites  map[string]*Invite `json:"invites"` // code → invite
	Buckets  map[int][]string   `json:"buckets"` // bucket → accounts
	NBuckets int                `json:"n_buckets"`
}

func load(path string) *State {
	st := &State{path: path, Bridges: map[string]*Bridge{}, Invites: map[string]*Invite{}, Buckets: map[int][]string{}, NBuckets: 8}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, st)
	}
	if st.NBuckets == 0 {
		st.NBuckets = 8
	}
	return st
}

func (st *State) save() {
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp := st.path + ".tmp"
	os.WriteFile(tmp, b, 0o600)
	os.Rename(tmp, st.path)
}

// fill gives every bucket up to `want` live bridges from the spare pool,
// preferring the least-exposed bridges.
func (st *State) fill(want int) {
	for b := 0; b < st.NBuckets; b++ {
		var live []string
		for _, acct := range st.Buckets[b] {
			if br := st.Bridges[acct]; br != nil && !br.Burned {
				live = append(live, acct)
			}
		}
		if len(live) >= want {
			st.Buckets[b] = live
			continue
		}
		inUse := map[string]int{}
		for _, accts := range st.Buckets {
			for _, a := range accts {
				inUse[a]++
			}
		}
		var spare []string
		for acct, br := range st.Bridges {
			if br.Burned {
				continue
			}
			if contains(live, acct) {
				continue
			}
			spare = append(spare, acct)
		}
		sort.Slice(spare, func(i, j int) bool { // least exposed first
			if inUse[spare[i]] != inUse[spare[j]] {
				return inUse[spare[i]] < inUse[spare[j]]
			}
			return st.Bridges[spare[i]].HandedTo < st.Bridges[spare[j]].HandedTo
		})
		for _, a := range spare {
			if len(live) >= want {
				break
			}
			live = append(live, a)
		}
		st.Buckets[b] = live
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (st *State) redeem(code string) ([]string, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	inv := st.Invites[code]
	if inv == nil || inv.Uses <= 0 {
		return nil, fmt.Errorf("invalid or exhausted invite")
	}
	if time.Since(inv.LastUse) < 10*time.Second {
		return nil, fmt.Errorf("too fast; try again shortly")
	}
	st.fill(inv.Bridges)
	var lines []string
	for _, acct := range st.Buckets[inv.Bucket] {
		if len(lines) >= inv.Bridges {
			break
		}
		if br := st.Bridges[acct]; br != nil && !br.Burned {
			br.HandedTo++
			lines = append(lines, br.Line)
		}
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("no bridges available right now")
	}
	inv.Uses--
	inv.LastUse = time.Now()
	st.save()
	return lines, nil
}

// report marks a bridge as reported by this invite; three distinct invites
// within 24 h retire it and its buckets are refilled.
func (st *State) report(code, account string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	inv := st.Invites[code]
	br := st.Bridges[account]
	if inv == nil || br == nil || br.Burned {
		return fmt.Errorf("unknown invite or bridge")
	}
	if !contains(st.Buckets[inv.Bucket], account) {
		return fmt.Errorf("this invite never received that bridge") // stops griefing other buckets
	}
	cut := time.Now().Add(-24 * time.Hour)
	var keep []Report
	seen := map[string]bool{}
	for _, r := range br.Reports {
		if r.At.After(cut) {
			keep = append(keep, r)
			seen[r.Code] = true
		}
	}
	if !seen[code] {
		keep = append(keep, Report{Code: code, At: time.Now()})
		seen[code] = true
	}
	br.Reports = keep
	if len(seen) >= 3 {
		br.Burned = true
		log.Printf("bridge %s retired after %d reports", relayShort(account), len(seen))
		st.fill(2)
	}
	st.save()
	return nil
}

func relayShort(a string) string {
	if len(a) > 16 {
		return a[:16] + "…"
	}
	return a
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: bridgedb serve|add|invite|status ...")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	statePath := fs.String("state", "bridgedb.json", "state file")
	listen := fs.String("listen", "127.0.0.1:8080", "serve: listen address (put a CDN or TLS proxy in front)")
	admin := fs.String("admin", os.Getenv("BRIDGEDB_ADMIN"), "serve: admin token for /admin/* (or BRIDGEDB_ADMIN)")
	uses := fs.Int("uses", 20, "invite: how many redemptions the code allows")
	nb := fs.Int("bridges", 2, "invite: bridges handed out per redemption")
	fs.Parse(os.Args[2:])
	st := load(*statePath)
	switch os.Args[1] {
	case "add":
		for _, line := range fs.Args() {
			ri, err := relay.ParseBridgeLine(line)
			if err != nil {
				log.Fatal(err)
			}
			st.Bridges[ri.Account] = &Bridge{Line: strings.TrimSpace(line), Account: ri.Account, Added: time.Now()}
			fmt.Println("added", relayShort(ri.Account))
		}
		st.fill(2)
		st.save()
	case "invite":
		code := randHex(8)
		var bucket int
		fmt.Sscanf(randHex(1), "%x", &bucket)
		bucket %= st.NBuckets
		st.Invites[code] = &Invite{Code: code, Bucket: bucket, Uses: *uses, Bridges: *nb, Created: time.Now()}
		st.save()
		fmt.Println(code)
	case "status":
		live, burned := 0, 0
		for _, b := range st.Bridges {
			if b.Burned {
				burned++
			} else {
				live++
			}
		}
		fmt.Printf("bridges: %d live, %d burned; invites: %d; buckets: %d\n", live, burned, len(st.Invites), st.NBuckets)
		for acct, b := range st.Bridges {
			fmt.Printf("  %s handed_to=%d reports=%d burned=%v\n", relayShort(acct), b.HandedTo, len(b.Reports), b.Burned)
		}
	case "serve":
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
		mux.HandleFunc("/redeem", func(w http.ResponseWriter, r *http.Request) {
			var in struct{ Code string }
			if r.Method != http.MethodPost || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil {
				http.Error(w, "bad request", 400)
				return
			}
			lines, err := st.redeem(strings.TrimSpace(in.Code))
			if err != nil {
				time.Sleep(500 * time.Millisecond) // slow down guessing
				http.Error(w, err.Error(), 403)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"bridges": lines})
		})
		mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
			var in struct{ Code, Account string }
			if r.Method != http.MethodPost || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil {
				http.Error(w, "bad request", 400)
				return
			}
			if err := st.report(strings.TrimSpace(in.Code), strings.TrimSpace(in.Account)); err != nil {
				http.Error(w, err.Error(), 403)
				return
			}
			w.Write([]byte(`{"ok":true}`))
		})
		mux.HandleFunc("/admin/invite", func(w http.ResponseWriter, r *http.Request) {
			if *admin == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Admin")), []byte(*admin)) != 1 {
				http.Error(w, "forbidden", 403)
				return
			}
			st.mu.Lock()
			code := randHex(8)
			var bucket int
			fmt.Sscanf(randHex(1), "%x", &bucket)
			st.Invites[code] = &Invite{Code: code, Bucket: bucket % st.NBuckets, Uses: *uses, Bridges: *nb, Created: time.Now()}
			st.save()
			st.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]string{"code": code})
		})
		ln, err := net.Listen("tcp", *listen)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("bridgedb on %s (%d bridges, %d invites)", *listen, len(st.Bridges), len(st.Invites))
		log.Fatal(http.Serve(ln, mux))
	default:
		fmt.Println("usage: bridgedb serve|add|invite|status ...")
		os.Exit(2)
	}
}
