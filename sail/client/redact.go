package client

import (
	"io"
	"regexp"
	"strings"
	"sync"
)

// Redaction. The user picks a nickname once; from then on every log line,
// trace and status screen names the user by it. Wallet addresses become the
// nickname, and any IPv4 literal that is not one of the relays we talk to
// becomes "<nick>'s device". The wire is unaffected; this is about what gets
// written down and shown.

var (
	redMu    sync.Mutex
	nick     string
	wallets  []string
	relayIPs = map[string]bool{}
	ipRe     = regexp.MustCompile(`\b(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})\b`)
	acctRe   = regexp.MustCompile(`\bnano_[13][13-9a-km-uw-z]{59}\b`)
)

// SetNick sets the nickname and the wallet addresses it replaces.
func SetNick(n string, walletAddrs ...string) {
	redMu.Lock()
	nick = strings.TrimSpace(n)
	wallets = append([]string(nil), walletAddrs...)
	redMu.Unlock()
}

// Nick returns the nickname ("" if none set).
func Nick() string {
	redMu.Lock()
	defer redMu.Unlock()
	return nick
}

// KeepIP marks an address as a relay's, so it is logged as "relay" rather
// than as the user's device. Relays are named by country and short account.
func KeepIP(ip string) {
	redMu.Lock()
	relayIPs[ip] = true
	redMu.Unlock()
}

// Redact rewrites s per the rules above. With no nickname set it still
// hides wallet addresses behind a fixed label.
func Redact(s string) string {
	redMu.Lock()
	n := nick
	ws := wallets
	keep := relayIPs
	redMu.Unlock()
	label := n
	if label == "" {
		label = "user"
	}
	for _, w := range ws {
		s = strings.ReplaceAll(s, w, label)
	}
	s = acctRe.ReplaceAllStringFunc(s, func(a string) string {
		return a[:11] + "…" // other accounts (relays) stay recognisable but short
	})
	s = ipRe.ReplaceAllStringFunc(s, func(ip string) string {
		if strings.HasPrefix(ip, "127.") {
			return ip
		}
		if keep[ip] {
			return "relay"
		}
		return label + "'s device"
	})
	return s
}

// RedactingWriter redacts each write before passing it on.
type RedactingWriter struct{ W io.Writer }

func (r RedactingWriter) Write(p []byte) (int, error) {
	if _, err := r.W.Write([]byte(Redact(string(p)))); err != nil {
		return 0, err
	}
	return len(p), nil
}
