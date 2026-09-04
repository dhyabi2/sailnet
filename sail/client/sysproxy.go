package client

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// System proxy: the desktop app points the whole computer's browsers at its
// SOCKS5 port while connected and puts the previous setting back on
// disconnect. The previous setting is saved to disk first, so a crash or a
// killed process is repaired at the next start (RestoreSystemProxy runs
// before anything else), and the machine is never left on a dead proxy.
//
//   - macOS: networksetup, per network service (Wi-Fi, Ethernet, ...)
//   - Windows: the WinINet settings in the registry (ProxyEnable/ProxyServer)
//   - Linux: GNOME/gsettings when present (KDE and others: the app shows the
//     SOCKS address to enter by hand)

type sysProxyBackup struct {
	OS       string                   `json:"os"`
	Services map[string]macSocksState `json:"services,omitempty"` // macOS
	Windows  *winProxyState           `json:"windows,omitempty"`
	Gnome    map[string]string        `json:"gnome,omitempty"`
}

type macSocksState struct {
	Enabled bool   `json:"enabled"`
	Server  string `json:"server"`
	Port    string `json:"port"`
}

type winProxyState struct {
	Enable string `json:"enable"` // registry ProxyEnable as printed by reg query, "" when absent
	Server string `json:"server"`
}

func sysProxyBackupPath() string { return filepath.Join(dataDir(), "sysproxy-backup.json") }

