package wire

import "testing"

func TestReplayWindow(t *testing.T) {
	var w replayWindow
	for _, seq := range []uint64{1, 2, 3, 5, 4, 70, 69} {
		if !w.accept(seq) {
			t.Fatalf("seq %d should be accepted", seq)
		}
	}
	for _, seq := range []uint64{3, 5, 70, 69, 0, 6} { // 6 is now more than 64 behind 70
		if w.accept(seq) {
			t.Fatalf("seq %d should be rejected", seq)
		}
	}
}

func TestOnionReplayRejected(t *testing.T) {
	cPriv, cPub, _ := GenX25519()
	hPriv, hPub, _ := GenX25519()
	client, _ := DeriveHopKeys(cPriv, hPub, cPub, hPub)
	hop, _ := DeriveHopKeys(hPriv, cPub, cPub, hPub)
	box, err := OnionSeal([]*HopKeys{client}, CmdData, 7, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := PeelForward(hop, box); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := PeelForward(hop, box); err != ErrReplay {
		t.Fatalf("replay accepted: %v", err)
	}
	reply, _ := SealBackward(hop, CmdData, 7, []byte("y"))
	if _, _, _, _, err := PeelBackward([]*HopKeys{client}, reply); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := PeelBackward([]*HopKeys{client}, reply); err != ErrReplay {
		t.Fatalf("backward replay accepted: %v", err)
	}
}
