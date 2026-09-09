package relay

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dhyabi2/sail/shape"
	"github.com/dhyabi2/sail/wire"
)

func TestFastParamsTakeOutTheWaitsAndThePadding(t *testing.T) {
	p := &shape.Params{Coalesce: 30 * time.Millisecond, MaxDelay: 200 * time.Millisecond, PadAfterIdle: 0.5, PadTail: 0.2, MinRecord: 100, MaxRecord: 1400}
	q := fastParams(p)
	if q.Coalesce != 0 || q.PadAfterIdle != 0 || q.PadTail != 0 || q.MaxDelay > 5*time.Millisecond {
		t.Fatalf("fast params = %+v", q)
	}
	if q.MinRecord != p.MinRecord || q.MaxRecord != p.MaxRecord {
		t.Fatal("record cutting must be kept: the link still looks like HTTPS")
	}
	if p.Coalesce == 0 {
		t.Fatal("the original must be untouched")
	}
}

// A Direct (fast) circuit works end to end, and a link-level command a relay
// does not know is ignored, not fatal — which is what a relay from before
// CmdFast does with it.
func TestDirectFastCircuitAndUnknownLinkCommandsAreHarmless(t *testing.T) {
	reg := &Registry{}
	s, ri, _ := startRelay(t, reg, 7, true)
	var tag [32]byte
	copy(tag[:], "direct-fast-anchor-for-the-test!")
	s.Quota.Credit(fmt.Sprintf("%x", tag), 50<<20, "")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(strings.Repeat("x", 300000))) }))
	defer target.Close()

	c, err := BuildFast([]*RelayInfo{ri}, tag, nil, 10*time.Second, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Something a relay of any version must shrug off: an unknown command on circuit 0.
	c.w.write(&wire.Cell{Cmd: 99})
	if bad := c.Ping(5 * time.Second); bad != -1 {
		t.Fatal("the link must survive an unknown command")
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		st, err := c.Open(addr, 5*time.Second)
		if err != nil {
			return nil, err
		}
		return fakeConn{st}, nil
	}}
	resp, err := (&http.Client{Transport: tr, Timeout: 20 * time.Second}).Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != 300000 {
		t.Fatalf("got %d bytes through the fast circuit", len(body))
	}
}
