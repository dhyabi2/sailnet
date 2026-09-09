package relay

import (
	"testing"

	"github.com/dhyabi2/sail/nano"
)

func TestRepBonusOnlyForAVoteForAFriend(t *testing.T) {
	friend := nano.PubkeyToAddress([32]byte{1, 2, 3})
	other := nano.PubkeyToAddress([32]byte{4, 5, 6})
	op := nano.PubkeyToAddress([32]byte{0x53, 0x41, 1, 9}) // a Sailnet op in the representative field, not a vote
	s := &Server{RepFriends: map[string]bool{friend: true}, RepBonus: 20}

	if n, ok := s.repBonus(1000, friend); !ok || n != 1200 {
		t.Fatalf("a friend's voter should get +20%%, got %d %v", n, ok)
	}
	for name, rep := range map[string]string{"another rep": other, "an op, not a vote": op, "no rep": "", "garbage": "nano_notanaddress"} {
		if n, ok := s.repBonus(1000, rep); ok || n != 1000 {
			t.Fatalf("%s must earn no bonus, got %d %v", name, n, ok)
		}
	}
	off := &Server{RepFriends: map[string]bool{friend: true}} // bonus 0 = off
	if _, ok := off.repBonus(1000, friend); ok {
		t.Fatal("with no bonus set nothing changes")
	}
	none := &Server{RepBonus: 20} // no friends listed
	if _, ok := none.repBonus(1000, friend); ok {
		t.Fatal("with no friends listed nothing changes")
	}
}
