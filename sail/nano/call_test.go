package nano

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A 429 benches an endpoint for the rest of its cooldown, and a call whose
// every endpoint is benched tries nothing. It used to build its "ledger
// unreachable" error from a nil lastErr and panic — taking down a relay
// whose only reachable endpoint had just rate-limited the startup probe.
func TestCallWithEveryEndpointBenchedReturnsAnError(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer stub.Close()

	nc := &Client{URLs: []string{stub.URL}, HTTP: &http.Client{Timeout: 2 * time.Second}}
	var out map[string]any
	// The first call benches the endpoint (429); the second finds nothing
	// left to try, and must report it rather than panic on a nil error.
	if err := nc.Call(context.Background(), map[string]any{"action": "version"}, &out); err == nil || !strings.Contains(err.Error(), "http 429") {
		t.Fatalf("first call: want the 429 recorded, got %v", err)
	}
	err := nc.Call(context.Background(), map[string]any{"action": "version"}, &out)
	if err == nil || !strings.Contains(err.Error(), "cooling down") {
		t.Fatalf("second call: want the cooling-down error, got %v", err)
	}
}
