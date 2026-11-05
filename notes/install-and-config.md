# Install, config and listening

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

## Install and updates

- The first install has to copy the binary over somehow. SSH is simplest, but it's only used for that step.
- Installed once so far, on 2026-11-02, before the no-sudo rule. It's being kept. It created the `tramline` system user (uid 995, no login), plus the files below.
- Today, updating means the user runs `deploy/install.sh` again.
- `tramline update` is still planned, to send a new binary over the authenticated connection and restart, refusing during a print or SD upload. **Its design is open:** the service runs unprivileged with a read-only `/usr/local/bin`, so it can't replace its own binary there. Keeping the binary under `/var/lib/tramline/` would solve that, at the cost of letting a compromised server rewrite itself.
- Files: binary at `/usr/local/bin/tramline`, config in `/etc/tramline/config.json`, and everything the server generates in `/var/lib/tramline/`. Runs as a dedicated user in `dialout`.
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
