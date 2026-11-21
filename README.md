# Tramline

A small print server for an Ender 3 V2, running on a Raspberry Pi. It's a lightweight alternative to OctoPrint: one Go binary with no dependencies that switches the printer's plug, heats, checks the bed's tram and prints. `tramline gui` does all of that from a browser on your computer, and a read-only web page on the Pi shows the print in 3D.

The name joins bed **tram**ming with the serial **line** to the printer.

## Features

- **Power:** switches the printer through a Hue smart plug, on by itself for a print, and off once the nozzle is below 50 °C after any print it started.
- **Heating:** preheats to bed 80 °C and nozzle 150 °C by default, or any temperatures, and turns the heaters off.
- **Tram checks:** probes the four corners, with an inset and repeats if wanted, and says which knob to turn and which way. Every print and scheduled print checks first and won't start out of tram.
- **Bed mesh:** reads the mesh the printer holds, with no movement, reads each print's fresh mesh before the plug is cut, and keeps a history of them.
- **Files:** uploads G-code to the Pi, lists and deletes files, and copies files to the printer's SD card at about 14 KB/s, with cancel and retry.
- **Checksums:** `files` shows each file's SHA-256, its first 12 digits as `sha256sum` starts, or whole with `--full`, on the Pi and on the card. The Pi's files are read from its disk again when listed, at most every 10 minutes, and a file that no longer matches its upload is shown as damaged, and isn't copied or streamed. The card can't be read back over USB, so a card file's sum is what the Pi sent in a copy whose size then checked out. `status`, `schedule list` and both web pages show the sum of the file being printed, copied or scheduled, the web pages behind an "i" to hover or tap.
- **Printing two ways:** streams the file from the Pi line by line, or prints from the SD card, which carries on if the Pi goes down.
- **Scheduling:** checks tram now, powers off, then at the set time powers on, checks again and prints unattended. It cancels itself if the check fails or another print has started since.
- **Stopping:** cancels a print with the heaters off, or an emergency stop with `M112` and the plug cut.
- **Status:** power, temperatures, the current job, progress and how recent scheduled prints ended, also as JSON.
- **Desktop GUI:** `tramline gui` does everything the commands do in a browser on your computer, with drag-and-drop uploads, a 3D print view, a live tram check and the mesh as a coloured surface.
- **Web page:** a read-only status page with a 3D view of the print, served by the Pi to any browser or phone, with no login.
- **Security:** mutual TLS with a key per client, allowed and revoked by fingerprint on the Pi, listening only on the LAN and Tailscale.
- **Simulator:** `serve --sim` runs a simulated printer and plug, so prints and the GUI can be tried on your computer without the Pi.
- **One binary:** server and client in one Go binary, standard library only, with no dependencies. The client runs on Linux and macOS, and the server on Linux, which can open the printer's serial port.

## How it works

```
your computer ── HTTPS, client certificates ──┐      ┌── USB ──▶ printer
                                              ├─▶ Pi ┤
any browser ──── HTTP, read-only ─────────────┘      └── LAN ──▶ Hue plug
```

The same binary is both server and client. Only the server touches the printer and the plug.

## Printing

From a printer that's off to a finished print with the printer off again:

```sh
tramline status                                      # is anything running?
tramline upload part_PETG.gcode                      # save it on the Pi
tramline print part_PETG.gcode --from pi --bed-clear # the rest happens by itself
tramline web                                         # watch it in the browser
tramline gui                                         # or do all of it in the browser
```

`print` then does all of this, carrying on if your computer goes away:

1. Turns the printer on if it's off.
2. Heats the bed to 80 °C and the nozzle to 150 °C, the temperatures the PETG start G-code probes at, and checks tram. If the check fails, nothing has started: `status` says which knob to turn and which way, then `tramline print --continue` checks again.
3. Prints.
4. When the print ends, however it ended, turns the heaters off, waits for the nozzle to drop below 50 °C, and turns the printer off.

If you do something else with the printer during that wait, such as `preheat`, `tram` or `power on`, it stays on. Prints started from the printer's own screen leave it on too.

### From the Pi or from the card

`print` always needs one of these. `<file>` in any command is the file's name on the Pi, which is the name it had when you uploaded it.

| | How it prints | What it needs |
|---|---|---|
| `--from pi` | The Pi sends the file line by line | Only `upload`. Don't restart the server during it, as it can't carry on afterwards |
| `--from sd` | The printer reads its own card, and carries on if the Pi goes down | `upload`, then `copy`. Copying takes about 1 minute per 800 KB, so large files are better from the Pi |

`upload` only ever saves on the Pi, and `copy` is the only thing that writes to the card, with the printer on:

```sh
tramline upload part_PETG.gcode
tramline power on
tramline copy part_PETG.gcode
tramline print part_PETG.gcode --from sd --bed-clear
```

### Scheduling a print

```sh
tramline schedule part_PETG.gcode --from pi --at "2026-11-06 06:00" --bed-clear
tramline schedule list
tramline schedule cancel 3
```

`schedule` does this, with the time in your computer's timezone:

