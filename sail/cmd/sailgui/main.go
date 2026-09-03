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
	Socks    string `json:"socks"`
	Censored bool   `json:"censored"`
	Bridges  string `json:"bridges"`
	Nick     string `json:"nick"`
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
	p := prefs{Hops: 3, Socks: "127.0.0.1:1080"}
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
	w.Resize(fyne.NewSize(420, 560))
	w.SetFixedSize(true)

	os.Setenv("SAIL_HOME", home())
	os.MkdirAll(home(), 0o700)
	logs := &ring{}
	log.SetOutput(client.RedactingWriter{W: logs})
	p := loadPrefs()
	key := client.EnsureWallet()
	client.SetNick(p.Nick, key.Address)

	var (
		mu  sync.Mutex
		mgr *client.Manager
		ln  net.Listener
	)

	title := widget.NewLabelWithStyle("SAILNET", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	state := widget.NewLabelWithStyle("OFF", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
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
			if c, err := mgr.Circuit(); err == nil && c != nil {
				c.Close()
			}
			mgr = nil
		}
		state.SetText("OFF")
		path.SetText("")
		proxy.SetText("")
		toggle.SetText("CONNECT")
	}
	start := func() {
		mu.Lock()
		defer mu.Unlock()
		if mgr != nil {
			return
		}
		for _, line := range strings.Split(p.Bridges, "\n") {
			if strings.TrimSpace(line) != "" {
				client.AddBridge(strings.TrimSpace(line))
			}
		}
		_, noChain := os.Stat(filepath.Join(home(), "chain-"+key.Address[len(key.Address)-8:]+".json"))
		m := client.NewStealthManager(p.Hops, p.ExitCC, "0.0005", "0", "")
		if noChain != nil {
			m.AllowDirectBootstrap(true)
		}
		if p.Censored {
			m.SetCensored(true)
		}
		l, err := m.ServeSOCKS(p.Socks)
		if err != nil {
			dialog.ShowError(fmt.Errorf("SOCKS port %s: %v", p.Socks, err), w)
			return
		}
		go m.ServeDNS("127.0.0.1:5300", "1.1.1.1:53")
		go m.ServeStatus("127.0.0.1:1090")
		mgr, ln = m, l
		state.SetText("CONNECTING")
		proxy.SetText("SOCKS5 " + p.Socks + "   DNS 127.0.0.1:5300")
		toggle.SetText("DISCONNECT")
		go m.Circuit()
	}
	toggle = widget.NewButton("CONNECT", func() {
		mu.Lock()
		on := mgr != nil
		mu.Unlock()
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
	faucets := widget.NewButton("GET XNO", func() {
		u, _ := parseURL("https://hub.nano.org/faucets")
		a.OpenURL(u)
	})

	// Settings
	hops := widget.NewSelect([]string{"2", "3", "4"}, nil)
	hops.SetSelected(fmt.Sprint(p.Hops))
	exit := widget.NewEntry()
	exit.SetPlaceHolder("exit country, e.g. DE (blank = any)")
	exit.SetText(p.ExitCC)
	socks := widget.NewEntry()
	socks.SetText(p.Socks)
	nick := widget.NewEntry()
	nick.SetPlaceHolder("nickname shown instead of the wallet address")
	nick.SetText(p.Nick)
	censored := widget.NewCheck("Censored network: bridges only, no probes", nil)
	censored.SetChecked(p.Censored)
	bridges := widget.NewMultiLineEntry()
	bridges.SetPlaceHolder("bridge lines, one per line")
	bridges.SetText(p.Bridges)
	bridges.SetMinRowsVisible(3)
	save := widget.NewButton("SAVE", func() {
		fmt.Sscan(hops.Selected, &p.Hops)
		p.ExitCC = strings.ToUpper(strings.TrimSpace(exit.Text))
		p.Socks = strings.TrimSpace(socks.Text)
		p.Nick = strings.TrimSpace(nick.Text)
		p.Censored = censored.Checked
		p.Bridges = bridges.Text
		savePrefs(p)
		client.SetNick(p.Nick, key.Address)
		dialog.ShowInformation("Saved", "Settings apply to the next connection.", w)
	})
	settings := container.NewVBox(
		widget.NewLabelWithStyle("SETTINGS", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		container.NewGridWithColumns(2, widget.NewLabel("Hops"), hops),
		exit, socks, nick, censored, bridges, save,
	)

	main := container.NewVBox(
		title,
		widget.NewSeparator(),
		container.NewGridWithColumns(2, state, toggle),
		path,
		proxy,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("WALLET", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		addr,
		balance,
		container.NewGridWithColumns(3, copyAddr, faucets, newExit),
		widget.NewSeparator(),
		widget.NewLabelWithStyle("LOG", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		container.NewVScroll(logView),
	)
	tabs := container.NewAppTabs(
		container.NewTabItem("STATUS", main),
		container.NewTabItem("SETTINGS", container.NewVScroll(settings)),
	)
	w.SetContent(tabs)

	go func() {
		for range time.Tick(time.Second) {
			mu.Lock()
			m := mgr
			mu.Unlock()
			txt := logs.text()
			fyne.Do(func() {
				logView.SetText(txt)
				if m == nil {
					balance.SetText("")
					return
				}
				st := m.StatusJSON()
				if ps, _ := st["path"].(string); ps != "" {
					state.SetText("ON")
					path.SetText(ps)
				} else {
					state.SetText("CONNECTING")
				}
				if b, _ := st["balance"].(string); b != "" {
					balance.SetText(b + " XNO")
				}
			})
		}
	}()
	w.SetCloseIntercept(func() { stop(); a.Quit() })
	w.ShowAndRun()
}
