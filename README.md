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

```sh
alias tramline=dist/tramline-linux-amd64     # run from this folder, or tramline-darwin-arm64 on a Mac

tramline status                              # is anything running?
tramline upload part_PETG.gcode              # save it on the Pi, and copy small files to the SD card
tramline print part_PETG.gcode --bed-clear   # heat, check tram, then print
tramline web                                 # watch it in the browser
```

Every print checks tram first, and only starts on a pass. If the check fails, nothing has started: `status` says which knob to turn and which way, then `tramline print --continue` checks again.

Files that would take over 5 minutes to copy, about 4 MB, stay on the Pi and stream to the printer line by line. Don't restart the server during a stream, as it can't carry on afterwards.

## Commands

| Command | What it does |
|---|---|
| `status` | Power, temperatures and the current job |
| `web [--url]` | Open the web page, or just show its address |
| `power on\|off` | Switch the plug. Off is refused while hot or printing |
| `preheat [bed] [nozzle]`, `cool` | Heat, by default bed 80 °C and nozzle 150 °C, or turn the heaters off |
| `tram --bed-clear` | Probe the corners and report |
| `upload <file> [--sd\|--pi-only]` | Save a file on the Pi, and copy it to the card if that's the default |
| `copy <name>`, `copy --cancel` | Copy a file on the Pi to the card, or stop a copy |
| `files`, `files rm <name>` | List files, or delete one from both the Pi and the card |
| `mode [auto\|sd\|stream]` | Show or change what `upload` does by default |
| `print <name> --bed-clear` | Heat, check tram, then print |
| `print --continue` | Check tram again, after turning a knob |
| `cancel` | Cancel the print and turn the heaters off |
| `estop` | Emergency stop: `M112`, then cut the plug |

Run `tramline help` for everything, including the options and the commands used on the Pi.

## Web page

`tramline web` opens `http://192.168.1.121:8726/`, which also works on a phone. It shows status, temperatures and progress, with the file drawn in 3D as it prints. With nothing printing, it shows the last job.

- **It's read-only and has no login,** so anyone on the network can see the status and the current file's shape.
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
./sim-web.sh [--sd] [--minutes <n>] [--lan] [--no-open] <file.gcode>
```

## Security

The API uses mutual TLS: each client has its own key, and the Pi keeps a list of those allowed, by fingerprint. It listens on the LAN only. The web page is the one exception to needing a key, and can't reach the API.

## More detail

[ENGINEERING-NOTES.md](ENGINEERING-NOTES.md) has the design, firmware findings, test results and open issues, including what has only run on the simulator.
