package client

import "testing"

func TestEmptyAnswerAAAA(t *testing.T) {
	// query id 0x1234, RD, one question: example.com AAAA IN, plus an OPT record
	q := []byte{0x12, 0x34, 1, 0, 0, 1, 0, 0, 0, 0, 0, 1,
		7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 3, 'c', 'o', 'm', 0, 0, 28, 0, 1,
		0, 0, 41, 16, 0, 0, 0, 0, 0, 0, 0}
	if qtype(q) != 28 {
		t.Fatalf("qtype %d", qtype(q))
	}
	a := emptyAnswer(q)
	if a == nil || a[0] != 0x12 || a[1] != 0x34 || a[2]&0x80 == 0 || a[7] != 0 || len(a) != 12+17 {
		t.Fatalf("bad answer % x", a)
	}
}
