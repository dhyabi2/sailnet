// sailgui is the desktop Sailnet client: a small window with connect,
// wallet, status and settings. It runs the same client as `sailnode client`
// and exposes a SOCKS5 proxy and a DNS resolver on loopback for browsers and
// the browser extension.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"github.com/dhyabi2/sail/client"
)

//go:embed Icon.png
var iconPNG []byte

type prefs struct {
	Hops     int    `json:"hops"`
	ExitCC   string `json:"exitCC"`
	Exclude  string `json:"excludeCC"` // exit countries never to use, comma-separated
	Socks    string `json:"socks"`
	Censored bool   `json:"censored"`
	Bridges  string `json:"bridges"`
	Nick     string `json:"nick"`
	RPCURL   string `json:"rpcUrl"`
	RPCKey   string `json:"rpcKey"`
	// NoSysProxy turns off the system proxy: by default the app routes the
	// computer's browsers through its SOCKS port while connected.
	NoSysProxy bool `json:"noSystemProxy"`
}

type ring struct {
	mu    sync.Mutex
	lines []string
}

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.lines = append(r.lines, strings.TrimRight(string(p), "\n"))
	if len(r.lines) > 400 {
		r.lines = r.lines[len(r.lines)-400:]
	}
	r.mu.Unlock()
	return len(p), nil
}

func (r *ring) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.lines)
	if n > 40 {
		return strings.Join(r.lines[n-40:], "\n")
	}
	return strings.Join(r.lines, "\n")
}

func home() string {
	if h := os.Getenv("SAIL_HOME"); h != "" {
		return h
	}
	d, _ := os.UserConfigDir()
	return filepath.Join(d, "Sailnet")
}

func loadPrefs() prefs {
	p := prefs{Hops: 3, Socks: "127.0.0.1:1080", RPCURL: "https://www.sailnet.space/node/api"}
	if b, err := os.ReadFile(filepath.Join(home(), "gui.json")); err == nil {
		json.Unmarshal(b, &p)
	}
	return p
}

func savePrefs(p prefs) {
	b, _ := json.MarshalIndent(p, "", "  ")
	os.WriteFile(filepath.Join(home(), "gui.json"), b, 0o600)
}

