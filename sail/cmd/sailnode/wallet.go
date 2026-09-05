package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/dhyabi2/sail/client"
)

// runWallet implements `sailnode wallet export|import|where`.
//
// A relay's earnings live in one seed. Operators move machines, rebuild
// servers and restore snapshots, and there is nobody who can give them the
// key back if it is gone, so exporting it has to be one obvious command.
func runWallet(args []string) {
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
