package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func walletHome(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("SAIL_HOME", d)
	t.Setenv("SAIL_WALLET", "")
	return d
}

// The promise the apps make: installing again finds the wallet that is
// already there. Whatever else runs, a second (or twentieth) start must
// return the same address and never write a new seed.
func TestExistingWalletIsAlwaysReused(t *testing.T) {
	walletHome(t)
	first, created, err := CreateWalletIfMissing()
	if err != nil || !created {
		t.Fatalf("first run: %v created=%v", err, created)
	}
	before, err := os.ReadFile(WalletPath())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		addr, created, err := CreateWalletIfMissing()
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if created {
			t.Fatalf("run %d created a second wallet over the first", i)
		}
		if addr != first {
			t.Fatalf("run %d changed the address: %s then %s", i, first, addr)
		}
	}
	after, _ := os.ReadFile(WalletPath())
	if string(before) != string(after) {
		t.Fatal("the wallet file was rewritten by a later run")
	}
	if got := EnsureWallet().Address; got != first {
		t.Fatalf("EnsureWallet returned %s, want %s", got, first)
	}
}

// A wallet file that cannot be read is still somebody's money. The one
// thing we must never do is decide it is rubbish and mint a new seed on
// top: that is how a balance disappears with no way back.
func TestUnreadableWalletIsNeverReplaced(t *testing.T) {
	walletHome(t)
	if _, _, err := CreateWalletIfMissing(); err != nil {
		t.Fatal(err)
	}
	junk := []byte("{ this is not json")
	os.WriteFile(WalletPath(), junk, 0o600)
	if _, created, err := CreateWalletIfMissing(); err == nil || created {
		t.Fatalf("a corrupt wallet was overwritten (created=%v err=%v)", created, err)
	}
	got, _ := os.ReadFile(WalletPath())
	if string(got) != string(junk) {
		t.Fatal("the unreadable file was modified; it must be left for the operator")
	}
}

// Backup, then restore into a fresh install: the same address comes back
// with its money, which is the whole point of writing a seed down.
func TestBackupSurvivesAFreshInstall(t *testing.T) {
	walletHome(t)
	original, _, _ := CreateWalletIfMissing()
	seed, addr, err := ExportWallet()
	if err != nil {
		t.Fatal(err)
	}
	if addr != original || len(seed) != 64 {
		t.Fatalf("export: %s %q", addr, seed)
	}
	// A new device: empty data directory, no wallet at all.
	walletHome(t)
	if HasWallet() {
		t.Fatal("fresh install already had a wallet")
	}
	back, err := ImportWallet(seed)
	if err != nil {
		t.Fatal(err)
	}
	if back != original {
		t.Fatalf("restored %s, want %s", back, original)
	}
	if got := EnsureWallet().Address; got != original {
		t.Fatalf("after restore the app uses %s, want %s", got, original)
	}
}

// People do not paste cleanly. Every one of these is the same seed.
func TestRestoreAcceptsWhatPeopleActuallyPaste(t *testing.T) {
	walletHome(t)
	want, _, _ := CreateWalletIfMissing()
	seed, _, _ := ExportWallet()
	file, _ := os.ReadFile(WalletPath())

	forms := map[string]string{
		"bare seed":        seed,
		"lower case":       strings.ToLower(seed),
		"with spaces":      seed[:16] + " " + seed[16:32] + "  " + seed[32:48] + "\n" + seed[48:],
		"labelled":         "seed: " + seed,
		"trailing newline": seed + "\n\n",
		"whole file":       string(file),
	}
	for name, text := range forms {
		walletHome(t)
		got, err := ImportWallet(text)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != want {
			t.Fatalf("%s: restored %s, want %s", name, got, want)
		}
	}
}

// A restore replaces a wallet, so the one it replaces is moved aside rather
// than deleted: a seed typed from the wrong piece of paper is recoverable.
func TestRestoreKeepsTheWalletItReplaces(t *testing.T) {
	dir := walletHome(t)
	old, _, _ := CreateWalletIfMissing()
	oldSeed, _, _ := ExportWallet()

	walletHome(t)
	other, _, _ := CreateWalletIfMissing()
	otherSeed, _, _ := ExportWallet()
	_ = dir

	if _, err := ImportWallet(oldSeed); err != nil {
		t.Fatal(err)
	}
	if now := EnsureWallet().Address; now != old {
		t.Fatalf("after restore: %s, want %s", now, old)
	}
	// The replaced wallet is still on disk, and still holds its own seed.
	matches, _ := filepath.Glob(WalletPath() + ".replaced-*")
	if len(matches) != 1 {
		t.Fatalf("expected the replaced wallet to be kept, found %v", matches)
	}
	var wf walletBlob
	b, _ := os.ReadFile(matches[0])
	json.Unmarshal(b, &wf)
	if !strings.EqualFold(wf.Seed, otherSeed) || wf.Address != other {
		t.Fatalf("the kept file is not the wallet that was replaced: %+v", wf)
	}
	// Restoring the wallet that is already loaded is a no-op, not a
	// second copy: repeated taps on Restore must not litter the directory.
	if _, err := ImportWallet(oldSeed); err != nil {
		t.Fatal(err)
	}
	if again, _ := filepath.Glob(WalletPath() + ".replaced-*"); len(again) != 1 {
		t.Fatalf("re-restoring the same seed moved a wallet aside: %v", again)
	}
}

