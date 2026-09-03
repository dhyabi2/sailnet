// Package automap makes a home node reachable: it asks the router to forward a
// port with NAT-PMP (also answered by PCP-capable routers in compatibility
// mode) and then UPnP IGD, learns the public IP, renews the lease, and reports
// when the public IP changes. Modelled on the usual home-router automap approach, with the parts
// its issue tracker lists as missing: retransmits, CGNAT detection and a
// clear "not reachable" verdict so the caller can fall back to a reverse tunnel.
package automap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway1"
	"github.com/huin/goupnp/dcps/internetgateway2"
	natpmp "github.com/jackpal/go-nat-pmp"
)

// Mapping is an established port forward.
type Mapping struct {
	Protocol     string // "natpmp" or "upnp"
	PublicIP     net.IP
	ExternalPort uint16
	InternalPort uint16
	Lease        time.Duration
	CGNAT        bool // public IP is itself private/shared: the mapping cannot be reached from the internet
}

// Gateway returns the default IPv4 gateway (best effort, per OS).
func Gateway() (net.IP, error) {
	var out []byte
	var err error
	switch runtime.GOOS {
	case "darwin":
		out, err = exec.Command("route", "-n", "get", "default").Output()
		if err == nil {
			for _, l := range strings.Split(string(out), "\n") {
				if f := strings.Fields(l); len(f) == 2 && f[0] == "gateway:" {
					return net.ParseIP(f[1]), nil
				}
			}
		}
	case "linux":
		out, err = exec.Command("ip", "route", "show", "default").Output()
		if err == nil {
			if f := strings.Fields(string(out)); len(f) >= 3 && f[0] == "default" && f[1] == "via" {
				return net.ParseIP(f[2]), nil
			}
		}
	case "windows":
		out, err = exec.Command("cmd", "/c", "route print 0.0.0.0").Output()
		if err == nil {
			for _, l := range strings.Split(string(out), "\n") {
				if f := strings.Fields(l); len(f) >= 3 && f[0] == "0.0.0.0" && f[1] == "0.0.0.0" {
					return net.ParseIP(f[2]), nil
				}
			}
		}
	}
	// fallback: first hop of the local IP's /24
	if ip := LocalIP(); ip != nil {
		g := ip.To4()
		g[3] = 1
		return g, nil
	}
	return nil, errors.New("no default gateway found")
}

// LocalIP returns the IPv4 used to reach the internet.
func LocalIP() net.IP {
	c, err := net.Dial("udp", "1.1.1.1:53")
	if err != nil {
		return nil
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP
}

// IsShared reports whether an IP is private, CGNAT (100.64/10) or otherwise unroutable.
func IsShared(ip net.IP) bool {
	if ip == nil || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return true
	}
	_, cg, _ := net.ParseCIDR("100.64.0.0/10")
	return cg.Contains(ip)
}

// Map tries NAT-PMP (3 attempts with backoff) then UPnP IGD v2 then v1 to
// forward port → port for `lease`. Returns a Mapping or an error that says why
// the node cannot be reached (caller should fall back to home/reverse mode).
func Map(ctx context.Context, port uint16, lease time.Duration) (*Mapping, error) {
	var errs []string
	if gw, err := Gateway(); err == nil {
		for attempt := 0; attempt < 3; attempt++ {
			c := natpmp.NewClientWithTimeout(gw, 2*time.Second<<uint(attempt))
			ext, err := c.GetExternalAddress()
			if err != nil {
				errs = append(errs, "natpmp: "+err.Error())
				continue
			}
			res, err := c.AddPortMapping("tcp", int(port), int(port), int(lease.Seconds()))
			if err != nil {
				errs = append(errs, "natpmp map: "+err.Error())
				continue
			}
			pub := net.IPv4(ext.ExternalIPAddress[0], ext.ExternalIPAddress[1], ext.ExternalIPAddress[2], ext.ExternalIPAddress[3])
			return &Mapping{Protocol: "natpmp", PublicIP: pub, ExternalPort: res.MappedExternalPort, InternalPort: port, Lease: time.Duration(res.PortMappingLifetimeInSeconds) * time.Second, CGNAT: IsShared(pub)}, nil
		}
	} else {
		errs = append(errs, err.Error())
	}
	local := LocalIP()
	if local == nil {
		return nil, errors.New("no local IP")
	}
	// UPnP IGD v2
	if clients, _, err := internetgateway2.NewWANIPConnection2ClientsCtx(ctx); err == nil && len(clients) > 0 {
		c := clients[0]
		if err := c.AddPortMappingCtx(ctx, "", port, "TCP", port, local.String(), true, "sailnet", uint32(lease.Seconds())); err == nil {
			ext, _ := c.GetExternalIPAddressCtx(ctx)
			pub := net.ParseIP(ext)
			return &Mapping{Protocol: "upnp", PublicIP: pub, ExternalPort: port, InternalPort: port, Lease: lease, CGNAT: IsShared(pub)}, nil
		} else {
			errs = append(errs, "upnp2: "+err.Error())
		}
	}
	if clients, _, err := internetgateway1.NewWANIPConnection1ClientsCtx(ctx); err == nil && len(clients) > 0 {
		c := clients[0]
		if err := c.AddPortMappingCtx(ctx, "", port, "TCP", port, local.String(), true, "sailnet", uint32(lease.Seconds())); err == nil {
			ext, _ := c.GetExternalIPAddressCtx(ctx)
			pub := net.ParseIP(ext)
			return &Mapping{Protocol: "upnp", PublicIP: pub, ExternalPort: port, InternalPort: port, Lease: lease, CGNAT: IsShared(pub)}, nil
		} else {
			errs = append(errs, "upnp1: "+err.Error())
		}
	} else if err != nil {
		errs = append(errs, "upnp discover: "+err.Error())
	}
	return nil, fmt.Errorf("router did not accept a port mapping (%s)", strings.Join(errs, "; "))
}

// PublicIPViaProbe asks a public echo service what IP we appear from (used to
// detect CGNAT even when the router hands out a mapping happily).
func PublicIPViaProbe(ctx context.Context) (net.IP, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	c, err := d.DialContext(ctx, "tcp", "api.ipify.org:80")
	if err != nil {
		return nil, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(8 * time.Second))
	fmt.Fprintf(c, "GET / HTTP/1.0\r\nHost: api.ipify.org\r\n\r\n")
	buf := make([]byte, 4096)
	n, _ := c.Read(buf)
	body := string(buf[:n])
	if i := strings.Index(body, "\r\n\r\n"); i >= 0 {
		body = strings.TrimSpace(body[i+4:])
	}
	ip := net.ParseIP(body)
	if ip == nil {
		return nil, errors.New("no ip in probe reply")
	}
	return ip, nil
}

// Renew keeps a mapping alive until ctx ends, re-mapping every lease/2 and
// calling onChange when the public IP moves.
func Renew(ctx context.Context, port uint16, lease time.Duration, current *Mapping, onChange func(*Mapping)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(lease / 2):
		}
		m, err := Map(ctx, port, lease)
		if err != nil {
			continue
		}
		if current == nil || !m.PublicIP.Equal(current.PublicIP) || m.ExternalPort != current.ExternalPort {
			current = m
			onChange(m)
		}
	}
}
