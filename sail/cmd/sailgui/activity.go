package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dhyabi2/sail/client"
)

// The Activity tab: which local process is using the proxy, and how much.
// The client meters every SOCKS connection by its source port; the owner of
// that port is asked from the OS (lsof on macOS, /proc on Linux, netstat on
// Windows) and cached. Nothing here leaves the machine.

type flow struct {
	SrcPort int    `json:"srcPort"`
	Dst     string `json:"dst"`
	Up      int64  `json:"up"`
	Down    int64  `json:"down"`
	Last    int64  `json:"last"`
	Open    bool   `json:"open"`
}

var (
	ownerMu    sync.Mutex
	ownerCache = map[int]string{}
)

// ownerOf names the process that opened the local port, or "" if unknown.
func ownerOf(port int) string {
	ownerMu.Lock()
	if n, ok := ownerCache[port]; ok {
		ownerMu.Unlock()
		return n
	}
	ownerMu.Unlock()
	name := ""
	switch runtime.GOOS {
	case "darwin":
		out, _ := exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:ESTABLISHED", "-Fc").Output()
		for _, l := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(l, "c") && !strings.Contains(l, "sailgui") {
				name = l[1:]
				break
			}
		}
	case "windows":
		out, _ := exec.Command("netstat", "-ano", "-p", "TCP").Output()
		re := regexp.MustCompile(`\s+TCP\s+\S+:` + strconv.Itoa(port) + `\s+\S+\s+ESTABLISHED\s+(\d+)`)
		if m := re.FindStringSubmatch(string(out)); m != nil {
			task, _ := exec.Command("tasklist", "/FI", "PID eq "+m[1], "/FO", "CSV", "/NH").Output()
			if f := strings.Split(strings.TrimSpace(string(task)), ","); len(f) > 0 {
				name = strings.Trim(f[0], "\"")
			}
		}
	case "linux":
		out, _ := exec.Command("ss", "-tnpH", "sport = :"+strconv.Itoa(port)).Output()
		re := regexp.MustCompile(`users:\(\("([^"]+)"`)
		if m := re.FindStringSubmatch(string(out)); m != nil {
			name = m[1]
		}
	}
	if name != "" {
		ownerMu.Lock()
		ownerCache[port] = name
		ownerMu.Unlock()
	}
	return name
}

// activityText renders the per-process table.
func activityText() string {
	var fl []flow
	json.Unmarshal([]byte(client.Flows()), &fl)
	type agg struct {
		up, down int64
		n, open  int
	}
	by := map[string]*agg{}
	for _, f := range fl {
		name := ownerOf(f.SrcPort)
		if name == "" {
			if f.Open {
				name = "unknown process"
			} else {
				name = "closed connections (process gone)"
			}
		}
		a := by[name]
		if a == nil {
			a = &agg{}
			by[name] = a
		}
		a.up += f.Up
		a.down += f.Down
		a.n++
		if f.Open {
			a.open++
		}
	}
	names := make([]string, 0, len(by))
	for n := range by {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return by[names[i]].up+by[names[i]].down > by[names[j]].up+by[names[j]].down })
	var b strings.Builder
	fmt.Fprintf(&b, "Last 10 minutes, by process. %d connections through the proxy.\n\n", len(fl))
	if len(names) == 0 {
		b.WriteString("No traffic yet. Point a browser at the SOCKS5 proxy.")
	}
	for _, n := range names {
		a := by[n]
		fmt.Fprintf(&b, "%s\n   ↑ %s   ↓ %s   %d connections", n, human(a.up), human(a.down), a.n)
		if a.open > 0 {
			fmt.Fprintf(&b, ", %d active", a.open)
		}
		b.WriteString("\n\n")
	}
	return b.String()
}

func human(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.2f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.0f kB", float64(n)/1e3)
	}
	return fmt.Sprintf("%d B", n)
}

var _ = time.Second
