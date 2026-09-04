// wsprobe subscribes to a Nano WebSocket and prints what comes back.
package main

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/net/websocket"
)

func main() {
	url := "wss://ws.nano.to"
	if len(os.Args) > 1 {
		url = os.Args[1]
	}
	ws, err := websocket.Dial(url, "", "https://sailnet.space")
	if err != nil {
		fmt.Println("dial:", err)
		return
	}
	defer ws.Close()
	msgs := []string{
		`{"action":"subscribe","topic":"confirmation","ack":true,"id":"a","options":{"accounts":["nano_1cexacexqrr51coik9eeqzebmfej7g6chrd1eygmzm9ad86qek84pnhpda1t"]}}`,
		`{"action":"ping"}`,
	}
	for _, m := range msgs {
		if err := websocket.Message.Send(ws, m); err != nil {
			fmt.Println("send:", err)
			return
		}
	}
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		ws.SetReadDeadline(deadline)
		var s string
		if err := websocket.Message.Receive(ws, &s); err != nil {
			fmt.Println("recv:", err)
			return
		}
		if len(s) > 300 {
			s = s[:300] + "…"
		}
		fmt.Println("<-", s)
	}
}
