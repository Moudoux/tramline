# Install, config and listening

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

## Packages

Built 2026-11-21, at the user's request: a PKGBUILD for the client on Arch, and an arm64 `.deb` for the server on the Pi, which arch-config installs. **Both are installed, 2026-11-21: the `.deb` 0.1.0 on the Pi, and the Arch package 0.1.0-1 on the desktop, with `~/.local/bin/tramline` removed.** **0.1.1, the status streams, released 2026-11-21 as `tramline-v0.1.1` (`.deb` sha256 `1e67df7bce4c0bfdefc6b3aefb14ccfbc42877d051b7f8aeafd9712eb2d46cd5`, checked by download) and pinned in arch-config, uncommitted there. Not yet installed on the Pi or the desktop.** A new release's download link answered 404 for a few minutes after publishing, so wait for it before applying. The version is in `VERSION` (0.1.0 to start), the PKGBUILD's `pkgver` must match it, and both packages build with it in place of a `dev-` time.

- **Arch, `packaging/arch/PKGBUILD`:** the client binary in `/usr/bin/tramline`, and the README. It builds through `build.sh` in the pinned container, so it needs Docker rather than Go, and runs the tests too. `just arch-pkg` builds it, `makepkg -si` there installs it. Nothing for the server: no unit, user or data folder on the desktop. `~/.local/bin/tramline` from `just install` comes first on the PATH, so remove it with `just uninstall` once the package is installed.
- **Pi, `packaging/build-deb.sh`, through `just deb`:** `dist/tramline_<version>_arm64.deb`, made with `dpkg-deb` (Arch's `dpkg` package, in arch-config's `dev` task). It checks the binary's version under `qemu-aarch64-static` before packaging. It holds:
  - `/usr/bin/tramline`, and the unit in `/usr/lib/systemd/system/`, running `/usr/bin/tramline serve`.
  - `sysusers.d`: the `tramline` user in `dialout`, kept as it is if it exists (uid 995 on the Pi).
  - `tmpfiles.d`: `/var/lib/tramline`, mode 700.
  - `modules-load.d`: `ch341` at boot, see the driver note below.
  - `/usr/share/tramline/config.json`, copied to `/etc/tramline/config.json` only if there's none. It's not a conffile, so dpkg never asks about the edited one on the Pi.
- **`postinst`** creates the user and folder, writes the config if missing, loads `ch341`, enables the service and restarts it. **Restarting ends a stream, so don't install during a print.** Before `tramline init` there's no `tls.crt`, so it enables the service without starting it, and says what to run.
- **Removing** stops and disables the service. **Purging** also removes `/etc/tramline/config.json`, but keeps `/var/lib/tramline` and the user, as the server's identity, the Hue key and the history can't be made again.
- **arch-config's `pi/` `tramline` task only installs the `.deb`:** it downloads it from this repo's release `v<version>` (0.1.0 and 0.1.1 were first released as `tramline-v<version>` in stringing-along, before Tramline moved to its own repo on 2026-11-22), pinned by sha256, and installs it with apt unless that version is installed. Once, it also disables the service and removes what the hand install left: `/etc/systemd/system/tramline.service`, which would override the package's unit, `/etc/modules-load.d/tramline.conf` and `/usr/local/bin/tramline`. So a new version is: change `VERSION` and `pkgver`, commit and push to `main`. **Since 2026-11-25 CI releases by itself:** a push to `main` whose `VERSION` has no tag `v<VERSION>` yet is built as that version, with the `.deb` built by `packaging/build-deb.sh` on the runner, and once the checks and tests pass, the release `v<VERSION>` is published with the binaries, the `.deb` and `SHA256SUMS` (which has the `.deb`'s sum, also shown in the run's summary), creating the tag. CI also refuses a `pkgver` that differs from `VERSION`. Then put the version and sha256 in arch-config. `just deb` is still there for a local build. Release binaries now carry the plain version, such as `0.1.2`, as the `.deb` does, not the tag's `v0.1.2`.
- `deploy/install.sh` and `just deploy`, which installed by hand over SSH before the package, were removed once it was installed. `packaging/deb/` now holds the unit and the default config.

## Install and updates

