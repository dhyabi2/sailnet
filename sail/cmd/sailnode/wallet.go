package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dhyabi2/sail/client"
)

// useServiceHome points this command at the data directory the installed
// sailnode service actually uses.
//
// A relay run by systemd usually keeps its wallet somewhere like
// /var/lib/sail, set through the unit, while a shell has no SAIL_HOME at all
// and would look in the operator's home directory. Without this, an operator
// asking to back up their relay's wallet would be told there is no wallet,
// or shown the wrong one, which is exactly the moment they can least afford
// a wrong answer.
func useServiceHome() {
	if os.Getenv("SAIL_HOME") != "" || os.Getenv("SAIL_WALLET") != "" || client.HasWallet() {
		return
	}
	unit, err := exec.Command("systemctl", "cat", "sailnode").Output()
	if err != nil {
		body, err2 := os.ReadFile("/etc/systemd/system/sailnode.service")
		if err2 != nil {
			return
		}
		unit = body
	}
	home, wallet := serviceWallet(string(unit))
	if wallet == "" {
		return
	}
	if _, err := os.Stat(wallet); err != nil {
		return // the unit names a wallet that is not there: answer for the default path
	}
	if home != "" {
		os.Setenv("SAIL_HOME", home)
	}
	os.Setenv("SAIL_WALLET", wallet)
	fmt.Fprintln(os.Stderr, "using the wallet of the installed sailnode service:", wallet)
}

// serviceWallet reads a systemd unit and reports the data directory and
// wallet file it configures, following an EnvironmentFile when the unit
// keeps its settings in one.
func serviceWallet(unit string) (home, wallet string) {
	settings := map[string]string{}
	read := func(text string) {
		for _, l := range strings.Split(text, "\n") {
			l = strings.TrimSpace(l)
			for _, key := range []string{"SAIL_HOME", "SAIL_WALLET"} {
				if _, after, ok := strings.Cut(l, key+"="); ok {
					v, _, _ := strings.Cut(after, " ")
					if v = strings.Trim(v, `"'`); v != "" {
						settings[key] = v
					}
				}
			}
		}
	}
	read(unit)
	for _, l := range strings.Split(unit, "\n") {
		if _, after, ok := strings.Cut(strings.TrimSpace(l), "EnvironmentFile="); ok {
			if body, err := os.ReadFile(strings.TrimPrefix(strings.TrimSpace(after), "-")); err == nil {
				read(string(body))
			}
		}
	}
	home, wallet = settings["SAIL_HOME"], settings["SAIL_WALLET"]
	if wallet == "" && home != "" {
		wallet = filepath.Join(home, "wallet.json")
	}
	return home, wallet
}

// runWallet implements `sailnode wallet export|import|where`.
//
// A relay's earnings live in one seed. Operators move machines, rebuild
// servers and restore snapshots, and there is nobody who can give them the
// key back if it is gone, so exporting it has to be one obvious command.
func runWallet(args []string) {
	useServiceHome()
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "export", "backup", "show":
		seed, addr, err := client.ExportWallet()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("address:", addr)
		fmt.Println("seed:   ", seed)
		fmt.Println()
		fmt.Println("Write the seed down and keep it off this machine. Anyone who has it")
		fmt.Println("has the money in it, and nobody can recover it for you if you lose it.")
		fmt.Println("Restore it later with:  sailnode wallet import <seed>")

	case "import", "restore":
		text := strings.Join(args[1:], " ")
		if strings.TrimSpace(text) == "" || text == "-" {
			b, _ := io.ReadAll(os.Stdin) // allow: sailnode wallet import < backup.json
			text = string(b)
		} else if b, err := os.ReadFile(strings.TrimSpace(text)); err == nil {
			text = string(b) // a path to a saved wallet file works too
		}
		addr, err := client.ImportWallet(text)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("restored:", addr)
		fmt.Println("stored at", client.WalletPath())
		fmt.Println("any wallet that was there is kept beside it as .replaced-<time>")
		fmt.Println("restart the relay to use it:  systemctl restart sailnode")

	case "where":
		fmt.Println(client.WalletPath())

	default:
		fmt.Println("usage: sailnode wallet export|import <seed|file|->|where")
		os.Exit(2)
	}
}
