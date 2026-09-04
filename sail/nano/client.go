package nano

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Default RPC endpoints. Sailnet's own endpoint comes first: it keeps its
// upstream key server-side and fails over on its own. Public nodes follow as
// fallbacks. NANO_RPC_URLS (or the apps' RPC setting) overrides the list.
const (
	PrimaryRPC  = "https://www.sailnet.space/node/api"
	FallbackRPC = "https://sailnet-app.vercel.app/node/api"
)

var DefaultRPCs = []string{PrimaryRPC, FallbackRPC, "https://rpc.nano-gpt.com", "https://node.somenano.com/proxy", "https://nanoslo.0x.no/proxy", "https://app.natrium.io/api"}

// Client talks JSON-RPC to Nano nodes with failover on transport errors.
type Client struct {
	URLs    []string
	HTTP    *http.Client
	APIKey  string // optional key for a user-configured rpc.nano.to endpoint (sent to that host only)
	Verbose bool
	Budget  *Budget // nil = DefaultBudget

	mu       sync.Mutex
	cooldown map[string]time.Time // endpoint → do not call before (after a ban/429)
}

// benched reports whether an endpoint is in cooldown.
func (c *Client) benched(u string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Before(c.cooldown[u])
}

// bench puts an endpoint in cooldown; a banned endpoint must not be retried,
// because every retry extends the ban.
func (c *Client) bench(u string, d time.Duration) {
	c.mu.Lock()
	if c.cooldown == nil {
		c.cooldown = map[string]time.Time{}
	}
	c.cooldown[u] = time.Now().Add(d)
	c.mu.Unlock()
	log.Printf("rpc: %s benched for %s", u, d.Round(time.Second))
}

// NewClient returns a client using the default endpoints.
func NewClient() *Client {
	urls := DefaultRPCs
	if env := os.Getenv("NANO_RPC_URLS"); env != "" {
		urls = strings.Split(env, ",")
	}
	c := &Client{URLs: urls, HTTP: &http.Client{Timeout: 20 * time.Second}, APIKey: os.Getenv("NANO_RPC_KEY")}
	if len(urls) > 0 && isLocalURL(urls[0]) {
		c.Budget = NewBudget(50, 100) // your own node: no public rate limit to respect
	}
	return c
}

