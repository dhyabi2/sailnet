package main

import (
	"fmt"
	"io"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"github.com/dhyabi2/sail/client"
)

// The wallet section of the settings tab: write the seed down, and put it
// back.
//
// Sailnet holds no copy of anyone's key, so an app that cannot show you your
// seed is an app that can lose your money for good — a reinstall on a phone,
// a wiped laptop, a restored machine. These two buttons are the whole
// recovery story, so they are plain and hard to get wrong.
//
// connected reports whether a tunnel is up. Restoring is refused while it
// is, because circuits in flight are prepaid from the wallet being replaced.
func walletSection(w fyne.Window, connected func() bool) fyne.CanvasObject {
	backup := widget.NewButton("BACK UP WALLET", func() {
		seed, addr, err := client.ExportWallet()
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		seedBox := widget.NewMultiLineEntry()
		seedBox.SetText(seed)
		seedBox.Wrapping = fyne.TextWrapBreak
		seedBox.SetMinRowsVisible(2)
		body := container.NewVBox(
			widget.NewLabel("This seed is your wallet. Anyone who has it can spend\nyour balance, and nobody can recover it for you if you\nlose it. Write it down and keep it off this computer."),
			widget.NewLabel("Address: "+addr),
			seedBox,
			container.NewGridWithColumns(2,
				widget.NewButton("COPY SEED", func() { w.Clipboard().SetContent(seed) }),
				widget.NewButton("SAVE TO FILE...", func() { saveSeed(w, seed, addr) }),
			),
		)
		dialog.ShowCustom("Back up your wallet", "DONE", body, w)
	})

	restore := widget.NewButton("RESTORE WALLET", func() {
		if connected() {
			dialog.ShowError(fmt.Errorf("disconnect first: the open circuits are paid for from the wallet you are replacing"), w)
			return
		}
		entry := widget.NewMultiLineEntry()
		entry.SetPlaceHolder("Paste the 64-character seed from your backup")
		entry.Wrapping = fyne.TextWrapBreak
		entry.SetMinRowsVisible(3)
		body := container.NewVBox(
			widget.NewLabel("Restoring replaces the wallet on this computer.\nThe one it replaces is kept beside it, not deleted."),
			entry,
			widget.NewButton("LOAD FROM FILE...", func() { loadSeed(w, entry) }),
		)
		d := dialog.NewCustomConfirm("Restore a wallet", "RESTORE", "CANCEL", body, func(ok bool) {
			if !ok {
				return
			}
			addr, err := client.ImportWallet(entry.Text)
			if err != nil {
				dialog.ShowError(err, w)
				return
			}
			dialog.ShowInformation("Wallet restored", addr+"\n\nQuit and open Sailnet again to use it.", w)
		}, w)
		d.Resize(fyne.NewSize(460, 300))
		d.Show()
	})

	return container.NewVBox(
		widget.NewSeparator(),
		widget.NewLabelWithStyle("WALLET", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel("Your balance lives in one seed. Back it up before you put\nmoney in it: no server keeps a copy."),
		container.NewGridWithColumns(2, backup, restore),
	)
}

func saveSeed(w fyne.Window, seed, addr string) {
	dialog.ShowFileSave(func(wc fyne.URIWriteCloser, err error) {
		if err != nil || wc == nil {
			return
		}
		defer wc.Close()
		fmt.Fprintf(wc, "Sailnet wallet backup\naddress: %s\nseed: %s\n\nAnyone holding this seed holds the balance. Keep it somewhere safe.\n", addr, seed)
		dialog.ShowInformation("Saved", "Keep this file somewhere only you can read.", w)
	}, w)
}

func loadSeed(w fyne.Window, entry *widget.Entry) {
	dialog.ShowFileOpen(func(rc fyne.URIReadCloser, err error) {
		if err != nil || rc == nil {
			return
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		entry.SetText(strings.TrimSpace(string(b)))
	}, w)
}