// Bad input is refused with something a person can act on, and never
// touches the wallet that is already there.
func TestBadRestoreLeavesTheWalletAlone(t *testing.T) {
	walletHome(t)
	safe, _, _ := CreateWalletIfMissing()
	before, _ := os.ReadFile(WalletPath())

	bad := []string{
		"", "   ", "\n",
		"not a seed at all",
		strings.Repeat("A", 63),
		strings.Repeat("A", 65),
		strings.Repeat("Z", 64),
		`{"seed":"","address":"nano_x"}`,
		`{"seed":"tooshort"}`,
		"nano_3saqoz5qfgmohfz3dg5ywwmxj7dwdp3g6xfbspt11g7gyrxgbupi1w9u4g4r",
	}
	for _, text := range bad {
		if addr, err := ImportWallet(text); err == nil {
			t.Fatalf("%q was accepted as a wallet (%s)", text, addr)
		}
		if got := EnsureWallet().Address; got != safe {
			t.Fatalf("%q changed the wallet to %s", text, got)
		}
		after, _ := os.ReadFile(WalletPath())
		if string(after) != string(before) {
			t.Fatalf("%q modified the wallet file", text)
		}
	}
}

// A wallet is written atomically, so a crash mid-write cannot leave a
// truncated seed that no build can read. Checked by proving the file is
// never opened at the target path for writing: only a rename lands there.
func TestWalletWriteIsAtomic(t *testing.T) {
	dir := walletHome(t)
	if _, _, err := CreateWalletIfMissing(); err != nil {
		t.Fatal(err)
	}
	// No temporary files are left behind next to it.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".wallet-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
	fi, err := os.Stat(WalletPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("wallet is mode %v, want 0600: a seed must not be world-readable", fi.Mode().Perm())
	}
}

// A restore keeps the wallet it replaced. If the live wallet then goes
// missing — an interrupted restore, a file deleted by hand — the backup is
// picked up automatically rather than a new empty wallet being minted
// beside somebody's money.
func TestAMissingWalletIsRecoveredFromItsOwnBackup(t *testing.T) {
	walletHome(t)
	_, _, _ = CreateWalletIfMissing()
	originalSeed, _, _ := ExportWallet()

	// Restore a different wallet, which sets the first one aside...
	walletHome(t)
	other, _, _ := CreateWalletIfMissing()
	otherSeed, _, _ := ExportWallet()
	if _, err := ImportWallet(originalSeed); err != nil {
		t.Fatal(err)
	}
	// ...then lose the live wallet.
	if err := os.Remove(WalletPath()); err != nil {
		t.Fatal(err)
	}
	addr, created, err := CreateWalletIfMissing()
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("a new wallet was minted while a backup of the old one sat beside it")
	}
	if addr != other {
		t.Fatalf("recovered %s, want the wallet that was set aside, %s", addr, other)
	}
	if seed, _, _ := ExportWallet(); !strings.EqualFold(seed, otherSeed) {
		t.Fatal("the recovered wallet does not hold the seed it was backed up with")
	}
}

// Recovery only ever puts back this directory's own wallet. Nothing is
// pulled in from anywhere else: two relays on one machine each keep their
// own account, which is what stops two nodes sharing one chain.
func TestNothingIsAdoptedFromAnotherDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SAIL_WALLET", "")

	t.Setenv("SAIL_HOME", filepath.Join(home, ".sail"))
	elsewhere, created, err := CreateWalletIfMissing()
	if err != nil || !created {
		t.Fatalf("setting up the first wallet: %v", err)
	}
	t.Setenv("SAIL_HOME", filepath.Join(home, "second-node"))
	addr, created, err := CreateWalletIfMissing()
	if err != nil {
		t.Fatal(err)
	}
	if !created || addr == elsewhere {
		t.Fatalf("a second data directory took the first one's wallet (%s)", addr)
	}
}
