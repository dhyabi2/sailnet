package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// A relay upgrade replaces a binary on a live machine and then hands it to
// systemd. These tests cover the two steps that decide whether that is safe:
// noticing that what was installed cannot run, and updating `sail` beside it
// without ever installing something the operator did not already have.

func TestSmokeTestAcceptsARealBuildAndRejectsRubbish(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "sailnode")
	build := exec.Command("go", "build", "-o", good, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building sailnode: %v\n%s", err, out)
	}
	if err := smokeTest(good); err != nil {
		t.Fatalf("a real build should pass the smoke test, got: %v", err)
	}

	// What a truncated download or a wrong-architecture asset looks like from
	// here: a file that is executable and simply is not our binary.
	bad := filepath.Join(dir, "rubbish")
	if err := os.WriteFile(bad, []byte("\x7fELF not really\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := smokeTest(bad); err == nil {
		t.Fatal("a binary that cannot run should fail the smoke test")
	}

	// A script that runs and says nothing is still not sailnode.
	quiet := filepath.Join(dir, "quiet")
	if err := os.WriteFile(quiet, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := smokeTest(quiet); err == nil {
		t.Fatal("a binary that is not sailnode should fail the smoke test")
	}
}

// serveAssets publishes bodies by asset name, with the .sha256 that accompanies
// each one, exactly as the release workflow does.
func serveAssets(t *testing.T, bodies map[string]string) map[string]string {
	t.Helper()
	mux := http.NewServeMux()
	urls := map[string]string{}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	for name, body := range bodies {
		body := body
		sum := sha256.Sum256([]byte(body))
		mux.HandleFunc("/"+name, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, body)
		})
		mux.HandleFunc("/"+name+".sha256", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), name)
		})
		urls[name] = srv.URL + "/" + name
		urls[name+".sha256"] = srv.URL + "/" + name + ".sha256"
	}
	return urls
}

func sailAssetName() string {
	name := fmt.Sprintf("sail-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func TestUpgradeCompanionReplacesAnInstalledSail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sail")
	if err := os.WriteFile(path, []byte("old sail"), 0o755); err != nil {
		t.Fatal(err)
	}
	upgradeCompanion(dir, serveAssets(t, map[string]string{sailAssetName(): "new sail"}))

	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new sail" {
		t.Fatalf("sail should have been replaced, got %q (%v)", got, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("the replacement should be executable, got mode %v", info.Mode())
	}
}

func TestUpgradeCompanionAddsNothingThatWasNotThere(t *testing.T) {
	dir := t.TempDir()
	upgradeCompanion(dir, serveAssets(t, map[string]string{sailAssetName(): "new sail"}))
	if _, err := os.Stat(filepath.Join(dir, "sail")); !os.IsNotExist(err) {
		t.Fatal("an upgrade must not install a command the operator never had")
	}
}

func TestUpgradeCompanionLeavesSailAloneOnAChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sail")
	if err := os.WriteFile(path, []byte("old sail"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A checksum asset that does not describe the body served beside it.
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	asset := sailAssetName()
	mux.HandleFunc("/"+asset, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "tampered") })
	mux.HandleFunc("/"+asset+".sha256", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(make([]byte, 32)), asset)
	})
	upgradeCompanion(dir, map[string]string{asset: srv.URL + "/" + asset, asset + ".sha256": srv.URL + "/" + asset + ".sha256"})

	got, err := os.ReadFile(path)
	if err != nil || string(got) != "old sail" {
		t.Fatalf("unverified code must never be installed, got %q (%v)", got, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("the failed download should have been cleaned up, dir holds %d entries", len(entries))
	}
}
