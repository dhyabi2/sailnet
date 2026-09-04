// stuncheck sends a STUN binding request over UDP and prints the mapped
// address and round trip: the same thing WebRTC voice (calls, X Spaces,
// Discord) does before it can talk. Used to verify UDP through the tunnel.
package main

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	servers := []string{"stun.l.google.com:19302", "stun.cloudflare.com:3478", "stun1.l.google.com:19302"}
	if len(os.Args) > 1 {
		servers = os.Args[1:]
	}
	for _, s := range servers {
		fmt.Printf("%-28s ", s)
		c, err := net.DialTimeout("udp", s, 8*time.Second)
		if err != nil {
			fmt.Println("dial:", err)
			continue
		}
		req := make([]byte, 20)
		binary.BigEndian.PutUint16(req[0:], 0x0001) // binding request
		binary.BigEndian.PutUint32(req[4:], 0x2112A442)
		rand.Read(req[8:20])
		t0 := time.Now()
		c.SetDeadline(time.Now().Add(8 * time.Second))
		c.Write(req)
		buf := make([]byte, 512)
		n, err := c.Read(buf)
		c.Close()
		if err != nil {
			fmt.Println("no answer:", err)
			continue
		}
		mapped := ""
		for i := 20; i+4 <= n; {
			t := binary.BigEndian.Uint16(buf[i:])
			l := int(binary.BigEndian.Uint16(buf[i+2:]))
			if i+4+l > n {
				break
			}
			v := buf[i+4 : i+4+l]
			if (t == 0x0020 || t == 0x0001) && len(v) >= 8 && v[1] == 1 {
				port := binary.BigEndian.Uint16(v[2:])
				ip := make([]byte, 4)
				copy(ip, v[4:8])
				if t == 0x0020 {
					port ^= 0x2112
					for k := range ip {
						ip[k] ^= req[4+k]
					}
				}
				mapped = fmt.Sprintf("%d.%d.%d.%d:%d", ip[0], ip[1], ip[2], ip[3], port)
			}
			i += 4 + (l+3)/4*4
		}
		fmt.Printf("ok in %s, mapped %s\n", time.Since(t0).Round(time.Millisecond), mapped)
	}
}
