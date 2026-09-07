package relay

import (
	"context"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dhyabi2/sail/token"
)

// A real three-hop circuit at the price the network actually charges, with
// every quota derived from a real payment rather than a round number.
//
// This is the test the price change needed. The old failure was never in the
// data path: it was that a fixed XNO anchor bought a hundred kilobytes at a
// higher price, and a fixed XNO pool bought less than the 8 MiB a relay
// requires before it will prepay a peer at all — so the money ran out or was
// never sent, and the circuit died for reasons no wire test would show.
func TestPricedCircuitAtTheNewDefault(t *testing.T) {
	const anchorBytes = 10 << 20 // what one client anchor buys, in the client
	const poolBytes = 32 << 20   // what one relay-to-relay pool buys

	for _, tc := range []struct {
		name string
		xno  string
	}{
		{"the old price", "0.00005"},
		{"the new default", "0.0005"},
		{"a relay ten times dearer than the default", "0.005"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rate, err := token.RateFromXNO(tc.xno)
			if err != nil {
				t.Fatal(err)
			}
			rateRaw := token.RateToRaw(rate)

			reg := &Registry{}
			var servers []*Server
			var infos []*RelayInfo
			for i := 0; i < 3; i++ {
				s, ri, _ := startRelay(t, reg, i+20, i == 2)
				// Price every relay at the rate under test, and meter against it.
				ri.MinRate = rate
				s.Quota.SetMinRate(rateRaw)
				s.MinCredit = 10 << 20
				// Pools are prepaid on the ledger, which a wire test has none
				// of, so the running relays use static pool tags and the
				// sizing decision is checked on an identically configured
				// server below.
				s.PoolBytes = poolBytes
				servers = append(servers, s)
				infos = append(infos, ri)
			}

			// A relay must be willing to prepay each of its peers. This is the
			// check that used to fail silently at any price above about 12x the
			// old default, taking the peer out of every circuit through it.
			payer := &Server{PoolRaw: mustXNO(t, "0.001"), PoolBytes: poolBytes} // the --pool our deployed relays pass
			for i := 0; i < 2; i++ {
				if got := payer.poolSize(infos[i+1]); got < 8<<20 {
					t.Fatalf("hop %d would refuse to prepay hop %d: its pool buys only %d bytes", i, i+1, got)
				}
			}

			// Credit exactly what the real payments buy, at this price.
			var tag [32]byte
			copy(tag[:], fmt.Sprintf("priced-e2e-%019d-tag", rate))
			anchor := RawFor(anchorBytes, rateRaw)
			servers[0].Quota.Credit(hexTag(tag), BytesFor(anchor, rateRaw), "")
			for i := 0; i < 2; i++ {
				pool := payer.poolRaw(infos[i+1])
				servers[i+1].Quota.Credit(PoolTag(infos[i].Account, infos[i+1].Account), BytesFor(pool, rateRaw), "")
			}
			t.Logf("%s XNO/MiB: anchor %s XNO buys %d MiB; pool %s XNO buys %d MiB",
				tc.xno, token.FormatXNO(anchor), BytesFor(anchor, rateRaw)>>20,
				token.FormatXNO(payer.poolRaw(infos[1])), payer.poolSize(infos[1])>>20)

			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(strings.Repeat("payload!", 128<<10/8))) // 128 KiB
			}))
			defer target.Close()

			c, err := Build(infos, tag, 10*time.Second, nil, nil)
			if err != nil {
				t.Fatalf("build failed at hop %d: %v", c.Failed, err)
			}
			defer c.Close()
			tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				st, err := c.Open(addr, 5*time.Second)
				if err != nil {
					return nil, err
				}
				return fakeConn{st}, nil
			}}
			resp, err := (&http.Client{Transport: tr, Timeout: 20 * time.Second}).Get(target.URL)
			if err != nil {
				t.Fatalf("fetch through 3 hops: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			if len(body) != 128<<10 {
				t.Fatalf("got %d bytes through the circuit, want %d", len(body), 128<<10)
			}

			// One 128 KiB page must not exhaust an anchor at any price. It did
			// before: at 0.005 XNO/MiB the old fixed anchor bought 102 KiB, so
			// the page itself outran what the client had paid for.
			q, err := c.QueryQuota(5 * time.Second)
			if err != nil {
				t.Fatalf("quota query: %v", err)
			}
			if q <= 0 {
				t.Fatalf("anchor exhausted by a single page: %d bytes left", q)
			}
			if q > BytesFor(anchor, rateRaw) {
				t.Fatalf("quota %d exceeds what was paid for", q)
			}
			t.Logf("128 KiB fetched, %d MiB of the anchor left", q>>20)
		})
	}
}

func hexTag(t [32]byte) string {
	return strings.ToUpper(fmt.Sprintf("%x", t))
}

var _ = big.NewInt
