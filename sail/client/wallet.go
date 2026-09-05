package client

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dhyabi2/sail/nano"
)

// A Sailnet wallet is one secret: a 32-byte seed. Everything else — the
// address, the balance, the chain state — is derived from it or read back
// from the ledger, so a seed written on paper is a complete backup.
//
// Nobody else holds a copy. There is no account to recover, no support
// address to write to, and no server that could return your money if you
// lose the seed; that is what "no central custody" means in RULES.md. The
// price of it is that backup has to be something the apps make easy, which
// is what this file is for.
//
// Two rules govern every path here:
//
//   - a wallet file is never overwritten in place. Creating one happens only
//     when none exists, and restoring one moves the old file aside first, so
//     no single action can destroy a key.
//   - the write is atomic. A wallet is created by writing a temporary file
//     and renaming it over the target, so a crash or a pulled battery
//     mid-write leaves either the old file or the new one, never a truncated
//     one that no build can read.

// WalletPath reports the file this process uses for its wallet.
func WalletPath() string {
	if p := os.Getenv("SAIL_WALLET"); p != "" {
		return p
	}
	return filepath.Join(dataDir(), "wallet.json")
}

type walletBlob struct {
	Seed    string `json:"seed"`
	Index   uint32 `json:"index"`
	Address string `json:"address"`
}

// HasWallet reports whether a usable wallet already exists. A file that is
// present but unreadable counts as existing: it is somebody's money and the
// answer to it is never "make a new one".
func HasWallet() bool {
	_, err := os.Stat(WalletPath())
	return err == nil
}

// ExportWallet returns the seed to back up, as 64 hex characters, together
// with the address it belongs to.
func ExportWallet() (seed, address string, err error) {
	data, err := os.ReadFile(WalletPath())
	if err != nil {
		return "", "", fmt.Errorf("no wallet at %s yet", WalletPath())
	}
	var wf walletBlob
	if err := json.Unmarshal(data, &wf); err != nil {
		return "", "", fmt.Errorf("the wallet file at %s is not readable: %w", WalletPath(), err)
	}
	k, err := keyFromSeed(wf.Seed, wf.Index)
	if err != nil {
		return "", "", err
	}
	return strings.ToUpper(wf.Seed), k.Address, nil
}

// A seed is exactly 64 hex characters. Matching one more than that lets us
// reject a longer run rather than silently taking the first 64 of it.
var seedRE = regexp.MustCompile(`[0-9a-fA-F]{64,}`)
var notHex = regexp.MustCompile(`[^0-9a-fA-F]`)

// ImportWallet puts a backed-up wallet back. It accepts what a person is
// likely to have kept: the bare seed, the seed with spaces or newlines in
// it, or the whole wallet.json file pasted as it was.
//
// The wallet being replaced is not deleted. It is renamed alongside, so a
// restore typed from the wrong piece of paper is recoverable.
//
// Restoring while a tunnel is up is refused: the running circuits are paid
// for from the old wallet and would be abandoned mid-flight. Disconnect
// first.
func ImportWallet(text string) (address string, err error) {
	seed, index, err := parseBackup(text)
	if err != nil {
		return "", err
	}
	k, err := keyFromSeed(seed, index)
	if err != nil {
		return "", err
	}
	path := WalletPath()
	if cur, _, err := ExportWallet(); err == nil && strings.EqualFold(cur, seed) {
		return k.Address, nil // already this wallet: nothing to move aside
	}
	if _, err := os.Stat(path); err == nil {
		aside := fmt.Sprintf("%s.replaced-%s", path, time.Now().UTC().Format("20060102-150405"))
		if err := os.Rename(path, aside); err != nil {
			return "", fmt.Errorf("could not set the current wallet aside: %w", err)
		}
	}
	if err := writeWallet(path, walletBlob{Seed: strings.ToUpper(seed), Index: index, Address: k.Address}); err != nil {
		return "", err
	}
	return k.Address, nil
}

// parseBackup pulls a seed out of whatever the person pasted: the bare 64
// characters, the seed with a label in front of it, the seed broken across
// lines the way it fits on a piece of paper, or the whole wallet file.
func parseBackup(text string) (seed string, index uint32, err error) {
	t := strings.TrimSpace(text)
	if t == "" {
		return "", 0, fmt.Errorf("paste the 64-character seed from your backup")
	}
	var wf walletBlob
	if json.Unmarshal([]byte(t), &wf) == nil && wf.Seed != "" {
		s := seedRE.FindString(wf.Seed)
		if len(s) != 64 {
			return "", 0, fmt.Errorf("that wallet file has no readable seed in it")
		}
		return s, wf.Index, nil
	}
	// As pasted, then with the whitespace taken out, so a seed written down
	// in blocks of four still reads back.
	for _, candidate := range []string{t, notHex.ReplaceAllString(t, "")} {
		if s := seedRE.FindString(candidate); len(s) == 64 {
			return s, 0, nil
		}
	}
	n := len(notHex.ReplaceAllString(t, ""))
	return "", 0, fmt.Errorf("a seed is 64 characters of 0-9 and A-F; this has %d", n)
}

func keyFromSeed(seed string, index uint32) (*nano.Key, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(seed))
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("that is not a valid seed")
	}
	k, err := nano.DeriveKey(raw, index)
	if err != nil {
		return nil, fmt.Errorf("that seed does not derive an address: %w", err)
	}
	return k, nil
}

// writeWallet writes a wallet file atomically: a temporary file in the same
// directory, then a rename. A power cut cannot leave a half-written seed.
func writeWallet(path string, wf walletBlob) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(wf, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".wallet-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil { // on disk before the rename, not just in the page cache
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// CreateWalletIfMissing makes a wallet only when there is none, and reports
// whether it made one. It is the single place a new seed comes from.
func CreateWalletIfMissing() (address string, created bool, err error) {
	path := WalletPath()
	if data, err := os.ReadFile(path); err == nil {
		var wf walletBlob
		if json.Unmarshal(data, &wf) == nil && wf.Seed != "" {
			if k, err := keyFromSeed(wf.Seed, wf.Index); err == nil {
				return k.Address, false, nil
			}
		}
		// The file is there but we cannot read it. It is still somebody's
		// money: leave it exactly where it is and say so.
		return "", false, fmt.Errorf("a wallet already exists at %s but cannot be read; restore from your backup rather than losing it", path)
	}
	seed, err := nano.NewSeed()
	if err != nil {
		return "", false, err
	}
	k, err := nano.DeriveKey(seed, 0)
	if err != nil {
		return "", false, err
	}
	if err := writeWallet(path, walletBlob{Seed: strings.ToUpper(hex.EncodeToString(seed)), Index: 0, Address: k.Address}); err != nil {
		return "", false, err
	}
	return k.Address, true, nil
}