// SetSystemProxy routes the computer's browsers through the SOCKS5 proxy at
// addr ("127.0.0.1:1080"). It saves the previous setting first.
func SetSystemProxy(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	// A backup that already exists belongs to a run that did not restore:
	// restore it first, so the saved state is always the user's own setting.
	RestoreSystemProxy()
	b := sysProxyBackup{OS: runtime.GOOS}
	switch runtime.GOOS {
	case "darwin":
		b.Services = map[string]macSocksState{}
		for _, svc := range macServices() {
			b.Services[svc] = macGetSocks(svc)
		}
		if err := writeSysProxyBackup(b); err != nil {
			return err
		}
		var firstErr error
		for svc := range b.Services {
			if out, err := exec.Command("networksetup", "-setsocksfirewallproxy", svc, host, port).CombinedOutput(); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %v (%s)", svc, err, strings.TrimSpace(string(out)))
				}
				continue
			}
			exec.Command("networksetup", "-setsocksfirewallproxystate", svc, "on").Run()
		}
		return firstErr
	case "windows":
		b.Windows = &winProxyState{Enable: regQuery("ProxyEnable"), Server: regQuery("ProxyServer")}
		if err := writeSysProxyBackup(b); err != nil {
			return err
		}
		key := `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`
		if out, err := exec.Command("reg", "add", key, "/v", "ProxyServer", "/t", "REG_SZ", "/d", "socks="+host+":"+port, "/f").CombinedOutput(); err != nil {
			return fmt.Errorf("reg add: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		if out, err := exec.Command("reg", "add", key, "/v", "ProxyEnable", "/t", "REG_DWORD", "/d", "1", "/f").CombinedOutput(); err != nil {
			return fmt.Errorf("reg add: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		winRefreshProxy()
		return nil
	case "linux":
		if _, err := exec.LookPath("gsettings"); err != nil {
			return fmt.Errorf("no gsettings: set SOCKS5 %s:%s in your desktop's network settings", host, port)
		}
		b.Gnome = map[string]string{}
		for _, k := range []string{"org.gnome.system.proxy mode", "org.gnome.system.proxy.socks host", "org.gnome.system.proxy.socks port"} {
			out, _ := exec.Command("gsettings", append([]string{"get"}, strings.Fields(k)...)...).Output()
			b.Gnome[k] = strings.TrimSpace(string(out))
		}
		if err := writeSysProxyBackup(b); err != nil {
			return err
		}
		exec.Command("gsettings", "set", "org.gnome.system.proxy.socks", "host", host).Run()
		exec.Command("gsettings", "set", "org.gnome.system.proxy.socks", "port", port).Run()
		return exec.Command("gsettings", "set", "org.gnome.system.proxy", "mode", "manual").Run()
	}
	return fmt.Errorf("system proxy not supported on %s", runtime.GOOS)
}

// RestoreSystemProxy puts back whatever SetSystemProxy saved, if anything,
// and removes the backup. Safe to call at any time, including at startup.
func RestoreSystemProxy() {
	data, err := os.ReadFile(sysProxyBackupPath())
	if err != nil {
		return
	}
	var b sysProxyBackup
	if json.Unmarshal(data, &b) != nil {
		os.Remove(sysProxyBackupPath())
		return
	}
	switch b.OS {
	case "darwin":
		for svc, st := range b.Services {
			if st.Enabled && st.Server != "" {
				exec.Command("networksetup", "-setsocksfirewallproxy", svc, st.Server, st.Port).Run()
				exec.Command("networksetup", "-setsocksfirewallproxystate", svc, "on").Run()
			} else {
				exec.Command("networksetup", "-setsocksfirewallproxystate", svc, "off").Run()
			}
		}
	case "windows":
		key := `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`
		if b.Windows != nil {
			if b.Windows.Server == "" {
				exec.Command("reg", "delete", key, "/v", "ProxyServer", "/f").Run()
			} else {
				exec.Command("reg", "add", key, "/v", "ProxyServer", "/t", "REG_SZ", "/d", b.Windows.Server, "/f").Run()
			}
			enable := "0"
			if strings.HasSuffix(strings.TrimSpace(b.Windows.Enable), "1") {
				enable = "1"
			}
			exec.Command("reg", "add", key, "/v", "ProxyEnable", "/t", "REG_DWORD", "/d", enable, "/f").Run()
			winRefreshProxy()
		}
	case "linux":
		if mode, ok := b.Gnome["org.gnome.system.proxy mode"]; ok {
			exec.Command("gsettings", "set", "org.gnome.system.proxy", "mode", strings.Trim(mode, "'")).Run()
		}
		if h, ok := b.Gnome["org.gnome.system.proxy.socks host"]; ok {
			exec.Command("gsettings", "set", "org.gnome.system.proxy.socks", "host", strings.Trim(h, "'")).Run()
		}
		if p, ok := b.Gnome["org.gnome.system.proxy.socks port"]; ok && p != "" {
			exec.Command("gsettings", "set", "org.gnome.system.proxy.socks", "port", p).Run()
		}
	}
	os.Remove(sysProxyBackupPath())
	log.Printf("system proxy restored")
}

func writeSysProxyBackup(b sysProxyBackup) error {
	data, _ := json.MarshalIndent(b, "", "  ")
	os.MkdirAll(dataDir(), 0o700)
	return os.WriteFile(sysProxyBackupPath(), data, 0o600)
}

func macServices() []string {
	out, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return nil
	}
	var svcs []string
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "*") || strings.Contains(l, "asterisk") {
			continue
		}
		svcs = append(svcs, l)
	}
	return svcs
}

func macGetSocks(svc string) macSocksState {
	out, _ := exec.Command("networksetup", "-getsocksfirewallproxy", svc).Output()
	st := macSocksState{}
	for _, l := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "Enabled":
			st.Enabled = v == "Yes"
		case "Server":
			st.Server = v
		case "Port":
			st.Port = v
		}
	}
	return st
}

func regQuery(name string) string {
	out, err := exec.Command("reg", "query", `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`, "/v", name).Output()
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, name) {
			f := strings.Fields(l)
			if len(f) >= 3 {
				return f[len(f)-1]
			}
		}
	}
	return ""
}

// winRefreshProxy tells running programs the WinINet settings changed.
func winRefreshProxy() {
	// PowerShell one-liner calling InternetSetOption(INTERNET_OPTION_SETTINGS_CHANGED, INTERNET_OPTION_REFRESH).
	ps := `$s='[DllImport("wininet.dll")] public static extern bool InternetSetOption(IntPtr h,int o,IntPtr b,int l);'; $t=Add-Type -MemberDefinition $s -Name W -Namespace N -PassThru; $t::InternetSetOption([IntPtr]::Zero,39,[IntPtr]::Zero,0) | Out-Null; $t::InternetSetOption([IntPtr]::Zero,37,[IntPtr]::Zero,0) | Out-Null`
	exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).Run()
}
