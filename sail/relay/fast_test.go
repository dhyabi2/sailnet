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

	"github.com/dhyabi2/sail/wire"
)

// A link-level command a relay does not know is ignored, not fatal: what a
// relay from before any new command does with it, and what every relay does
// with CmdFast, which was measured slower and withdrawn.
func TestUnknownLinkCommandsAreHarmless(t *testing.T) {
	reg := &Registry{}
	s, ri, _ := startRelay(t, reg, 7, true)
	var tag [32]byte
	copy(tag[:], "direct-anchor-for-the-link-test!")
	s.Quota.Credit(fmt.Sprintf("%x", tag), 50<<20, "")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(strings.Repeat("x", 300000))) }))
	defer target.Close()
	c, err := Build([]*RelayInfo{ri}, tag, 10*time.Second, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.w.write(&wire.Cell{Cmd: 99})
	c.w.write(&wire.Cell{Cmd: wire.CmdFast})
	if bad := c.Ping(5 * time.Second); bad != -1 {
		t.Fatal("the link must survive unknown and withdrawn commands")
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
		t.Fatalf("got %d bytes", len(body))
	}
}