- The first install has to copy the binary over somehow. SSH is simplest, but it's only used for that step.
- Installed once so far, on 2026-11-02, before the no-sudo rule. It's being kept. It created the `tramline` system user (uid 995, no login), plus the files below.
- Updating means a new `.deb` through arch-config, see Packages.
- `tramline update` is still planned, to send a new binary over the authenticated connection and restart, refusing during a print or SD upload. **Its design is open:** the service runs unprivileged with a read-only `/usr/bin`, so it can't replace its own binary there. Keeping the binary under `/var/lib/tramline/` would solve that, at the cost of letting a compromised server rewrite itself.
- Files: binary at `/usr/bin/tramline` (`/usr/local/bin/tramline` before the package), config in `/etc/tramline/config.json`, and everything the server generates in `/var/lib/tramline/`. Runs as a dedicated user in `dialout`.
- **The serial driver must be loaded before the service starts.** The unit has `DevicePolicy=closed` and `DeviceAllow=char-ttyUSB rw`. systemd looks up the `ttyUSB` group in `/proc/devices` once, when the service starts, and that group only exists once `usbserial` is loaded. If the Pi boots with the printer unplugged, the server can't open the port until it restarts. That happened from 2026-11-12 to 11-19, during the cable test. Since 2026-11-19, `ch341` loads at boot from `/etc/modules-load.d/tramline.conf`, set in arch-config `pi/` (`task tramline`), which also manages the unit. The server now logs why it can't connect, such as `can't open the printer's port: ...` or `... didn't answer M115`, once each time the reason changes. Before that it was silent.
- The server only reads the config, and never rewrites it. Secrets it generates stay out of the config, in `/var/lib/tramline/`, readable only by the service: the Hue key and the TLS key.
- JSON is used because Go's standard library reads it without a dependency.

## Config

`tram_soak_seconds` is optional: how long a print waits at temperature before its first tram check, 0 to 1200, default 0. It isn't in the example below as the default suits the current thin plate. See Tram before printing.

`listen_interfaces` and `port` say where to serve, see Listening by interface. `listen`, one fixed address and port, can replace them, and is what the simulator uses.

```json
{
  "listen_interfaces": ["eth0", "tailscale0"],
  "port": 8726,
  "serial": {
    "device": "/dev/serial/by-id/usb-1a86_USB_Serial-if00-port0",
    "baud": 250000
  },
  "hue": {
    "address": "192.168.1.128",
    "bridge_id": "001788FFFE123456",
    "plug_light_id": "00000000-0000-4000-8000-000000000000"
  }
}
```

## Listening by interface

Built 2026-11-04, at the user's request, so the Pi can be used from other devices over Tailscale. **Interfaces are named rather than addresses,** as the user didn't want hardcoded addresses: `eth0` gets its address from DHCP, and a Tailscale address normally lasts as long as the Pi stays registered, but can change if it's re-added. The Hue bridge's address is still fixed, in `hue.address`, and `bridge_id` proves it's the right device.

- **How it works:** `InterfaceListener` in `internal/server/interfaces.go` is a `net.Listener` that looks up each interface's addresses every 30 seconds and listens on each on `port`. That covers IPv4 and IPv6, without link-local addresses. It moves to a new address by itself.
- **One interface can't take another down:** an interface missing at boot, Tailscale being down or a port in use only gets logged and tried again. A failed lookup leaves what's already open alone. A problem is logged when it changes, not on every try.
- **It never fails to start,** so a mistyped interface name shows up only in the log, as `no such network interface`, with the server running and not reachable there. If `eth0` were misnamed, the LAN wouldn't answer.
- **`eth0` doesn't cover `eth0.2`,** which is its own interface, as `ip -br addr` shows.
- **It needs `AF_NETLINK` in the service's `RestrictAddressFamilies`:** Go uses it to look up an interface's addresses. Without it the log says `netlinkrib: address family not supported by protocol`, and nothing can be listened on, so a new config needs the new service file. Checked with `systemd-run --user` on the desktop, both ways.
- **Same server on every address:** the page has no login, so every device on the tailnet can see it, which the user accepted. The API still needs an allowed client certificate.
- **The client's URL uses the name:** `tramline login https://raspberrypi:8726 <fingerprint>`, which the user has done. The client checks the server by fingerprint, not by name, so the certificate's IP entry doesn't matter and nothing needs regenerating. `raspberrypi` resolves to the LAN address through the router, and should resolve to the Tailscale one through MagicDNS, which hasn't been tried yet.
- **Upload limit:** the client's 30-minute limit was sized for a home network, so a big upload over a slow connection could hit it.

- The plug is identified **by UUID, as the user asked**, not by name, so renaming it in the Hue app changes nothing.
- The UUID is the plug's `light` resource ID in CLIP v2, which is the ID that switching uses. A device also has a separate `device` UUID, so don't mix them up.
- The app calls the plug **"Light plug"**. That name is only used once, to find its UUID in the `tramline hue plugs` listing. The UUID can't be looked up until the bridge is paired.
- At startup, the server checks the UUID exists on the bridge and logs its current name. If it doesn't exist, power commands fail with a clear error and never guess another device.
- If the plug is ever removed and re-added to the bridge, it's expected to get a new UUID, so the config would need updating.
