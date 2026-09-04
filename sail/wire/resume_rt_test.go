package wire

import "testing"

func TestResumeProofRoundTrip(t *testing.T) {
	var key [32]byte
	key[0] = 7
	p := ResumeProof(key, 1234, 99)
	ts, recv, ok := VerifyResume(key, p)
	if !ok || ts != 1234 || recv != 99 {
		t.Fatalf("round trip: %v %d %d (len %d)", ok, ts, recv, len(p))
	}
}
