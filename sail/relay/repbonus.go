package relay

import (
	"log"

	"github.com/dhyabi2/sail/nano"
)

// Representative-aligned price: a relay may credit more bytes per XNO to a
// payer whose account votes for a Nano representative the operator respects.
//
// It is the operator's opinion expressed as a price, per relay, with no
// authority behind it: --rep-friends names the representatives, --rep-bonus
// the extra bytes in percent. The proof is the payer's own payment block,
// which carries the account's representative, so it costs no extra ledger
// call and reveals nothing the payment did not already reveal. It changes
// bytes per XNO only — never routing, never selection (RULES.md 4). A
// representative field that carries a Sailnet op instead of a vote earns
// nothing: that block did not vote.

// repBonus returns the credit for n bytes given the payer's representative
// address, and whether a bonus applied.
func (s *Server) repBonus(n int64, rep string) (int64, bool) {
	if s.RepBonus <= 0 || len(s.RepFriends) == 0 || rep == "" || n <= 0 {
		return n, false
	}
	pub, err := nano.AddressToPubkey(rep)
	if err != nil || nano.TaggedRep(pub) {
		return n, false
	}
	if !s.RepFriends[rep] {
		return n, false
	}
	return n + n*int64(s.RepBonus)/100, true
}

func (s *Server) logRepBonus(before, after int64, rep string) {
	log.Printf("rep-aligned: payer votes for %s; %d KiB credited as %d KiB (+%d%%)", short(rep), before>>10, after>>10, s.RepBonus)
}