1. Straight away, it turns the printer on if it's off, heats the bed to 80 °C and the nozzle to 150 °C, and checks tram. If the check fails, `status` says which knob to turn, and `tramline print --continue` checks again, as often as needed.
2. On a pass, it turns the heaters off, waits for the nozzle to drop below 50 °C, and turns the printer off until the scheduled time.
3. At the scheduled time, it turns the printer on, heats to 80 °C and 150 °C again, and checks tram again.
4. A pass starts the print, which ends like any other, with the printer turned off. A fail cancels the print, with no second try, and turns the printer off, and `schedule list` says why. Nothing tells you when that happens, so check in the morning.

Keep the bed clear until it prints. If any print has started since you scheduled this one, it cancels itself without moving, as a part may be on the bed.

## Commands

| Command | What it does |
|---|---|
| `status` | Power, temperatures and the current job |
| `gui [--no-open] [--port <n>]` | Open the full GUI in a browser, see below. Ctrl-C stops it |
| `web [--url]` | Open the web page, or just show its address |
| `power on\|off` | Switch the plug. Off is refused while hot or printing |
| `preheat [bed] [nozzle]`, `cool` | Heat, by default bed 80 °C and nozzle 150 °C, or turn the heaters off |
| `tram --bed-clear` | Probe the corners and report |
| `upload <path>` | Save a file on the Pi, nothing else |
| `copy <file>`, `copy --cancel` | Copy a file on the Pi to the card, or stop a copy |
| `files [--full]`, `files rm <file>` | List files with their SHA-256 sums, or delete one from both the Pi and the card |
| `print <file> --from pi\|sd --bed-clear` | Turn on if needed, heat, check tram, print, then turn off once cool |
| `print --continue` | Check tram again, after turning a knob |
| `cancel` | Cancel the print and turn the heaters off |
| `estop` | Emergency stop: `M112`, then cut the plug |
| `schedule <file> --from pi\|sd --at "YYYY-MM-DD HH:MM" --bed-clear` | Check tram now, power off, then print at that time if tram still passes |
| `schedule list [--json]`, `schedule cancel <id>` | List scheduled prints and how recent ones ended, or cancel one that hasn't started printing |

Run `tramline help` for everything, including the options and the commands used on the Pi.

## GUI

`tramline gui` opens a browser with everything the commands above do: files, with uploads by dragging them onto the window, printing and scheduling, cancelling, copying to the card, the plug, preheat and cool, tram checks, the Hue plug and an emergency stop. The print is drawn in 3D, as on the web page.

- **It runs on your computer, not the Pi,** on `127.0.0.1` only, and talks to the Pi with this computer's key, the same way the commands do. The browser never gets the key. Ctrl-C stops it, and anything already sent to the Pi carries on.
- **It signs the browser in once,** with a link that only works one time. To open it in another browser or after closing it, run `tramline gui` again.
- **The Pi still decides:** printing, scheduling and tram checks need the "bed is clear" box ticked each time, and the Pi's own rules apply as they do to the commands.
- **The emergency stop is held** for just over half a second, so a stray click can't set it off.

## Web page

`tramline web` opens the page at `http://raspberrypi:8726/`, which also works on a phone. It shows status, temperatures and progress, with the file drawn in 3D as it prints. With nothing printing, it shows the last job. It also lists scheduled prints: all that are to come, and the last five that ended, with why.

- **It's read-only and has no login,** so anyone on the network or the tailnet can see the status, the current file's shape, and the names and times of scheduled prints.
- **It can't affect a print,** as it never talks to the printer.
- **The head's position is worked out, not measured.** It comes from how far through the file the print has got, about every second when streaming and every 30 seconds or so from the card.

## Building and installing

Only Docker is needed, and the build uses the standard library alone. The macOS builds are for the client and the simulator: only Linux can open a real serial port, so the server stays on the Pi.

The [justfile](justfile) wraps the scripts. Run `just` in this folder to list its recipes:

```sh
just build             # ./build.sh: check, test and build for Linux and macOS into dist/
just test              # ./build.sh test: check and test only
just install           # build, then install this machine's client as tramline in ~/.local/bin
just uninstall         # remove it again
just deb               # build the Pi's package, dist/tramline_<version>_arm64.deb (needs dpkg-deb)
just arch-pkg          # build the Arch package in packaging/arch/ (makepkg -si there installs it)
```

The packages use the version in [VERSION](VERSION). The Pi's `.deb` installs the server, its unit (enabled), its user, its data folder and a default config, and arch-config installs it from this repository's releases, which is how the Pi gets a new version. See Packages in the [install notes](notes/install-and-config.md).

`just install` takes `TRAMLINE_BINDIR` for another folder, which has to be on your PATH.

Setting up a new computer as a client is covered in the [main README](../README.md#tramline-for-printing).

## Trying it without a printer

This runs a print on a simulated printer on this machine, with a throwaway login, and doesn't touch the Pi or the printer. Ctrl-C ends it.

```sh
./sim-web.sh [--sd] [--minutes <n>] [--schedule <minutes>] [--lan] [--no-open] <file.gcode>
./sim-gui.sh [--minutes <n>] [--fail-tram] [--no-open] [--port <n>] [file.gcode ...]   # the GUI, with these files uploaded
```

## Security

The API uses mutual TLS: each client has its own key, and the Pi keeps a list of those allowed, by fingerprint. It listens on the LAN and on Tailscale, set by `listen_interfaces` in the Pi's config, and nowhere else. The web page is the one exception to needing a key, and can't reach the API.

## More detail

[notes/](notes/README.md), the engineering notes, have the design, firmware findings, test results and open issues, including what has only run on the simulator.
