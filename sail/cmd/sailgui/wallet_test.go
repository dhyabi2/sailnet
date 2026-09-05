package main

import (
	"reflect"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"github.com/dhyabi2/sail/client"
)

// walk visits every object in a tree, including the ones inside containers
// and scrollers, so a test can find a button or read what is on screen.
// walk visits every object in a tree, including the ones inside containers,
// scrollers and the popup a dialog renders into, so a test can find a button
// or read what is on screen. Fyne wraps things in several private types, so
// this reaches into any Content or Objects field it finds.
func walk(o fyne.CanvasObject, fn func(fyne.CanvasObject)) {
	seen := map[fyne.CanvasObject]bool{}
	var rec func(fyne.CanvasObject)
	rec = func(o fyne.CanvasObject) {
		if o == nil || seen[o] {
			return
		}
		seen[o] = true
		fn(o)
		v := reflect.ValueOf(o)
		for v.Kind() == reflect.Ptr && !v.IsNil() {
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			return
		}
		var visit func(reflect.Value)
		visit = func(f reflect.Value) {
			if !f.IsValid() || !f.CanInterface() {
				return
			}
			if f.Kind() == reflect.Slice {
				for i := 0; i < f.Len(); i++ {
					visit(f.Index(i))
				}
				return
			}
			if child, ok := f.Interface().(fyne.CanvasObject); ok {
				rec(child)
			}
		}
		for _, name := range []string{"Content", "Objects", "content", "win", "Children"} {
			visit(v.FieldByName(name))
		}
		// A container embedded by value or by pointer, whatever it is called.
		for i := 0; i < v.NumField(); i++ {
			visit(v.Field(i))
		}
	}
	rec(o)
}

func findButton(root fyne.CanvasObject, label string) *widget.Button {
	var found *widget.Button
	walk(root, func(o fyne.CanvasObject) {
		if b, ok := o.(*widget.Button); ok && b.Text == label && found == nil {
			found = b
		}
	})
	return found
}

// onScreen collects the text of everything currently displayed, the dialog
// on top included.
func onScreen(w fyne.Window) string {
	var b strings.Builder
	collect := func(o fyne.CanvasObject) {
		switch v := o.(type) {
		case *widget.Label:
			b.WriteString(v.Text + "\n")
		case *widget.Entry:
			b.WriteString(v.Text + "\n")
		case *widget.Button:
			b.WriteString(v.Text + "\n")
		}
	}
	walk(w.Content(), collect)
	for _, o := range w.Canvas().Overlays().List() {
		walk(o, collect)
	}
	return b.String()
}

func closeOverlays(w fyne.Window) {
	for w.Canvas().Overlays().Top() != nil {
		w.Canvas().Overlays().Remove(w.Canvas().Overlays().Top())
	}
}

// The two buttons a person's money depends on, driven the way a person
// drives them: built, tapped, and checked for what appears. A panic or a
// missing dialog here is somebody unable to back up their wallet.
func TestWalletButtonsWork(t *testing.T) {
	t.Setenv("SAIL_HOME", t.TempDir())
	t.Setenv("SAIL_WALLET", "")
	addr, _, err := client.CreateWalletIfMissing()
	if err != nil {
		t.Fatal(err)
	}
	seed, _, err := client.ExportWallet()
	if err != nil {
		t.Fatal(err)
	}
	a := test.NewApp()
	defer a.Quit()
	w := test.NewWindow(nil)
	defer w.Close()
	section := walletSection(w, func() bool { return false })
	w.SetContent(section)

	backup, restore := findButton(section, "BACK UP WALLET"), findButton(section, "RESTORE WALLET")
	if backup == nil || restore == nil {
		t.Fatal("the wallet section is missing its buttons")
	}

	test.Tap(backup)
	shown := onScreen(w)
	if !strings.Contains(shown, addr) {
		t.Fatalf("the backup dialog does not show the address:\n%s", shown)
	}
	if !strings.Contains(shown, seed) {
		t.Fatalf("the backup dialog does not show the seed:\n%s", shown)
	}
	closeOverlays(w)

	test.Tap(restore)
	if shown := onScreen(w); !strings.Contains(shown, "replaces the wallet") {
		t.Fatalf("the restore dialog did not open:\n%s", shown)
	}
	closeOverlays(w)
}

// While a tunnel is up, restoring is refused rather than half-done: the
// open circuits are prepaid out of the wallet being replaced.
func TestRestoreIsRefusedWhileConnected(t *testing.T) {
	t.Setenv("SAIL_HOME", t.TempDir())
	t.Setenv("SAIL_WALLET", "")
	if _, _, err := client.CreateWalletIfMissing(); err != nil {
		t.Fatal(err)
	}
	a := test.NewApp()
	defer a.Quit()
	w := test.NewWindow(nil)
	defer w.Close()
	section := walletSection(w, func() bool { return true }) // connected
	w.SetContent(section)
	test.Tap(findButton(section, "RESTORE WALLET"))
	if shown := onScreen(w); !strings.Contains(shown, "Disconnect first") {
		t.Fatalf("restoring while connected was not refused:\n%s", shown)
	}
}
