# Hue plug

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

The `tramline` server on the Pi handles all Hue communication directly, as the user asked. Clients only ever see `power on` and `power off`.

Bridge found 2026-11-02 through Hue's discovery service, then confirmed with its unauthenticated `/api/config`:

| Field | Value |
|---|---|
| Address | `192.168.1.128` |
| Bridge ID | Real value in `/etc/tramline/config.json` on the Pi |
| Model | `BSB002` |
| API version | `1.78.0` |

Tested on the real bridge and plug on 2026-11-02, with logs in `test-logs/2026-11-02-hue-plug/`. Times are GMT.

| Test | Result |
|---|---|
| Bridge certificate | Common name is the bridge ID, issued by `root-bridge`, and pairing accepted it |
| `tramline hue pair`, `tramline hue plugs` | Passed. Light plug's type is `plug` and its `light` UUID switches it |
| 3 `power on`, 3 `power off`, 20:23 to 20:26 | All passed, each taking about 0.3 seconds including reading the state back |
| USB device after `power on` | Appears after about 1.6 seconds |
| Printer identified after `power on` | Firmware after about 8 to 9 seconds, temperatures after about 10 to 11, no manual steps |
| USB disconnect after `power off` | About 2.5 to 2.8 seconds after the command, so `status` still shows the printer connected until then |
| Pi health right after the tests | `throttled=0x0`, no failed units, `sync` fine |

With the port open, the kernel logs `usb_serial_generic_read_bulk_callback - urb stopped: -32` twice just before each disconnect. It didn't appear in the earlier tests, when the port wasn't open. It looks like a side effect of the printer losing power, but that hasn't been confirmed.

Pairing and control:

- The user presses the bridge's link button, then `tramline hue pair` sends a `POST` to `https://<bridge>/api` with `devicetype` and `generateclientkey`. The key is stored in `/var/lib/tramline/`, readable only by the service. Paired 2026-11-02.
- Authenticate later calls with the `hue-application-key` header. Pin the bridge's certificate, and check the bridge ID.
- The address is set in the config. The server and bridge IPs should get DHCP reservations, because neither is found automatically if it changes.
- When pairing, the bridge's certificate must name the configured bridge ID, and that certificate is then pinned. If a firmware update ever changes it, calls fail with a clear error, and re-pairing is needed. Behaviour across a bridge firmware update is untested.
- Switching uses `PUT /clip/v2/resource/light/<id>` with `{"on":{"on":true}}`, which is confirmed to work for this plug.
- `Set` reads the plug state back after every switch, and fails if it doesn't change. The plug switched itself off once during testing. Subscribing to the bridge's event stream, to notice an unexpected power-off during a print, is still to do.
- **Keep the plug out of any room, zone or routine that gets switched off**, or a "lights off" could cut power mid-print. The user will handle this later, so it's still a risk until then.

Treat the plug state as the main sign the printer is on, and the serial device appearing as the second check.
