package client

import (
	"os"
	"path/filepath"
	"testing"
)

// A relay on a fresh box has no wallet, and the README says it makes one.
// It did not: the relay was the only entry point that loaded a wallet
// instead of ensuring one, so a first-time operator got a crash-looping
// service. The client, the desktop app and the Docker entrypoint all
// created one, which is why this survived so long — the Docker quickstart
// worked and the binary quickstart did not.
func TestFirstRunCreatesAWalletAndKeepsIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SAIL_HOME", home)
	t.Setenv("SAIL_WALLET", "")

	if HasWallet() {
		t.Fatal("a fresh data directory should have no wallet")
	}
	addr, created, err := CreateWalletIfMissing()
	if err != nil || !created || addr == "" {
		t.Fatalf("first run: addr=%q created=%v err=%v", addr, created, err)
	}
	if _, err := os.Stat(filepath.Join(home, "wallet.json")); err != nil {
		t.Fatalf("wallet was not written where the node looks: %v", err)
	}

	// Starting again must reuse it. A relay that minted a new seed on every
	// restart would strand its earnings on an address nobody is watching.
	addr2, created2, err := CreateWalletIfMissing()
	if err != nil || created2 {
		t.Fatalf("second run created another wallet: created=%v err=%v", created2, err)
	}
	if addr2 != addr {
		t.Fatalf("address changed across restarts: %s then %s", addr, addr2)
	}
}

// Under systemd there is no HOME unless the unit sets one. os.UserHomeDir
// then returns "" with an error, and discarding it produced the relative
// path ".sail/wallet.json" — resolved against the unit's working directory,
// so the wallet went somewhere nobody would look. A data directory is
// always absolute.
func TestDataDirIsAbsoluteWithoutHome(t *testing.T) {
	t.Setenv("SAIL_HOME", "")
	os.Unsetenv("SAIL_HOME")
	t.Setenv("HOME", "")
	os.Unsetenv("HOME")

	// Run from a directory with no legacy .sail in it.
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	d := dataDir()
	if !filepath.IsAbs(d) {
		t.Fatalf("data directory %q is relative: a service would write its wallet wherever it happened to start", d)
	}
}

// ...unless a node really has been running on the old relative path, in
// which case its wallet is there and correcting the default must not move
// money out from under it.
func TestLegacyRelativeWalletIsKept(t *testing.T) {
	t.Setenv("SAIL_HOME", "")
	os.Unsetenv("SAIL_HOME")
	t.Setenv("HOME", "")
	os.Unsetenv("HOME")

	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd) })
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(".sail", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".sail", "wallet.json"), []byte(`{"seed":"00","index":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if d := dataDir(); d != ".sail" {
		t.Fatalf("data directory %q; an existing wallet at .sail must keep being used", d)
	}
}