func main() {
	a := app.NewWithID("net.sailnet.desktop")
	a.Settings().SetTheme(mono{})
	a.SetIcon(fyne.NewStaticResource("Icon.png", iconPNG))
	w := a.NewWindow("SAILNET")
	w.Resize(fyne.NewSize(460, 720))
	w.SetFixedSize(true)

	os.Setenv("SAIL_HOME", home())
	os.MkdirAll(home(), 0o700)
	logs := &ring{}
	log.SetOutput(client.RedactingWriter{W: logs})
	p := loadPrefs()
	client.RestoreSystemProxy() // a previous run that crashed while connected left the system proxy on: repair first
	key := client.EnsureWallet()
	client.SetNick(p.Nick, key.Address)

	var (
		mu       sync.Mutex
		mgr      *client.Manager
		ln       net.Listener
		starting bool
	)

	title := widget.NewLabelWithStyle("SAILNET", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	state := widget.NewLabelWithStyle("OFF", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	steps := widget.NewLabel("") // what the client is doing, step by step
	steps.Wrapping = fyne.TextWrapWord
	path := widget.NewLabel("")
	path.Wrapping = fyne.TextWrapWord
	balance := widget.NewLabel("")
	addr := widget.NewEntry()
	addr.SetText(key.Address)
	addr.Disable()
	proxy := widget.NewLabel("")
	logView := widget.NewLabel("")
	logView.Wrapping = fyne.TextWrapWord
	logView.TextStyle = fyne.TextStyle{Monospace: true}

	var toggle *widget.Button
	stop := func() {
		mu.Lock()
		defer mu.Unlock()
		if ln != nil {
			ln.Close()
			ln = nil
		}
		if mgr != nil {
			go mgr.Shutdown() // never on the UI thread, never builds a circuit to close it
			mgr = nil
		}
		go client.RestoreSystemProxy()
		state.SetText("OFF")
		steps.SetText("")
		path.SetText("")
		proxy.SetText("")
		toggle.SetText("CONNECT")
		toggle.Enable()
	}
	start := func() {
		mu.Lock()
		if mgr != nil || starting {
			mu.Unlock()
			return
		}
		starting = true
		mu.Unlock()
		// The button answers at once; everything slow (relay list, RTT
		// probes, payment, circuit) runs off the UI thread and reports
		// through the steps line.
		state.SetText("CONNECTING")
		steps.SetText("Starting…")
		toggle.SetText("CONNECTING…")
		toggle.Disable()
		go func() {
			for _, line := range strings.Split(p.Bridges, "\n") {
				if strings.TrimSpace(line) != "" {
					client.AddBridge(strings.TrimSpace(line))
				}
			}
			_, noChain := os.Stat(filepath.Join(home(), "chain-"+key.Address[len(key.Address)-8:]+".json"))
			m := client.NewStealthManagerBootstrap(3, "", "0.0005", "0", "", noChain != nil)
			m.SetExcludeExit(p.Exclude)
			m.SetCensored(true)
			l, err := m.ServeSOCKS(p.Socks)
			if err != nil {
				fyne.Do(func() {
					dialog.ShowError(fmt.Errorf("SOCKS port %s: %v", p.Socks, err), w)
					state.SetText("OFF")
					steps.SetText("")
					toggle.SetText("CONNECT")
					toggle.Enable()
				})
				mu.Lock()
				starting = false
				mu.Unlock()
				return
			}
			go m.ServeDNS("127.0.0.1:5300", "1.1.1.1:53")
			go m.ServeStatus("127.0.0.1:1090")
			mu.Lock()
			mgr, ln = m, l
			starting = false
			mu.Unlock()
			routed := "Browsers: set SOCKS5 " + p.Socks + " yourself (system proxy is off in Settings)"
			if !p.NoSysProxy {
				if err := client.SetSystemProxy(p.Socks); err != nil {
					routed = "Could not set the system proxy (" + err.Error() + "); set SOCKS5 " + p.Socks + " in your browser"
				} else {
					routed = "This computer's browsers now go through Sailnet (system proxy → SOCKS5 " + p.Socks + ")"
				}
			}
			fyne.Do(func() {
				proxy.SetText(routed + "   DNS 127.0.0.1:5300")
				toggle.SetText("DISCONNECT")
				toggle.Enable()
			})
			go m.Circuit()
			go func() { // keep trying; an empty wallet waits for the entry's confirmation push
				for {
					time.Sleep(20 * time.Second)
					mu.Lock()
					cur := mgr
					mu.Unlock()
					if cur != m {
						return
					}
					if c, err := m.Circuit(); err == nil && c != nil {
						m.StopFundsWatch()
					} else if err != nil && strings.Contains(err.Error(), "no XNO") {
						m.EnsureFundsWatch()
					}
				}
			}()
		}()
	}
	toggle = widget.NewButton("CONNECT", func() {
		mu.Lock()
		on := mgr != nil
		busy := starting
		mu.Unlock()
		if busy {
			return
		}
		if on {
			stop()
		} else {
			start()
		}
	})
	newExit := widget.NewButton("NEW EXIT", func() {
		mu.Lock()
		m := mgr
		mu.Unlock()
		if m == nil {
			return
		}
		go func() {
			if c, err := m.Circuit(); err == nil {
				c.Close()
			}
			m.Circuit()
		}()
	})
	copyAddr := widget.NewButton("COPY ADDRESS", func() { w.Clipboard().SetContent(key.Address) })
	refresh := widget.NewButton("REFRESH", nil)
	refresh.OnTapped = func() {
		mu.Lock()
		m := mgr
		mu.Unlock()
		if m == nil {
			return
		}
		refresh.SetText("CHECKING…")
		refresh.Disable()
		go func() {
			b := m.RefreshFunds()
			fyne.Do(func() {
				if b != "" {
					balance.SetText(b + " XNO")
				}
				refresh.SetText("REFRESH")
				refresh.Enable()
			})
		}()
	}
	fundAsked := false
	askFunds := func() {
		msg := widget.NewLabel("This wallet has no XNO yet. Send it a little Nano: 0.0005 XNO buys about 25 MB.\nIt connects by itself the moment the funds confirm.")
		msg.Wrapping = fyne.TextWrapWord
		ad := widget.NewEntry()
		ad.SetText(key.Address)
		ad.Disable()
		open := func(u string) func() {
			return func() {
				if pu, err := parseURL(u); err == nil {
					a.OpenURL(pu)
				}
			}
		}
		links := container.NewGridWithColumns(3,
			widget.NewButton("FREE FAUCET", open("https://hub.nano.org/faucets")),
			widget.NewButton("BINANCE", open("https://www.binance.com/en/trade/XNO_USDT")),
			widget.NewButton("KRAKEN", open("https://www.kraken.com/prices/nano")))
		body := container.NewVBox(msg, ad,
			widget.NewButton("COPY ADDRESS", func() { w.Clipboard().SetContent(key.Address) }), links)
		dialog.ShowCustom("Fund the wallet", "CLOSE", body, w)
	}
	faucets := widget.NewButton("GET XNO", func() {
		u, _ := parseURL("https://hub.nano.org/faucets")
		a.OpenURL(u)
	})

	// Settings
	// Exit exclusion: one checkbox per country the client knows relays in.
	excluded := map[string]bool{}
	for _, c := range strings.Split(p.Exclude, ",") {
		if c = strings.TrimSpace(strings.ToUpper(c)); c != "" {
			excluded[c] = true
		}
	}
	exclBox := container.NewVBox(widget.NewLabel("Never exit through:"))
	var exclChecks []*widget.Check
	countries := client.CachedCountries()
	if len(countries) == 0 {
		exclBox.Add(widget.NewLabel("No relays known yet; connect once, then choose."))
	}
	for _, c := range countries {
		ch := widget.NewCheck(c, nil)
		ch.SetChecked(excluded[c])
		exclChecks = append(exclChecks, ch)
		exclBox.Add(ch)
	}
	socks := widget.NewEntry()
	socks.SetText(p.Socks)
	nick := widget.NewEntry()
	nick.SetPlaceHolder("nickname shown instead of the wallet address")
	nick.SetText(p.Nick)
	bridges := widget.NewMultiLineEntry()
	bridges.SetPlaceHolder("bridge lines, one per line")
	bridges.SetText(p.Bridges)
	bridges.SetMinRowsVisible(3)
	save := widget.NewButton("SAVE", func() {
		var ex []string
		for _, ch := range exclChecks {
			if ch.Checked {
				ex = append(ex, ch.Text)
			}
		}
		p.Exclude = strings.Join(ex, ",")
		mu.Lock()
		if mgr != nil {
			mgr.SetExcludeExit(p.Exclude)
		}
		mu.Unlock()
		p.Socks = strings.TrimSpace(socks.Text)
		p.Nick = strings.TrimSpace(nick.Text)
		p.Bridges = bridges.Text
		savePrefs(p)
		client.SetNick(p.Nick, key.Address)
		dialog.ShowInformation("Saved", "Settings apply to the next connection.", w)
	})
	sysProxy := widget.NewCheck("Route this computer's browsers through Sailnet while connected (system proxy)", func(on bool) {
		p.NoSysProxy = !on
		savePrefs(p)
	})
	sysProxy.SetChecked(!p.NoSysProxy)
	settings := container.NewVBox(
		widget.NewLabelWithStyle("SETTINGS", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		sysProxy,
		exclBox, socks, nick, bridges,
		save,
	)

	top := container.NewVBox(
		title,
		steps,
		widget.NewSeparator(),
		container.NewGridWithColumns(2, state, toggle),
		path,
		proxy,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("WALLET", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		addr,
		balance,
		container.NewGridWithColumns(4, copyAddr, refresh, faucets, newExit),
		widget.NewSeparator(),
		widget.NewLabelWithStyle("LOG", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
	)
	// The log takes every pixel the fixed rows above leave: a border layout
	// gives the centre object the remaining space, unlike a VBox, which gives
	// each child only its minimum height.
	logScroll := container.NewVScroll(logView)
	main := container.NewBorder(top, nil, nil, nil, logScroll)
	tabs := container.NewAppTabs(
		container.NewTabItem("STATUS", main),
		container.NewTabItem("SETTINGS", container.NewVScroll(settings)),
	)
	w.SetContent(tabs)

	go func() {
		lastLog, lastSteps := "", ""
		done := []string{}
		for range time.Tick(time.Second) {
			mu.Lock()
			m := mgr
			mu.Unlock()
			txt := logs.text()
			var st map[string]any
			if m != nil {
				st = m.StatusJSON() // off the UI thread: it may touch caches
			}
			fyne.Do(func() {
				if txt != lastLog { // re-laying out a long label every second is what used to burn CPU
					lastLog = txt
					logView.SetText(txt)
				}
				if m == nil {
					balance.SetText("")
					return
				}
				stage, _ := st["stage"].(string)
				if ps, _ := st["path"].(string); ps != "" {
					state.SetText("ON")
					path.SetText(ps)
				} else {
					state.SetText("CONNECTING")
				}
				if stage != "" && (len(done) == 0 || done[len(done)-1] != stage) {
					done = append(done, stage)
					if len(done) > 6 {
						done = done[len(done)-6:]
					}
				}
				line := ""
				for i, d := range done {
					mark := "✓ "
					if i == len(done)-1 && d != "Connected" {
						mark = "… "
					}
					line += mark + d + "\n"
				}
				if line != lastSteps {
					lastSteps = line
					steps.SetText(strings.TrimRight(line, "\n"))
				}
				if b, _ := st["balance"].(string); b != "" {
					balance.SetText(b + " XNO")
				}
				if nf, _ := st["needsFunds"].(bool); nf {
					state.SetText("WAITING FOR XNO")
					path.SetText("Send a little XNO to the wallet below: 0.0005 XNO buys about 25 MB. It connects by itself when the funds confirm.")
					if !fundAsked {
						fundAsked = true
						askFunds()
					}
				}
			})
		}
	}()
	w.SetCloseIntercept(func() { stop(); client.RestoreSystemProxy(); a.Quit() })
	w.ShowAndRun()
}
