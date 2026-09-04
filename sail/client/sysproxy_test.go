package client

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// On a Mac with admin rights the system SOCKS proxy is set and put back.
func TestSystemProxyRoundTrip(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	before := macGetSocks("Wi-Fi")
	if err := SetSystemProxy("127.0.0.1:1080"); err != nil {
		t.Skip("cannot set system proxy here: ", err)
	}
	out, _ := exec.Command("networksetup", "-getsocksfirewallproxy", "Wi-Fi").Output()
	if !strings.Contains(string(out), "Enabled: Yes") || !strings.Contains(string(out), "Port: 1080") {
		RestoreSystemProxy()
		t.Fatalf("not set: %s", out)
	}
	RestoreSystemProxy()
	after := macGetSocks("Wi-Fi")
	if after != before {
		t.Fatalf("not restored: before %+v after %+v", before, after)
	}
}
