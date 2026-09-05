package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The faucet is a convenience. Whatever it answers, including an empty body,
// nonsense, an error or nothing at all, a client must come away with an error
// it can report and must never panic: an app that cannot get a free trial
// still has to run.
func TestFaucetFailuresNeverPanic(t *testing.T) {
	answers := []http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ok":true}`)) },                  // no hash
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ok":true,"hash":"ab"}`)) },      // short hash
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`not json at all`)) },              // garbage
		func(w http.ResponseWriter, r *http.Request) { w.Write(nil) },                                    // empty
		func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) },                      // server error
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ok":false,"error":"dry"}`)) },   // refused
		func(w http.ResponseWriter, r *http.Request) { time.Sleep(50 * time.Millisecond); w.Write(nil) }, // slow then empty
	}
	for i, h := range answers {
		srv := httptest.NewServer(h)
		old := FaucetURL
		FaucetURL = srv.URL
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("answer %d made the client panic: %v", i, r)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			ClaimFaucet(ctx, srv.Client(), "nano_3bb34zg548rb1j79oy18fy7g8tgrwxoz68h5z1x71sn1ieq1uone6sobihcs")
		}()
		FaucetURL = old
		srv.Close()
	}
}
