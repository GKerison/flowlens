<p align="center">
  <img src="docs/assets/guanliu-icon.svg" width="120" alt="GuanLiu icon" />
</p>

# GuanLiu (FlowLens)

A cross-platform HTTP/HTTPS debugging workstation. A native desktop client: start the proxy, install the CA, scan the QR code on your phone, and read plaintext traffic in three steps.

[中文](./README.md) · [Requirements](./REQUIREMENTS.md) · [Conventions](./CONVENTIONS.md) · [Manual test checklist](./docs/manual-test.md)

> Status: M1–M15 delivered. Ready for daily use, with bilingual UI, three interface styles, and three desktop platforms.

## Quick start

1. **Start the proxy** from the top bar. It listens on `0.0.0.0:8888` by default, so a phone on the same Wi-Fi can connect directly. For local-only use, change the listen address to `127.0.0.1` in Settings.
2. **Install the certificate**: Settings → Certificate → scan the QR code to open the download page. **On iPhone, after installing the profile you must also enable full trust under Settings → General → About → Certificate Trust Settings**, or Safari will report that the server identity cannot be verified.
3. **Connect your phone**: set the HTTP proxy of the phone's Wi-Fi to the local/LAN address shown in the top bar (click the address to copy credentials are included automatically when authentication is enabled).
4. **Read plaintext**: browse any HTTPS site on the phone; the session appears in the list and details are shown on the right.

Pausing capture turns the proxy into a **plain relay**: no decryption, no recording, and no browsing disruption. Rewrite rules (Map Local, etc.) are paused too. Resume with one click — no restart needed.

## Features

- **Proxy & TLS**: self-signed CA, one-click system trust install/uninstall, certificate distribution page with QR code, HTTP Basic proxy authentication, per-domain decryption bypass
- **Traffic analysis**: list / domain grouping, sortable columns, URL and response-body search, UTF-8/GBK charset detection, WebSocket/SSE frames, JSON/image/form previews
- **Data tools**: HAR/JSON export (save-as), request replay with editing, Map Local, Map Remote, request/response breakpoints
- **Mobile onboarding**: virtual NIC address filtering, usable proxy address dropdown, self-check, iOS full-trust guidance and verification link
- **Experience**: bilingual UI (Chinese by default), standard / spacious / minimal styles, interface animations, config import/export

## Build

Requires Go 1.26+ and Node.js 18+.

```bash
# Development mode
wails3 task dev

# Build for the current platform
wails3 task build

# Package the current platform
wails3 task package

# Server mode (no GUI, HTTP-served UI)
wails3 task build:server
bin/flowlens-server
```

See `Taskfile.yml` and `build/<platform>/Taskfile.yml` for cross-compilation options.

## Branching & release policy

- `main` is the only releasable branch: **develop every change on a feature branch, merge into main, then tag to release**
- Pushing a `v*` tag triggers GitHub Actions to build macOS, Windows and Linux artifacts and publish a Release
- See the branching workflow section in `CONVENTIONS.md`

## Security notes

- The CA private key is stored with 0600 permissions and never exposed to the UI or logs
- `.p12` export requires confirmation, an 8+ character password, and 0600 file permissions
- Install the root certificate only on your own devices: installing it allows the holder of the private key to decrypt that device's HTTPS traffic
- The proxy listens on `0.0.0.0` by default; **enabling authentication is strongly recommended in LAN mode**

## License

[MIT](./LICENSE) · Third-party dependency licenses are retained with their modules; the vendored `third_party/go-mitmproxy` keeps its original license and patch markers.
