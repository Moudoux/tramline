# Tramline

A small print server for an Ender 3 V2, running on a Raspberry Pi. It's a lightweight alternative to OctoPrint: one Go binary with no dependencies that switches the printer's plug, heats, checks the bed's tram and prints. A read-only web page shows the print in 3D.

The name joins bed **tram**ming with the serial **line** to the printer.

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
alias tramline=dist/tramline-linux-amd64     # run from this folder, or tramline-darwin-arm64 on a Mac

tramline status                                      # is anything running?
tramline upload part_PETG.gcode                      # save it on the Pi
tramline print part_PETG.gcode --from pi --bed-clear # the rest happens by itself
tramline web                                         # watch it in the browser
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
| `web [--url]` | Open the web page, or just show its address |
| `power on\|off` | Switch the plug. Off is refused while hot or printing |
| `preheat [bed] [nozzle]`, `cool` | Heat, by default bed 80 °C and nozzle 150 °C, or turn the heaters off |
| `tram --bed-clear` | Probe the corners and report |
| `upload <path>` | Save a file on the Pi, nothing else |
| `copy <file>`, `copy --cancel` | Copy a file on the Pi to the card, or stop a copy |
| `files`, `files rm <file>` | List files, or delete one from both the Pi and the card |
| `print <file> --from pi\|sd --bed-clear` | Turn on if needed, heat, check tram, print, then turn off once cool |
| `print --continue` | Check tram again, after turning a knob |
| `cancel` | Cancel the print and turn the heaters off |
| `estop` | Emergency stop: `M112`, then cut the plug |
| `schedule <file> --from pi\|sd --at "YYYY-MM-DD HH:MM" --bed-clear` | Check tram now, power off, then print at that time if tram still passes |
| `schedule list [--json]`, `schedule cancel <id>` | List scheduled prints and how recent ones ended, or cancel one that hasn't started printing |

Run `tramline help` for everything, including the options and the commands used on the Pi.

## Web page

`tramline web` opens the page at `http://raspberrypi:8726/`, which also works on a phone. It shows status, temperatures and progress, with the file drawn in 3D as it prints. With nothing printing, it shows the last job. It also lists scheduled prints: all that are to come, and the last five that ended, with why.

- **It's read-only and has no login,** so anyone on the network or the tailnet can see the status, the current file's shape, and the names and times of scheduled prints.
- **It can't affect a print,** as it never talks to the printer.
- **The head's position is worked out, not measured.** It comes from how far through the file the print has got, about every second when streaming and every 30 seconds or so from the card.

## Building and installing

Only Docker is needed, and the build uses the standard library alone. The macOS builds are for the client and the simulator: only Linux can open a real serial port, so the server stays on the Pi.

```sh
./build.sh                       # check, test and build for Linux and macOS into dist/
./build.sh test                  # check and test only
deploy/install.sh raspberrypi    # install on the Pi, which uses sudo there
```

Setting up a new computer as a client is covered in the [main README](../README.md#tramline-for-printing).

## Trying it without a printer

This runs a print on a simulated printer on this machine, with a throwaway login, and doesn't touch the Pi or the printer. Ctrl-C ends it.

```sh
./sim-web.sh [--sd] [--minutes <n>] [--schedule <minutes>] [--lan] [--no-open] <file.gcode>
```

## Security

The API uses mutual TLS: each client has its own key, and the Pi keeps a list of those allowed, by fingerprint. It listens on the LAN and on Tailscale, set by `listen_interfaces` in the Pi's config, and nowhere else. The web page is the one exception to needing a key, and can't reach the API.

## More detail

[ENGINEERING-NOTES.md](ENGINEERING-NOTES.md) has the design, firmware findings, test results and open issues, including what has only run on the simulator.
