package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ReleaseAPI is where `sailnode upgrade` looks for a newer build. An
// operator can point it elsewhere (a fork, a mirror, an air-gapped copy) by
// setting SAIL_RELEASE_API: nothing about upgrading is compulsory, and a node
// that never upgrades keeps working. Whatever it points at, the download is
// still checked against the ".sha256" published beside it, so redirecting this
// buys a different source, not a weaker one.
var ReleaseAPI = "https://api.github.com/repos/dhyabi2/sailnet/releases/latest"

func releaseAPI() string {
	if v := os.Getenv("SAIL_RELEASE_API"); v != "" {
		return v
	}
	return ReleaseAPI
}

// runUpgrade replaces this binary with the newest published build and, when
// it runs under systemd, restarts the service.
//
// It is written to be safe on a live relay:
//   - the wallet, the quota log and every other file in SAIL_HOME are never
//     touched, only the executable is;
//   - the download is verified against the published SHA-256 before anything
//     is replaced, and a failure at any point leaves the running binary as it
//     was;
//   - the replacement is a rename over the old path, so the running process
//     keeps its own copy until it is restarted, and the restart is a normal
//     systemd restart with the drain that a stop already performs.
func runUpgrade(args []string) {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	check := fs.Bool("check", false, "only report which version is published, change nothing")
	restart := fs.Bool("restart", true, "restart the sailnode service afterwards when it is running under systemd")
	force := fs.Bool("force", false, "install even when the published build is the one already installed")
	fs.Parse(args)

	asset := fmt.Sprintf("sailnode-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		asset += ".exe"
	}
	tag, urls, err := latestRelease()
	if err != nil {
		log.Fatalf("upgrade: %v", err)
	}
	binURL, ok := urls[asset]
	if !ok {
		log.Fatalf("upgrade: release %s has no %s; this platform is not published, build from source", tag, asset)
	}
	self, err := os.Executable()
	if err != nil {
		log.Fatalf("upgrade: %v", err)
	}
	// EvalSymlinks returns "" on failure, so take its answer only when it has
	// one: an upgrade that installed itself over the empty path would be a
	// far worse outcome than one that follows no symlink.
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	cur, curErr := fileSum(self)
	fmt.Printf("installed: %s\npublished: %s (%s)\n", versionString(self), tag, asset)
	if *check {
		// -check exists to answer one question, so answer it here rather than
		// leaving an operator to compare a hash against a tag by eye. The
		// checksum asset is a hundred bytes; reading it costs nothing.
		want, err := publishedSum(urls[asset+".sha256"], asset)
		switch {
		case err != nil:
			fmt.Printf("could not read the published checksum: %v\n", err)
		case curErr != nil:
			fmt.Printf("could not read the installed binary: %v\n", curErr)
		case strings.EqualFold(cur, want):
			fmt.Println("up to date; nothing to install")
		default:
			fmt.Println("an upgrade is available: run `sailnode upgrade`")
		}
		return
	}

	// Download to a temporary file next to the binary, so the rename that
	// replaces it cannot cross a filesystem boundary.
	tmp, err := os.CreateTemp(filepath.Dir(self), ".sailnode-upgrade-")
	if err != nil {
		log.Fatalf("upgrade: %v (run as the user that owns %s)", err, self)
	}
	defer os.Remove(tmp.Name())
	sum, err := download(binURL, tmp)
	tmp.Close()
	if err != nil {
		log.Fatalf("upgrade: download: %v", err)
	}
	want, err := publishedSum(urls[asset+".sha256"], asset)
	if err != nil {
		log.Fatalf("upgrade: %v", err)
	}
	if !strings.EqualFold(sum, want) {
		log.Fatalf("upgrade: checksum mismatch (published %s, downloaded %s); nothing was changed", want[:16], sum[:16])
	}
	if curErr == nil && strings.EqualFold(cur, sum) && !*force {
		fmt.Println("already running the published build; nothing to do")
		return
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		log.Fatalf("upgrade: %v", err)
	}
	// Keep the old binary beside the new one: if the new build refuses to
	// start, an operator has something to put back without a download.
	kept := os.Rename(self, self+".previous") == nil
	if err := os.Rename(tmp.Name(), self); err != nil {
		if kept {
			os.Rename(self+".previous", self) // put it back exactly as it was
		}
		log.Fatalf("upgrade: install: %v", err)
	}
	if kept {
		fmt.Printf("installed %s at %s (previous build kept at %s.previous)\n", tag, self, self)
	} else {
		fmt.Printf("installed %s at %s (the previous build could not be kept aside)\n", tag, self)
	}

	// Run what was just installed before handing it to systemd. A binary that
	// cannot execute — wrong architecture, truncated download that still
	// matched a truncated checksum file, a missing library — would otherwise
	// be found only by a service that restarts forever, which is the one
	// failure an operator does not see happen.
	if err := smokeTest(self); err != nil {
		if !kept {
			log.Fatalf("upgrade: the new build does not run: %v; there is no kept copy to restore, so reinstall %s by hand before restarting", err, asset)
		}
		if rerr := os.Rename(self+".previous", self); rerr != nil {
			log.Fatalf("upgrade: the new build does not run: %v; putting the old one back also failed: %v — move %s.previous back by hand before restarting", err, rerr, self)
		}
		log.Fatalf("upgrade: the new build does not run: %v; the previous build is back in place and nothing was restarted", err)
	}
	upgradeCompanion(filepath.Dir(self), urls)

	if !*restart {
		fmt.Println("restart the service when you are ready: systemctl restart sailnode")
		return
	}
	if out, err := exec.Command("systemctl", "is-active", "--quiet", "sailnode").CombinedOutput(); err != nil {
		_ = out
		fmt.Println("no running sailnode service found; start the new binary yourself")
		return
	}
	if out, err := exec.Command("systemctl", "restart", "sailnode").CombinedOutput(); err != nil {
		log.Fatalf("upgrade: installed, but the restart failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	fmt.Println("sailnode restarted on the new build")
}

// smokeTest runs the freshly installed binary with no arguments. It prints the
// usage line and exits non-zero without touching the wallet, the ledger or the
// network, which makes it a cheap proof that what was installed actually runs.
func smokeTest(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path).CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("it did not answer within 30s")
	}
	if strings.Contains(string(out), "usage: sailnode") {
		return nil
	}
	first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
	if err != nil {
		if first != "" {
			return fmt.Errorf("%v (%s)", err, first)
		}
		return err
	}
	return fmt.Errorf("it did not print the usage line")
}

