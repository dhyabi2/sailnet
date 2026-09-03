#!/bin/sh
# Packages the extension for both stores: out/sailnet-chrome.zip and
# out/sailnet-firefox.zip. The Firefox build swaps in manifest.firefox.json
# (event-page background, gecko id); the code is shared and detects the browser.
set -e
cd "$(dirname "$0")"
rm -rf out && mkdir -p out/chrome out/firefox
for f in background.js popup.html popup.js icon16.png icon48.png icon128.png; do cp "$f" out/chrome/; cp "$f" out/firefox/; done
cp manifest.json out/chrome/manifest.json
cp manifest.firefox.json out/firefox/manifest.json
(cd out/chrome && zip -qr ../sailnet-chrome.zip .)
(cd out/firefox && zip -qr ../sailnet-firefox.zip .)
ls -la out/*.zip
