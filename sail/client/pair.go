package client

import (
	"log"
	"strings"
	"time"

	"github.com/dhyabi2/sail/relay"
	"github.com/dhyabi2/sail/token"
)

// Modes: how this client uses the relays it runs (Settings → Network).
//
//	open    ordinary paths; relays of ours are never anything special
//	mine    a relay of ours is the exit (or a middle hop), free at that hop;
//	        the entry is a stranger, paid, so nobody sees us arrive at ours
//	direct  one hop, through a relay of ours, nothing paid: a VPN through a
//	        box we own — fastest, free, and not anonymity
const (
	ModeOpen   = "open"
	ModeMine   = "mine"
	ModeDirect = "direct"
)

// SetMode picks one of the modes above; anything else is "mine".
func (m *manager) SetMode(mode string) {
	switch mode {
	case ModeOpen, ModeDirect:
	default:
		mode = ModeMine
	}
	m.opts.mode = mode
	if m.opts.hopsWanted == 0 {
		m.opts.hopsWanted = m.opts.hops
	}
	if mode == ModeDirect {
		m.opts.hops = 1
	} else if m.opts.hopsWanted > 0 {
		m.opts.hops = m.opts.hopsWanted
	}
}

// Mode reports the current mode.
func (m *manager) Mode() string {
	if m.opts.mode == "" {
		return ModeMine
	}
	return m.opts.mode
}

// PairRelay makes this wallet an owner of the relay with that account, with
// the code `sailnode pair` printed on it. It pays that relay an ordinary
// anchor (kept there for later use), opens a one-hop circuit, and sends the
// code inside it; the relay records the wallet that paid. On success the
// relay is added to this client's own relays for this run — persisting the
// list is the caller's job (the app keeps it in its settings).
func (m *manager) PairRelay(account, code string) error {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 {
		return errors("a pairing code is six digits")
	}
	rel := m.reg.Get(account)
	if rel == nil {
		return errors("that relay is not known to this client yet; wait a minute for the relay list and try again")
	}
	sign := func(pub, tag [32]byte) []byte { return relay.SignCreate(m.key, pub, tag) }
	// A pairing circuit is free: the tag says which wallet is asking and the
	// relay lets it in while a code is active. Nothing is paid, nothing
	// touches the payment the running circuit was made with.
	c, err := relay.Build([]*relay.RelayInfo{rel}, relay.PairingTag(rel.Pub, m.key.Public), 20*time.Second, relay.PairingCreate(m.key.Public), sign)
	if err != nil {
		if c != nil {
			c.Close()
		}
		if strings.Contains(err.Error(), "hop 0 refused") {
			return errors("the relay did not accept a pairing circuit: run `sailnode pair` on it for a fresh code (and `sailnode upgrade` if it is older than v0.3.30)")
		}
		// The dial error names the relay's address; nothing shown to the user does.
		return errors("could not reach the relay: check that it is running and that its port 443 is open (ufw allow 443/tcp)")
	}
	defer c.Close()
	if err := c.Pair(code, 15*time.Second); err != nil {
		return err
	}
	if m.opts.mine == nil {
		m.opts.mine = map[string]bool{}
	}
	m.opts.mine[account] = true
	log.Printf("paired with %s: it is one of your relays now", short(account))
	return nil
}

// directEntry is the relay of ours to use as the single hop in Direct mode.
func (m *manager) directEntry(pick func(func(*relay.RelayInfo) bool) *relay.RelayInfo) (*relay.RelayInfo, error) {
	e := pick(func(r *relay.RelayInfo) bool { return m.mineUsable(r) && r.Flags&token.FlagExit != 0 })
	if e == nil {
		return nil, errors("none of your relays is reachable right now: switch Network to \"My relays\" or \"Open network\", or check the relay")
	}
	return e, nil
}