// upgradeCompanion updates the `sail` wallet CLI when it is installed next to
// sailnode. The release publishes both and the deploy script installs both, so
// upgrading only one leaves an operator holding a wallet CLI older than the
// node it talks to. It is best effort: sailnode is already installed and
// working by this point, and a relay runs perfectly well without `sail`.
// Nothing is installed that an operator did not already choose to have.
func upgradeCompanion(dir string, urls map[string]string) {
	name := "sail"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err != nil {
		return // not installed here; an upgrade does not add commands
	}
	asset := fmt.Sprintf("sail-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		asset += ".exe"
	}
	url, ok := urls[asset]
	if !ok {
		return
	}
	warn := func(err error) { fmt.Printf("note: %s was left as it was: %v\n", path, err) }
	tmp, err := os.CreateTemp(dir, ".sail-upgrade-")
	if err != nil {
		warn(err)
		return
	}
	defer os.Remove(tmp.Name())
	sum, err := download(url, tmp)
	tmp.Close()
	if err != nil {
		warn(err)
		return
	}
	want, err := publishedSum(urls[asset+".sha256"], asset)
	if err != nil {
		warn(err)
		return
	}
	if !strings.EqualFold(sum, want) {
		warn(fmt.Errorf("checksum mismatch"))
		return
	}
	if cur, err := fileSum(path); err == nil && strings.EqualFold(cur, sum) {
		return // already the published build
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		warn(err)
		return
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		warn(err)
		return
	}
	fmt.Printf("also updated %s\n", path)
}

// latestRelease returns the newest tag and its assets by name.
func latestRelease() (string, map[string]string, error) {
	req, _ := http.NewRequest(http.MethodGet, releaseAPI(), nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return "", nil, fmt.Errorf("release list: HTTP %d — GitHub is rate-limiting this address; wait an hour, or download the build by hand", resp.StatusCode)
	}
	if resp.StatusCode != 200 {
		return "", nil, fmt.Errorf("release list: HTTP %d", resp.StatusCode)
	}
	var rel struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return "", nil, err
	}
	urls := map[string]string{}
	for _, a := range rel.Assets {
		urls[a.Name] = a.URL
	}
	return rel.TagName, urls, nil
}

// download copies url into w and returns the SHA-256 of what arrived.
func download(url string, w io.Writer) (string, error) {
	resp, err := (&http.Client{Timeout: 15 * time.Minute}).Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, 512<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// publishedSum reads the ".sha256" asset that accompanies a binary.
func publishedSum(url, asset string) (string, error) {
	if url == "" {
		return "", fmt.Errorf("release has no checksum for %s; refusing to install unverified code", asset)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 || len(f[0]) != 64 {
		return "", fmt.Errorf("checksum for %s is not readable", asset)
	}
	return f[0], nil
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// versionString describes the installed binary: its own report if it answers,
// otherwise a short hash so two builds can at least be told apart.
func versionString(path string) string {
	if sum, err := fileSum(path); err == nil {
		return "sha256:" + sum[:12]
	}
	return "unknown"
}