// Call performs an RPC action and decodes the JSON result into out.
func (c *Client) Call(ctx context.Context, body map[string]any, out any) error {
	b := c.Budget
	if b == nil {
		b = DefaultBudget
	}
	if err := b.Wait(ctx, fmt.Sprint(body["action"])); err != nil {
		return err
	}
	raw, _ := json.Marshal(body)
	var lastErr error
	for _, u := range c.URLs {
		if c.benched(u) {
			continue
		}
		payload := raw
		if c.APIKey != "" && strings.Contains(u, "rpc.nano.to") {
			// The key belongs to rpc.nano.to only; never hand it to a fallback.
			keyed := map[string]any{}
			for k, v := range body {
				keyed[k] = v
			}
			keyed["key"] = c.APIKey
			payload, _ = json.Marshal(keyed)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
		if err == nil && c.APIKey != "" && strings.Contains(u, "rpc.nano.to") {
			req.Header.Set("key", c.APIKey)
		}
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue // transport failure → failover
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode == 429 {
			c.bench(u, 15*time.Minute)
			lastErr = fmt.Errorf("%s: http 429", u)
			continue
		}
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("%s: http %d", u, resp.StatusCode)
			continue
		}
		if len(bytes.TrimSpace(data)) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("{}")) {
			lastErr = fmt.Errorf("%s: empty response (rate limited?)", u)
			continue
		}
		var e struct {
			Error      any     `json:"error"`
			Message    string  `json:"message"`
			RetryAfter float64 `json:"retry_after"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != nil {
			msg := fmt.Sprint(e.Error)
			if _, isNum := e.Error.(float64); isNum || strings.Contains(strings.ToLower(e.Message+msg), "limit") || strings.Contains(msg, "429") || strings.Contains(strings.ToLower(e.Message), "banned") {
				d := 15 * time.Minute
				if e.RetryAfter > 0 {
					d = time.Duration(e.RetryAfter) * time.Second
				}
				c.bench(u, d) // rate limit / ban → next endpoint, and leave this one alone
				lastErr = fmt.Errorf("%s: %s %s", u, msg, e.Message)
				continue
			}
			return &RPCError{Action: fmt.Sprint(body["action"]), Msg: msg}
		}
		if out != nil {
			return json.Unmarshal(data, out)
		}
		return nil
	}
	return fmt.Errorf("ledger unreachable: %s", stripURL(lastErr.Error()))
}

// RPCError is a semantic error returned by the node (not failed over).
type RPCError struct{ Action, Msg string }

func (e *RPCError) Error() string { return e.Action + ": " + e.Msg }

// IsNotFound reports the "Account not found" condition.
func IsNotFound(err error) bool {
	var re *RPCError
	return errors.As(err, &re) && (re.Msg == "Account not found" || re.Msg == "Block not found")
}

// AccountInfo is the subset of account_info we need.
type AccountInfo struct {
	Frontier           string `json:"frontier"`
	Balance            string `json:"balance"`
	Representative     string `json:"representative"`
	BlockCount         string `json:"block_count"`
	ConfirmationHeight string `json:"confirmation_height"`
}

// AccountInfo fetches frontier/balance/representative; ok=false if unopened.
func (c *Client) AccountInfo(ctx context.Context, account string) (info AccountInfo, ok bool, err error) {
	err = c.Call(ctx, map[string]any{"action": "account_info", "account": account, "representative": true}, &info)
	if IsNotFound(err) {
		return info, false, nil
	}
	if err == nil && (info.Frontier == "" || info.Balance == "") {
		return info, false, errors.New("account_info returned no frontier (rate limited?)")
	}
	return info, err == nil, err
}

// HistoryBlock is one raw block from account_history (raw:true).
type HistoryBlock struct {
	Type           string `json:"type"`
	Subtype        string `json:"subtype"`
	Account        string `json:"account"`
	Representative string `json:"representative"`
	Link           string `json:"link"`
	Balance        string `json:"balance"`
	Previous       string `json:"previous"`
	Amount         string `json:"amount"`
	Height         string `json:"height"`
	Hash           string `json:"hash"`
	Signature      string `json:"signature"`
	Work           string `json:"work"`
	Confirmed      string `json:"confirmed"`
	LocalTimestamp string `json:"local_timestamp"`
}

// History returns the whole chain of an account, oldest first.
func (c *Client) History(ctx context.Context, account string) ([]HistoryBlock, error) {
	var all []HistoryBlock
	head := ""
	for {
		body := map[string]any{"action": "account_history", "account": account, "count": 500, "raw": true}
		if head != "" {
			body["head"] = head
		}
		var v struct {
			History  []HistoryBlock `json:"history"`
			Previous string         `json:"previous"`
		}
		if err := c.Call(ctx, body, &v); err != nil {
			if IsNotFound(err) {
				return nil, nil
			}
			return nil, err
		}
		all = append(all, v.History...)
		if v.Previous == "" || len(v.History) == 0 {
			break
		}
		head = v.Previous
	}
	// newest-first → oldest-first
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	// Some public RPCs (rpc.nano.to) ignore raw:true and omit subtype/link/
	// representative. Normalise what can be derived: subtype from type, and a
	// send's link from its destination account. Representatives and receive
	// links must be fetched with BlocksInfo when needed.
	for i := range all {
		b := &all[i]
		if b.Subtype == "" {
			b.Subtype = b.Type
		}
		if b.Link == "" && b.Subtype == "send" && b.Account != "" {
			if pk, err := AddressToPubkey(b.Account); err == nil {
				b.Link = strings.ToUpper(hex.EncodeToString(pk[:]))
			}
		}
	}
	return all, nil
}

// Receivable is one unreceived send.
type Receivable struct {
	Hash   string
	Amount *big.Int
	Source string
}

// Receivables lists unreceived sends to account (with source accounts).
func (c *Client) Receivables(ctx context.Context, account string, count int) ([]Receivable, error) {
	var v struct {
		Blocks json.RawMessage `json:"blocks"`
	}
	err := c.Call(ctx, map[string]any{"action": "receivable", "account": account, "count": count, "source": true, "sorting": true}, &v)
	if err != nil {
		return nil, err
	}
	var m map[string]struct {
		Amount string `json:"amount"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(v.Blocks, &m); err != nil {
		return nil, nil // "" or [] when empty
	}
	var out []Receivable
	for h, e := range m {
		amt, _ := new(big.Int).SetString(e.Amount, 10)
		out = append(out, Receivable{Hash: h, Amount: amt, Source: e.Source})
	}
	return out, nil
}

// BlockInfo is block_info / blocks_info contents.
type BlockInfo struct {
	BlockAccount string `json:"block_account"`
	Amount       string `json:"amount"`
	Subtype      string `json:"subtype"`
	Confirmed    string `json:"confirmed"`
	Height       string `json:"height"`
	Contents     struct {
		Type           string `json:"type"`
		Account        string `json:"account"`
		Previous       string `json:"previous"`
		Representative string `json:"representative"`
		Balance        string `json:"balance"`
		Link           string `json:"link"`
		LinkAsAccount  string `json:"link_as_account"`
		Signature      string `json:"signature"`
		Work           string `json:"work"`
	} `json:"contents"`
}

// BlocksInfo fetches several blocks by hash (chunked).
func (c *Client) BlocksInfo(ctx context.Context, hashes []string) (map[string]BlockInfo, error) {
	out := map[string]BlockInfo{}
	for i := 0; i < len(hashes); i += 100 {
		j := i + 100
		if j > len(hashes) {
			j = len(hashes)
		}
		var v struct {
			Blocks map[string]BlockInfo `json:"blocks"`
		}
		if err := c.Call(ctx, map[string]any{"action": "blocks_info", "json_block": true, "hashes": hashes[i:j]}, &v); err != nil {
			return nil, err
		}
		for h, b := range v.Blocks {
			out[h] = b
		}
	}
	return out, nil
}

// WorkGenerate asks the node for work; caller falls back to CPU on error.
func (c *Client) WorkGenerate(ctx context.Context, rootHex string, threshold uint64) (string, error) {
	var v struct {
		Work string `json:"work"`
	}
	body := map[string]any{"action": "work_generate", "hash": rootHex, "difficulty": fmt.Sprintf("%016x", threshold), "use_peers": "true"}
	err := c.Call(ctx, body, &v)
	if err != nil || v.Work == "" {
		// Work requests are rare and the CPU fallback costs minutes on a
		// small VPS, so a primary that is benched for rate limiting is still
		// worth one direct try: a 429 here loses a second, not six minutes.
		if err2 := c.callDirect(ctx, PrimaryRPC, body, &v); err2 == nil && v.Work != "" {
			return v.Work, nil
		}
		if err == nil {
			err = errors.New("empty work")
		}
		return "", err
	}
	return v.Work, nil
}

// callDirect posts to one endpoint, ignoring the cooldown list and the budget.
func (c *Client) callDirect(ctx context.Context, u string, body map[string]any, out any) error {
	if c.APIKey != "" && strings.Contains(u, "rpc.nano.to") {
		body["key"] = c.APIKey
	}
	raw, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" && strings.Contains(u, "rpc.nano.to") {
		req.Header.Set("key", c.APIKey)
	}
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: http %d", u, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Process publishes a signed block. Returns its hash.
func (c *Client) Process(ctx context.Context, b *Block, subtype string) (string, error) {
	var v struct {
		Hash string `json:"hash"`
	}
	err := c.Call(ctx, map[string]any{"action": "process", "json_block": true, "subtype": subtype, "block": b.JSON()}, &v)
	if err == nil && v.Hash == "" {
		return "", errors.New("process returned no hash (rate limited?)")
	}
	return v.Hash, err
}

// NodeStatus is what Probe learns from a Nano node.
type NodeStatus struct {
	URL      string
	Version  string
	Count    uint64 // blocks in the ledger
	Cemented uint64 // confirmed blocks
	Local    bool   // URL points at this machine or a private network
}

// Probe asks the first usable endpoint for its version and block counts.
func (c *Client) Probe(ctx context.Context) (NodeStatus, error) {
	var st NodeStatus
	for _, u := range c.URLs {
		st.URL = u
		st.Local = isLocalURL(u)
		var v struct {
			NodeVendor string `json:"node_vendor"`
		}
		var bc struct {
			Count    string `json:"count"`
			Cemented string `json:"cemented"`
		}
		saved := c.URLs
		c.URLs = []string{u}
		err1 := c.Call(ctx, map[string]any{"action": "version"}, &v)
		err2 := c.Call(ctx, map[string]any{"action": "block_count"}, &bc)
		c.URLs = saved
		if err1 != nil && err2 != nil {
			continue
		}
		st.Version = v.NodeVendor
		st.Count, _ = strconv.ParseUint(bc.Count, 10, 64)
		st.Cemented, _ = strconv.ParseUint(bc.Cemented, 10, 64)
		return st, nil
	}
	return st, errors.New("no Nano RPC endpoint answered")
}

// isLocalURL reports whether an RPC URL points at this host or a private network.
func isLocalURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

// Synced reports whether a node's ledger looks current: nearly everything it
// holds is cemented. A node still bootstrapping has a large gap.
func (s NodeStatus) Synced() bool {
	return s.Count > 0 && s.Cemented > 0 && s.Count-s.Cemented < 100000
}

// ConfigureRPC sets the endpoint list and key every later NewClient uses:
// url first (if given), then the public defaults as fallbacks. The apps call
// it from their settings; the CLI uses the NANO_RPC_URLS / NANO_RPC_KEY
// environment directly.
func ConfigureRPC(url, key string) {
	url = strings.TrimSpace(url)
	if url != "" {
		var urls []string
		for _, u := range strings.Split(url, ",") {
			if u = strings.TrimSpace(u); u != "" {
				urls = append(urls, u)
			}
		}
		for _, u := range DefaultRPCs {
			seen := false
			for _, v := range urls {
				if v == u {
					seen = true
				}
			}
			if !seen {
				urls = append(urls, u)
			}
		}
		os.Setenv("NANO_RPC_URLS", strings.Join(urls, ","))
	} else {
		os.Unsetenv("NANO_RPC_URLS")
	}
	if key = strings.TrimSpace(key); key != "" {
		os.Setenv("NANO_RPC_KEY", key)
	} else {
		os.Unsetenv("NANO_RPC_KEY")
	}
}

var urlInErr = regexp.MustCompile(`(?:Post|Get) "https?://[^"]+": `)

// stripURL removes the endpoint from a transport error: logs and screens say
// what went wrong, not which provider was asked.
func stripURL(msg string) string { return urlInErr.ReplaceAllString(msg, "") }
