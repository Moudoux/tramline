# Architecture

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

### Language: Go

- It builds to a single static ARM64 binary, about 7.5 MB.
- Goroutines suit one serial port with temperature reports, commands and client requests running together.

### Build and dependencies

Run `./build.sh` for checks, tests and binaries in `dist/`, or `./build.sh test` for checks and tests only. **Only Docker is needed**, as the user asked, with no Go setup on this machine. It also builds `dist/tramline-darwin-arm64`, for the client and `serve --sim`. `internal/serial` has a stub off Linux that refuses to open a port, so nothing here has driven a printer from a Mac. Tests run on Linux only, in the container, and macOS gets `go vet`; the Mac client has been run by hand against `sim-web.sh`.

- The official `golang:1.27.3-alpine` image is pinned by digest in `build.sh`, so a changed image can't slip in. Upgrading Go means changing that line on purpose.
- Containers run with `--network none` and `GOPROXY=off`, so nothing can be downloaded during a build.
- **Standard library only.** The user's rule is the standard library, with well-known third-party libraries only if needed, and nothing has needed one so far. `build.sh` fails if `go.mod` gains any module, which was tested, so adding one has to be a deliberate edit to that check.
- The serial port needs no library. Go's `syscall` sets the 250000 baud rate through the `TCSETS2` ioctl.
- `dist/` and `.cache/` are generated and can be deleted.

### Structure

**One `tramline` binary is both server and client, and doesn't depend on SSH at runtime.** This is the user's requirement. Requests go over HTTP.

- `tramline serve` runs on the Pi as a systemd service. It's the only program that opens the serial port and the only one that talks to the Hue Bridge.
- Every other command is a client. It runs on any machine, sends one request, and prints JSON for scripts or readable text for people.
- Clients never contact the printer or bridge directly, so the Hue key stays on the Pi.

**Protocol: HTTPS with JSON, not a custom protocol.** Decided by the user. A custom protocol would have to rebuild what HTTP already provides:

- large file uploads, status codes and streaming
- debugging with `curl` or a browser
- a possible web page or MCP wrapper later

Live status uses Server-Sent Events, which are plain HTTP. It's all in Go's standard library, so the binary stays small.

### Elevated commands on the Pi

**Claude must never run `sudo` on the Pi.** Claude gives the user the exact command and the user runs it. That covers `deploy/install.sh`, editing `/etc/tramline/config.json`, and starting or restarting the service. Claude also asks before any other lasting change there.

### Secrets rule

**Claude must never read secrets on the user's machine or the Pi. This is strictly forbidden.** Tramline is built so it doesn't need to:

- Nothing secret is ever displayed. Setup only shows public fingerprints, so they can be copied around or pasted into chat safely.
- `tramline keygen` writes the private key and prints only its fingerprint.
- The Hue key stays on the server. `hue pair` and the API never return it, and it's never logged.
- `deploy/install.sh` doesn't create or read any secret.
- Off limits, though nothing enforces this beyond the rule: `~/.config/tramline/client.key`, and in `/var/lib/tramline/` the files `tls.key` and `hue.key`. The rest of that folder is public or a fingerprint, but it's only readable through sudo anyway.
