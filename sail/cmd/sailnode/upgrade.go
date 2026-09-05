package main

import (
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
// operator can point it elsewhere (a fork, a mirror, an air-gapped copy):
// nothing about upgrading is compulsory, and a node that never upgrades
// keeps working.
var ReleaseAPI = "https://api.github.com/repos/dhyabi2/sailnet/releases/latest"

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
	self, _ = filepath.EvalSymlinks(self)
	fmt.Printf("installed: %s\npublished: %s (%s)\n", versionString(self), tag, asset)
	if *check {
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
	if cur, err := fileSum(self); err == nil && strings.EqualFold(cur, sum) && !*force {
		fmt.Println("already running the published build; nothing to do")
		return
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		log.Fatalf("upgrade: %v", err)
	}
	// Keep the old binary beside the new one: if the new build refuses to
	// start, an operator has something to put back without a download.
	os.Rename(self, self+".previous")
	if err := os.Rename(tmp.Name(), self); err != nil {
		os.Rename(self+".previous", self) // put it back exactly as it was
		log.Fatalf("upgrade: install: %v", err)
	}
	fmt.Printf("installed %s at %s (previous build kept at %s.previous)\n", tag, self, self)

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

// latestRelease returns the newest tag and its assets by name.
func latestRelease() (string, map[string]string, error) {
	req, _ := http.NewRequest(http.MethodGet, ReleaseAPI, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
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
