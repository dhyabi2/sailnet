# Sailnet for Android

A VPN app that routes the whole device through the Sailnet circuit: the Go client runs inside the app (gomobile), a userspace network stack (tun2socks core) reads the TUN interface, every TCP flow is opened through a 3-hop circuit, and DNS queries are answered through the circuit at the exit. No SOCKS setup, no root.

## What the app does
1. Creates a wallet in the app's private storage on first launch and shows its address. Send a little XNO to it (0.01 XNO is weeks of use at 0.00002 XNO/MiB).
2. On Connect, asks Android for VPN permission, opens a TUN interface (10.8.0.2/32, all routes, DNS 10.8.0.1), and starts the client with the three VPS relays as bridges (`res/raw/bridges.txt`).
3. Pays the entry relay 0.0005 XNO per anchor, builds the circuit, and shows the path, wallet balance and a log.
4. The app's own sockets are protected (VpnService.protect) so they leave outside the tunnel; everything else goes through it.

First run reaches the Nano ledger directly once to learn the wallet state; after that the client is in stealth mode (no Nano RPC on the local network).

## Build
Requirements: Go 1.25, gomobile, Android SDK 34 + NDK 26, JDK 17, Gradle 8.14.

```
cd sail
gomobile bind -target android/arm64,android/amd64 -androidapi 24 -javapkg net.sailnet -o ../android/app/libs/sail.aar ./mobile
cd ../android
gradle assembleDebug
# app/build/outputs/apk/debug/app-debug.apk
```

Install with `adb install -r app/build/outputs/apk/debug/app-debug.apk`, or copy the APK to the phone and open it (allow installs from unknown sources).

## Limits
- UDP other than DNS is dropped (QUIC falls back to TCP in browsers; VoIP apps will not work).
- IPv4 only.
- Debug-signed APK; sideload only.
