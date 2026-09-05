package main

import (
	"os"
	"path/filepath"
	"testing"
)

// An operator asking to back up their relay's wallet runs the command from a
// shell, where SAIL_HOME is not set; the service keeps the wallet somewhere
// else entirely. Answering "no wallet" there would be the worst possible
// wrong answer, so the unit is read to find the real one.
func TestServiceWalletIsFoundFromTheUnit(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")
	os.WriteFile(envFile, []byte("SAIL_HOME=/var/lib/sail\nSAIL_WALLET=/var/lib/sail/wallet.json\nEXTRA=\"\"\n"), 0o600)

	cases := []struct{ name, unit, home, wallet string }{
		{
			"settings in an environment file, as our installer writes it",
			"[Service]\nEnvironmentFile=" + envFile + "\nExecStart=/usr/local/bin/sailnode relay\n",
			"/var/lib/sail", "/var/lib/sail/wallet.json",
		},
		{
			"settings inline in the unit",
			"[Service]\nEnvironment=SAIL_HOME=/srv/sail\nExecStart=/usr/local/bin/sailnode relay\n",
			"/srv/sail", "/srv/sail/wallet.json",
		},
		{
			"a wallet path on its own, no home",
			"[Service]\nEnvironment=\"SAIL_WALLET=/etc/sail/key.json\"\n",
			"", "/etc/sail/key.json",
		},
		{
			"an optional environment file that does not exist",
			"[Service]\nEnvironmentFile=-/nowhere/env\n",
			"", "",
		},
		{
			"a unit that configures nothing: fall back to the default",
			"[Service]\nExecStart=/usr/local/bin/sailnode relay --rate 5\n",
			"", "",
		},
		{"empty", "", "", ""},
	}
	for _, c := range cases {
		home, wallet := serviceWallet(c.unit)
		if home != c.home || wallet != c.wallet {
			t.Errorf("%s:\n got home=%q wallet=%q\nwant home=%q wallet=%q", c.name, home, wallet, c.home, c.wallet)
		}
	}
}

// Discovery never overrides what the operator asked for explicitly.
func TestExplicitSettingsWin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SAIL_HOME", dir)
	useServiceHome()
	if got := os.Getenv("SAIL_HOME"); got != dir {
		t.Fatalf("SAIL_HOME was changed to %q", got)
	}
}
