# Tramline engineering notes

The record of Tramline's design, firmware findings, test results and open issues. For what it is and how to use it, see the [README](README.md).

**A read-only web page, `tramline web`, is installed on the Pi, see Web page. Status: running on the Pi with mutual TLS, paired with the real Hue Bridge, and `tramline power` works on the real plug. Stage 5, read-only printer status, has passed on the real printer. Stage 6, heating, has passed on the real printer, including the 30-minute idle timeout at low temperatures. `tramline tram` works on the real printer. Stage 7 has passed on the real printer. Stage 8, SD upload, has passed on the real printer, at 14 KB/s with a sliced file. Stage 9, `print`, `cancel` and `estop`, has passed on the real printer with a cold file and a small real print. Tram before every print is in use, with only its pass path run on the real printer. The first long print, 4 h 10 min, finished cleanly on 2026-11-04. Stage 10, printing by streaming, has passed on the real printer, including a 63-minute job that finished cleanly on 2026-11-04 with no resends, see Streaming design.** Last updated 2026-11-04.

| Part | State |
|---|---|
| Container build, dependency check | Done |
| Serial port, Marlin protocol, reconnecting | Done, tested on a pseudo-terminal and simulator. On the real printer it has identified the firmware, read temperatures and reconnected after power cycles and a server restart. It has also heated and moved it. It has also copied a file to the card with line numbers and checksums. Resends have only run on the simulator |
| HTTPS with mutual TLS and allow list | Done |
| `init`, `allow`, `serve`, `keygen`, `login`, `status` | Done |
| Hue pairing, plug listing, `power on\|off` | Done, tested on the real bridge and plug, including the refusal to power off while hot |
| systemd unit, `deploy/install.sh` | Done, installed and running |
| `tram`, `tram info`, wizard's rule | Done, tested on the real printer, hot and cold. The late-reply resync, and failure and link-drop handling, have only run on the simulator |
| `preheat`, `cool`, idle timeout | Done, tested on the real printer. Up to bed 80°C and nozzle 150°C, and the idle timeout only at low temperatures |
| Last-resort plug cut | Built, simulator and fakes only |
| `upload`, `copy`, `files`, `mode` | Done. Copies, resends, listing and delete have run on the real printer. Other failure handling is simulator only. `mode auto` only affects `upload`'s choice, and has run on the simulator's tests only |
| Printing from SD, `cancel`, `estop`, line numbers on every command | Done, tested on the real printer. `estop` only on a cold print, and resends of numbered commands, link drops and a stuck print only on the simulator |
| Tram before every print, `print --continue`, LCD messages, `tram_soak_seconds` | Done. The pass path and LCD messages have run on the real printer. A failed check, `--continue` and a soak above 0 are simulator only |
| Printing by streaming | Done 2026-11-04. Passed on the real printer: to the end, a long print of 182,184 lines, cancel cold and hot, the LCD's Stop, `M85` firing, a restart. A real resend, a lost link and `estop` during a stream are simulator only, as none has happened on the printer. The LCD's Pause isn't supported. See Streaming design |
| Read-only web page, `tramline web` | Built and installed 2026-11-04. The user has tried it on the simulator and on a phone. It hasn't yet been watched during a real print. See Web page |
| Listening by interface, including Tailscale, `listen_interfaces` | Built and installed 2026-11-04. On the Pi it listens on `eth0` and on `tailscale0`, IPv4 and IPv6, and the page answers on those addresses from the Pi itself, but not on `eth0.2` or `docker0`. The user confirmed it works over Tailscale from a phone on 5G. An address changing has only run in tests. See Listening by interface |
| Scheduled prints, `schedule`, `schedule list`, `schedule cancel` | Built 2026-11-05. Tests and the simulator only, not yet installed or run on the real printer. See Scheduled prints |
| `tramline update` | Not started |

## Next session

**Newest, 2026-11-05:** scheduled prints are built, pass on the simulator and are installed on the Pi, but haven't run on the real printer, see Scheduled prints for what to test first. Everything below this paragraph was written before that.

**Also 2026-11-05, asked for by the user after that, built and simulator only:** see CLI changes under Scheduled prints. Every print now turns the printer off, `print` turns it on, `upload` only uploads, `mode` is gone, `--from pi|sd` is required, and the web page lists scheduled prints.

Auto-off on every print means a real power cycle after each one, so the unexplained Pi hang and the plug's self-off under Open issue matter more. Capture the kernel log during the first real tests.

**Installed and committed:** the user installed `dev-20261105183233` on the Pi on 2026-11-05, and `status` confirmed it. It's committed and pushed to main. Nothing in it has run on the real printer yet: the first test proposed is scheduling `dry-run.gcode` about 20 minutes ahead with `--from pi`, about 30 minutes in all, with the kernel log captured, and only with the user's OK.

**Current state, 2026-11-04, evening:** stage 10 is done. Test 8, the long print, finished cleanly at 19:46:01 GMT with 0 resends, see Long print by streaming. The logs are in `test-logs/2026-11-04-stream/t8-long/`, with `server.log` and `kernel.log` saved. The code is in the private `stringing-along` repository, without `test-logs/`, `dist/` or `.cache/`, so those logs exist only on this machine.

**Next:** the web page is installed, see Web page, and needs watching during a real print. What's worth checking there is listed under Not proven on the real printer. The skills for SolidCraft, Cura slicing and Tramline were made 2026-11-04, in `../.claude/skills/`, so keep the `tramline` one in step with this README. The small real copy on the rewritten `sendLines` passed, see Upload's result under Long print by streaming.

**Installed versus source:** the Pi and `dist/` are both `dev-20261104213525`, which adds listening by interface, with `"listen_interfaces": ["eth0", "tailscale0"], "port": 8726` in `/etc/tramline/config.json`, see Listening by interface. The user runs `deploy/install.sh raspberrypi` to install a new build.

**Print design:** built as approved and tested on the real printer, with its firmware findings and results, under Print design below. Test files are in `test-logs/2026-11-03-print/`: `dry-run.gcode`, about 60 seconds of `G4` and `M118` with no heat or movement, `dry-run-long.gcode`, about 10 minutes of the same, and `test-tile_PETG.gcode`, 15 x 15 x 1 mm, 2 min 45 s printing and about 10 minutes in all from cold.

**Bed state:** in tram at the last checks, hot, 2026-11-04: 0.055 mm once, a fail, then after a small back-left turn 0.037, 0.029, 0.043, 0.037, 0.048, 0.040 and 0.033 mm, so it moves around close to the limit. A failed check simply waits for an adjustment. Always ask the user to confirm the bed is clear before any print, as a finished print may be on it.

**Plan:**

1. **Tram before every print:** built and in use since 2026-11-04. The pass path has run on the printer, see Tram before printing.
2. **Print the Claude Monitor stand:** done 2026-11-04, see Stand print result. In `../projects/claude-monitor-case/`, body "Stand Retro", `stand-retro-print_PETG.gcode`, 15.8 m, 2.7 MB. **It doesn't use the shared profile:** 0.16 mm layers with a 0.2 mm first layer, 2 walls, 6 top and 4 bottom layers, 10% grid, seam at the back, from a scratch copy of `../cura/`. `slice_petg.sh` alone gives a different print, at 0.2 mm, 3 walls, 20% gyroid and 9.4 MB, so compare a re-slice's header with the old file.
3. **Stage 10, printing by streaming,** which large files need under `mode auto`, with `M85` as the hung-Pi safeguard. Done 2026-11-04, see Streaming design. The lost lines depend on window size or speed, and none were lost at one line at a time, see Lost lines: test B, though the cause is still unknown.
4. **Skills for SolidCraft, Cura slicing and Tramline:** made 2026-11-04.

**Not proven on the real printer:**

- The web page on a real phone. Its layout and gestures have only run in a phone-sized frame in desktop Firefox, with made-up touch events, so Safari's and Chrome's own handling of pinch, and drawing speed on a phone's GPU, are unproven.
- The web page during a print: how far the drawn head is from the real one, whether the guesses at how many moves it lags (`LAG_MOVES` in `internal/server/web/viewer.js`) are right, and that browsers watching leave a stream's timing alone. They should, as the page never touches the serial port, but only a real print shows it.
- The wizard during a wait for adjusting. A failed tram check, the parked wait, the LCD's message and `--continue` first ran for real on 2026-11-04, see First stream on the real printer. They've run once, on back-left.
- The late-reply resync, as no command has timed out there, though `M118` does echo as it needs.
- `tram`'s failure handling: probe failure, link drop mid-probe, firmware halt. Simulator only.
- History in `/var/lib/tramline/tram-history.jsonl`. It works on the simulator, but that folder can't be read without sudo, so it hasn't been checked on the Pi. Six real runs should be in it.
- Knob direction on any corner but back-left.
- The plug cut, which has only run against a fake plug.
- A lost link during a print or a stream, a plug going off during one, and a stuck print being cancelled. `estop` during a hot print, a real stream or a copy. Resends of numbered commands, apart from copies: none happened in the 182,184 lines of the long stream. Simulator only.
- Restarting the server during a real hot print or a heater wait.
- Upload's failure handling other than resends: timeouts, cancelling, `cool` or the timer stopping a copy, a link drop, and `M29` at reconnect. Simulator only.

**Other open issues:**

- A real resend at a safe window, cause unknown, means a line may have been damaged on the wire, see Upload. Every command now has a line number and checksum, as decided on 2026-11-03. Numbered commands now run on the printer, but a resend of one, apart from copies, is still simulator only.
- **Damaged replies from the printer:** three in 182,184 lines of the long stream, cause unknown, all harmless so far, see Long print by streaming. A small fix for a later build: log any reply that doesn't have the expected form, and leave a `P` over 15 out of the planner figures.
- The LCD's bar and time left stay at 0 until a stream's first `M73` after the print screen opens at `M190`, about 3 minutes into printing on a 6 MB file, then work, and the user reports they tracked perfectly on the long print. Small fix for a later build: send `M73` again at the first real line.
- After a Tramline cancel of a stream, the LCD says print done, 100%. Only cosmetic, but it misleads. The LCD's Pause during a stream isn't supported.
- Files sliced before 2026-11-03 still have `G1 X0 Y{machine_depth}` in their end G-code, as `../cura/resolve_settings.py` only now fills it in, with Y220. Re-slice to get the fix. The user saw the bed come forward with the fix on the stand.
- The log's gaps during a print, see Stand print result, are fixed in the stage 10 build. Temperatures about once a minute have run on the printer during streams. Each `M27` wait and lines arriving during one need an SD print, and in-flight bytes and timing at each resend need a real resend, so those are still simulator only.
- Connecting took 18 seconds on one power-on, against the usual 9 to 11: the boot text arrived as garbage, and the first `M115` seems to have been lost, so the 10-second wait ran out. It recovered by itself. The garbage looks like a wrong baud rate during boot, which is only a guess.
- `tram info` sends `G29 W`, which prints nothing useful on this build, so it could be dropped. `C29` in `M503` has what's needed.
- `DefaultInset` is a constant, and the wizard may follow the mesh limits if they're changed in the LCD menu.
- The motion limits (60 seconds' silence, 3 minutes for `G28`, 90 seconds for `G30`) are generous. Real figures are at most 2.77 seconds' silence, 19.5 seconds for `G28` and 5.5 seconds for `G30`.
- A 0.05 mm limit means a bed within about 0.01 mm of it can flip between runs. Whether to add a margin is the user's decision.
- `External Reset` on power-on, and which command the early `ok` at startup answers, are still unknown. `status` still shows the printer connected for about 3 seconds after power off, though it can't let heating through. The causes of the plug's one self-off and of the one Pi hang are unknown. The user still needs to review Hue rooms and routines before any long print.

**Rules:** see the Elevated commands and Secrets sections. Build only with `./build.sh`. Ask the user before every test involving the printer, saying what it does and how long it should take. Capture kernel logs during power tests. Don't heat or move the printer without asking, and always ask for the bed to be confirmed clear before any print.

## Open issue: unexplained Pi hang, not reproduced

The Pi hung once on 2026-11-02, soon after the printer was turned off with the cable connected. Three controlled power-offs afterwards did not reproduce it. **The printer now looks less likely as the cause, but it isn't ruled out.**

### The hang

Times are GMT.

| Time | Event | Source |
|---|---|---|
| About 18:55 to 18:56 | Printer on and connected, SSH and serial working | Kernel log, SSH sessions |
| Unknown, soon after | Printer turned off, cable still connected. Probably with its own switch, as the user hadn't used the Hue plug in a long time | User; exact time not recorded |
| 18:57:41 | Last Beszel stats saved to disk, with CPU, RAM and temperature normal | Beszel database |
| 18:57:56 | Last timestamp saved by `systemd-timesyncd` | Boot log |
| After that, until about 19:09 | No more stats saved. SSH reset before login, ping and port 443 still answered | Beszel, SSH attempts |
| About 19:09:53 | User unplugged and replugged the Pi | `uptime -s` |

Normal stats stopped reaching the disk about 11 minutes before the restart, with no build-up beforehand. That fits the USB SSD or USB controller failing, but the kernel log was lost because `/etc/systemd/journald.conf` has `Storage=volatile`. The real cause is unknown, and it may have nothing to do with the printer.

### The repeat tests

The kernel log was streamed to this machine, with a check every 5 seconds of ping, login, `sync` and power warnings. Logs are in `test-logs/2026-11-02-power-off/` and `test-logs/2026-11-02-plug-power-cycle/`.

| Time | Event | Pi result |
|---|---|---|
| 19:16:04 to 19:16:06 | Printer powered on, and the Hue plug went off by itself about 2 seconds later | Clean USB disconnect, no errors |
| 19:19:14 | Plug on again, stayed on. `M115` and `M105` over serial | Normal |
| 19:22:49 | Printer turned off with its own switch after about 3.5 minutes | Clean USB disconnect, no errors |
| 19:22:49 to 19:28:48 | Watched for 6 minutes | All checks passed, 10 containers up, no failed units |
| 19:50:34 | Plug on with USB connected, stayed on | Normal |
| 19:53:40 | Printer turned off with the Hue plug after about 3 minutes | Clean USB disconnect, no errors |
| 19:53:40 to about 19:59 | Watched for 5 minutes | All checks passed |

The hang's boot had been running for longer than the test boot, which is a difference these tests can't cover.

### Follow-ups

- Keep a way to capture kernel logs during Tramline's power testing. Streaming over SSH needs no settings change. Enabling persistent logs is the user's call, as `Storage=volatile` may be deliberate to reduce SSD writes.
- Repeat power-offs during hardware testing, ideally some after the Pi has been up for days.
- If the hang returns, options to try, cheapest first: disconnect the device in software before cutting power by unbinding USB port `1-1.3`, then a USB isolator, then a powered hub.

**Clock warning:** the Pi has no battery-backed clock. Until NTP corrects it after boot, its clock can read 18:17 or 18:57:56. In this boot the correction came at 19:10:29, so files stamped 18:58 are from the restart, not the hang, and `last` shows the boot as 18:17.

### The Hue plug switching itself off

The plug went off about 2 seconds after the first power-on with USB connected, then stayed on with USB disconnected, then stayed on twice more with USB connected, at 19:19 and 19:50. It also stayed on for all three `tramline power on` tests at 20:23 to 20:26, the longest about 95 seconds. It hadn't been used in a long time, which may explain it, but the cause is unknown. Tramline must read the plug's state back after switching it, and confirm the printer answers, and not assume the command worked.

## Goal

1. Turn the printer on if it is off.
2. Preheat if needed.
3. Check bed tram, and pause for manual adjustment if needed.
4. Upload a print and start it.

This is a lightweight alternative to OctoPrint, which the user finds heavy. The full workflow becomes SolidCraft design, then slicing in `../cura/`, then Tramline.

## Hardware

| Item | Details | Source |
|---|---|---|
| Printer | Creality Ender 3 V2, stock Bowden, 0.4 mm nozzle | User |
| Board and firmware | 4.2.2 board, `Marlin 2.1.3 MRiscoC Ender3V2-422-BLTUBL-MPC`, built 17 Dec 2023 | M115 |
| Probe and levelling | Touch probe, UBL | User and M115 |
| Host | Raspberry Pi 4 Model B Rev 1.1, 4 GB RAM, aarch64, Debian 12 | SSH |
| Pi role | Also runs Docker containers and Tailscale | SSH |
| Pi boot disk | Sabrent USB SSD on the USB 3 bus, holds `/` and `/boot/firmware` | SSH |
| Power control | Philips Hue smart plug, with a Hue Bridge | User |
| Camera | None | User |
| Filament | PETG | User |
| Bed surface | Thin flexible plate made for PETG, not glass | User |

## Verified: connection

Tested 2026-11-02 from this machine.

- `ssh raspberrypi` logs in as `pi` without a password, and `pi` is in `dialout`.
- Serial path: `/dev/serial/by-id/usb-1a86_USB_Serial-if00-port0`, which pointed to `/dev/ttyUSB0`.
- The USB chip is a CH340 (`1a86:7523`) using the `ch341` driver, on USB port `1-1.3`.
- The serial device disappears cleanly when the printer loses power, and returns when it comes back, tested with both the plug and the printer's switch.
- The link works at **250000 baud**. It answered on the first baud rate tried, so 115200 was not tested.
- Temperatures read: nozzle 21.96°C, bed 22.49°C, heaters off.
- `vcgencmd get_throttled` returned `0x0` before and after the serial test, with the printer on.
- Opening the port does not appear to restart the board, tested once with the printer idle, see below. It hasn't been tested during a print.
- Opening the port let in 6 garbage bytes, which gave a harmless `Unknown command`. Send a newline first to clear these.


### Read-only status through the server

Stage 5, tested 2026-11-02 at 20:35 to 20:54 GMT, with logs in `test-logs/2026-11-02-stage5/`.

| Test | Result |
|---|---|
| Idle for about 11 minutes, 600 status samples | Plug on throughout. Connected after about 8 seconds, temperatures 2 seconds later, then no dropouts |
| Temperatures, heaters off | Kept updating. Nozzle about 20.7 to 21.4°C, bed about 20.5 to 20.8°C |
| `sudo systemctl restart tramline` with the printer on | Reconnected 0.56 seconds after the server started. Screen didn't change, and no `start` line |
| `power off`, then about 6 minutes' watch | USB dropped 3.1 seconds later, server logged the disconnect. Ping, login and `sync` fine, `throttled=0x0`, no failed units |

- **Board startup:** after power-on the board answers nothing until about 6 seconds after USB appears. See the 2026-11-03 update under Heating for why its late reply was logged as unprompted text.
- **`start` line:** it wasn't seen on power-on, probably because it's sent before USB is up, though that's unconfirmed. So its absence doesn't prove on its own that the board didn't restart. The restart result rests on that together with the screen and the fast reconnect.
- **Not tested:** pulling the USB cable with the printer on, which the user chose to skip, and reconnecting during a print.

### M115 capabilities

Present: `BINARY_FILE_TRANSFER`, `EEPROM`, `VOLUMETRIC`, `AUTOREPORT_TEMP`, `PRINT_JOB`, `AUTOLEVEL`, `RUNOUT`, `Z_PROBE`, `LEVELING_DATA`, `BUILD_PERCENT`, `EMERGENCY_PARSER`, `HOST_ACTION_COMMANDS`, `PROMPT_SUPPORT`, `SDCARD`, `SD_WRITE`, `LONG_FILENAME`, `LFN_WRITE`, `CUSTOM_FIRMWARE_UPLOAD`, `EXTENDED_M20`, `THERMAL_PROTECTION`, `ARCS`, `BABYSTEPPING`.

Absent: `SERIAL_XON_XOFF`, `AUTOREPORT_POS`, `PROGRESS`, `SOFTWARE_POWER`, `TOGGLE_LIGHTS`, `MULTI_VOLUME`, `REPEAT`, `AUTOREPORT_SD_STATUS`, `MOTION_MODES`, `MEATPACK`, `CONFIG_EXPORT`.

Reported area: full `248 x 231 x 250`, work `230 x 230 x 250`. These match the firmware's own defaults (`X_BED_SIZE 230`, `X_MAX_POS 248`, `Y_MAX_POS 231`), and the user has not changed them. The Cura profile separately slices for 220 x 220, so this needs no action.

## The USB cable

The cable is modified so the printer can't be powered from the Pi over USB. The user opened the jacket and cut only the red +5V wire. The [firmware wiki](https://github.com/mriscoc/Ender3V2S1/wiki/Octoprint) also recommends disabling +5V.

- With the printer off and the cable connected, the user confirmed the screen is fully black.
- With the printer on, data works, as shown above.

Software switching of USB power is not an option. The Pi 4's ports switch [as one group](https://github.com/mvp/uhubctl), which would also cut power to the boot SSD.

**Never run `uhubctl` against this Pi's USB hubs.**

## Firmware findings

Read from [the firmware source](https://github.com/mriscoc/Ender3V2S1) at commit `05903a8` (19 Sep 2026). **The printer's build is from Dec 2023, so all of this needs testing on the real printer.**

### Tramming

- `C35` launches the [tramming wizard](https://github.com/mriscoc/Ender3V2S1/wiki/Firmware-features), and `C35 T<n>` probes a single point.
- The readable code, `tram()` in `bed_tramming.cpp`, reports to the LCD only. The wizard itself, `trammingWizard()`, is in the closed-source `libproui` library.
- **The wizard's tolerance is 0.05 mm.** The float `0.05` is in `trammingWizard()`'s compiled code and in `bed_tramming_probe_tolerance`. This was read from `lib/proui/stm32f1/libproui_ubl_dwin.a` at tags `20231202` and `20240122`, either side of this printer's build, and at current `main`.
- The comparison logic comes from public Marlin 2.1.2.1, the same wizard before it went closed-source. It subtracts the average from the four readings, then passes if `max - min < 0.05`. Otherwise it names the corner furthest from the average, with "Lower" if above average and "Raise" if below.
- The library's probing order is front-left, front-right, back-right, back-left.
- `G35` is disabled because `ASSISTED_TRAMMING` is commented out.
- **`G30 X<x> Y<y>` is the route to use.** It sends `Bed X:.. Y:.. Z:..` over serial.
- `G30` homes if needed, and turns bed levelling off. The start G-code turns levelling back on with `G29 A`.
- **Tramming insets, since found to be wrong for this printer, see Tram check:** `tram()` prefers `BED_TRAMMING_INSET_LFRB`, which `Configuration.h` at tag `20231202` sets to 30 mm per side, so 30 and 200. It only falls back to the mesh limits (default `MESH_INSET` 25 mm) if that isn't defined. That file is the repo default and may differ from this printer's build, and the wizard is closed-source. 
- `tram()` sets the LCD status to `X: Y: Z:` for each point, which may show the wizard's points and values. `HOST_STATUS_NOTIFICATIONS` is off in that config, so they probably don't reach serial.
- `G30` ends in a normal `ok` even when it fails, with `Error:Probing Failed` or `Z Probe Past Bed` and no `Bed` line. It uses `G28O`, which homes only if needed.
- `C29` sets mesh insets, per the wiki. What it does with no parameters is unknown, so Tramline doesn't send it.
- Default speeds at that tag: homing 50 mm/s X/Y and 10 mm/s Z, probe travel 200 mm/s, Z probing 16 mm/s, and `busy:` every 2 seconds. Steppers switch off after 120 seconds idle.
- A [firmware issue](https://github.com/mriscoc/Ender3V2S1/issues/1186) reports that the wizard fails on a severely tilted bed. Manual tramming comes first in that case.

### Serial and host behaviour

- Default baud rate is 250000, changeable with `M575`.
- Buffer sizes in source: `BUFSIZE 16`, `TX_BUFFER_SIZE 64`, with `ADVANCED_OK` on. The live printer reported `ok P15 B15`.
- Emergency parser is on, so emergency commands get through even while the printer is busy.
- Per the wiki, cancel an SD print with `M524` rather than `M25`, and Meatpack is incompatible.
- The source enables `HOST_KEEPALIVE_FEATURE`, so long commands may send `busy:` messages. Timeouts must allow for this.
- The wiki describes `M75` and `M76` for host print timers. They aren't needed for SD prints unless testing shows otherwise.

### Custom codes

From the wiki:

| Code | Purpose |
|---|---|
| `C10` | Marks a file as a configuration file, skipping the print confirmation and print-done dialogs |
| `C35` | Launch tramming wizard |
| `C108` | Cancel screens waiting for a simple confirmation |
| `C115` | Show ProUI details |
| `C510` | Lock or unlock the screen |
| `C851` | Probe feed rate and multiple probing |

**Possible blocker:** because `C10` exists to skip a print confirmation dialog, a remotely started print may stop and wait at the LCD. Test this, and try `C108` if it does.

## Design

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

### Security

With SSH gone, Tramline must provide its own access control and encryption. Anyone who can send commands can heat the printer or start it moving, and the Pi's firewall is off (`ufw` inactive).

**Mutual TLS, chosen by the user over a shared token.** Each side has its own key pair and self-signed certificate, and trusts the other by SHA-256 fingerprint. There's no certificate authority.

- **Server identity:** `tramline init` creates the server's key. The client gets its fingerprint at login, not from the first connection, and refuses any other server.
- **Client identity:** `tramline keygen` creates the key on the client, so a private key never has to move between machines. TLS proves the client holds it, and it's never sent.
- **Allow list:** `tramline allow <fingerprint>` adds a client to `/var/lib/tramline/clients.allow`, which holds only public fingerprints. It's read for every connection and request, so allowing or revoking needs no restart, and revoking also cuts off open connections. A missing or corrupted list denies everyone.
- **TLS 1.3 only.** Every API endpoint needs an allowed certificate. Plain HTTP on the same port reaches only the read-only web page, which the user asked for on 2026-11-04, see Web page.
- **Listening addresses:** the LAN and Tailscale, decided by the user on 2026-11-04, which replaces LAN only. The server listens on the addresses of each interface in `listen_interfaces`, which are `eth0` and `tailscale0`. It doesn't listen on `eth0.2` or the Docker bridges, and "all interfaces" stays refused. Containers could still route to it, which is why client certificates are required. See Listening by interface.
- **Limits enforced by the server,** not the client: see Safety rules.
- **Files:** uploaded names are restricted to a safe character set and length, with a maximum upload size.

**What this doesn't solve:** `client.key` is a plain file with owner-only permissions. Claude Code's global settings deny its file tools access, but shell commands aren't fully covered, so anything running as the user could still read it. Whoever copies it can control Tramline until it's revoked. A key store that needs unlocking would stop Claude running commands unattended.

### Install and updates

- The first install has to copy the binary over somehow. SSH is simplest, but it's only used for that step.
- Installed once so far, on 2026-11-02, before the no-sudo rule. It's being kept. It created the `tramline` system user (uid 995, no login), plus the files below.
- Today, updating means the user runs `deploy/install.sh` again.
- `tramline update` is still planned, to send a new binary over the authenticated connection and restart, refusing during a print or SD upload. **Its design is open:** the service runs unprivileged with a read-only `/usr/local/bin`, so it can't replace its own binary there. Keeping the binary under `/var/lib/tramline/` would solve that, at the cost of letting a compromised server rewrite itself.
- Files: binary at `/usr/local/bin/tramline`, config in `/etc/tramline/config.json`, and everything the server generates in `/var/lib/tramline/`. Runs as a dedicated user in `dialout`.
- The server only reads the config, and never rewrites it. Secrets it generates stay out of the config, in `/var/lib/tramline/`, readable only by the service: the Hue key and the TLS key.
- JSON is used because Go's standard library reads it without a dependency.

### Config

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

### Listening by interface

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

### Proposed commands and API

Port `8726` is proposed, which spells TRAM on a phone keypad. Not currently in use on the Pi.

| Client command | Endpoint | What it does |
|---|---|---|
| `tramline init --ip <address>` | none | On the Pi, once: create the server's certificate |
| `tramline fingerprint` | none | On the Pi: show the server's public fingerprint |
| `tramline allow <fingerprint>` | none | On the Pi: allow a client |
| `tramline revoke <fingerprint>` | none | On the Pi: remove a client |
| `tramline clients` | none | On the Pi: list allowed clients |
| `tramline serve` | none | Run the server, add `--sim` for a simulated printer |
| `tramline keygen` | none | On a client, once: create its key and show its fingerprint |
| `tramline login <url> <server-fingerprint>` | `GET /v1/status` | Check both certificates and save the connection |
| `tramline status` | `GET /v1/status` | Power, temperatures, job, current activity |
| `tramline web [--url]` | `http://<host>:8726/`, no certificate | Open the read-only web page in a browser, or just show its address. Built |
| `tramline watch` | `GET /v1/events` | Live status stream |
| `tramline power on\|off` | `POST /v1/power` | Plug control; off refuses while hot or printing |
| `tramline preheat [bed] [nozzle]` | `POST /v1/preheat` | Start heating |
| `tramline cool` | `POST /v1/cool` | Heaters off |
| `tramline tram --bed-clear [--inset <mm>] [--repeat <n>] [--json]` | `POST /v1/tram` | Probe corners and report. Exits 3 if out of tram |
| `tramline tram info` | `GET /v1/tram/info` | Fixed read-only list: `M118`, `M114`, `M851`, `M503`, `G29 W` |
| `tramline upload <path>` | `GET` and `PUT /v1/files/<name>` | Save on the Pi unless it's already there, nothing else. Built |
| `tramline copy <name> [--no-wait]`, `tramline copy --cancel` | `POST /v1/files/<name>/copy`, `POST /v1/copy/cancel` | Copy a file on the Pi to SD, which is also how to retry, or stop a copy. Built |
| `tramline files [--json]`, `tramline files rm <name>` | `GET /v1/files`, `DELETE /v1/files/<name>` | List files on the Pi and the card, or delete one, with its copy on the card. Built |
| `tramline print <file> --from pi\|sd --bed-clear` | `POST /v1/print` | Turn on if needed, heat, check tram, print, turn off once cool; refused without the bed-clear confirmation. Built |
| `tramline cancel` | `POST /v1/cancel` | Cancel with `M524`. Built, simulator only |
| `tramline estop` | `POST /v1/estop` | `M112`, then cut the plug. Built, simulator only |
| `tramline schedule <file> --from pi\|sd --at "YYYY-MM-DD HH:MM" --bed-clear` | `POST /v1/schedule` | Check tram now, power off, print at that time if tram still passes. Built, simulator only |
| `tramline schedule list [--json]`, `tramline schedule cancel <id>` | `GET /v1/schedule`, `POST /v1/schedule/<id>/cancel` | List them, or cancel one that hasn't started printing. Built, simulator only |
| `tramline hue pair` | `POST /v1/hue/pair` | Pair after the bridge's button is pressed |
| `tramline hue plugs` | `GET /v1/hue/plugs` | List names and UUIDs, for filling in the config |
| `tramline update <binary>` | `PUT /v1/update` | Replace the server binary |

Long jobs return straight away with a job ID, and clients follow them through `status` or `watch`. An upload arrives at the Pi quickly over the network, so the slow serial copy carries on even if the client disconnects, and a failed copy can retry without re-sending the file.

### Web page

Built 2026-11-04, at the user's request. `tramline web` opens `http://192.168.1.121:8726/`: status, temperatures and progress, with the current job's file drawn in 3D, what's printed in orange, what's to come as a ghost, and a marker for the head. Drag to turn, right-drag to move, scroll to zoom, double-click to reset.

**On a phone:** open the same address, as `tramline web` only opens a browser on the machine running it. One finger turns, two fingers pinch to zoom and move together, and a double-tap resets. The panel gets its own part of the screen, at the top, or at the side with the phone turned, with a Details button for the rest, and the view frames the model to fit a tall screen.

**How it shares the port:** the first byte of a connection says which server gets it, in `splitListener` in `internal/server/web.go`. `0x16` starts a TLS handshake, and goes to the API with mutual TLS as before. Anything else goes to a separate `http.Server` with only the page's routes, so the API can't be reached from it, whatever the request. HTTPS in a browser fails, as a browser has no client certificate.

**What it exposes, with no login, to anything that can reach the port:** state, temperatures, the job's name, notes and errors, and the moves of the current job's file. It's plain HTTP, so anyone on the LAN can read them on the way. `WebStatus` is its own list of fields, so nothing added to the API becomes public by accident. Other files can't be listed or fetched. The port is still LAN only, by the `listen` setting.

**It can't affect a print:** it reads only what the server already holds in memory, and the uploaded file. It sends nothing to the printer or the Hue Bridge, and takes neither the operation lock nor the link. So plug state isn't shown, as reading it is a call to the bridge. Parsing the file takes about 90 ms for 92,794 lines on this machine, more on the Pi, is done once and kept in memory.

**The head's position is worked out, not measured.** `AUTOREPORT_POS` isn't in this firmware, and `M114` would wait in the queue, or can't be sent at all during a stream, which holds the link. So:

- The server reads the file's moves with `gcode.ParsePath`, using the same `Reader` and byte count as a job's position, and gives them to the page in a compact binary form.
- Times along the file come from its feed rates, scaled to the slicer's `;TIME:`.
- The page looks up the move at the job's position, steps back by `LAG_MOVES`, and glides the head there at the print's speed. Those are estimates of what's queued: 16 moves in a stream and 32 from the card, from the firmware's buffer sizes, not measured.
- **A stream** updates about once a second, as each line is answered, so it's close to live.
- **A print from the card** only has a position about every 30 seconds, and later if `M27` waits. Between them the page carries on at the file's speeds for up to 50 seconds, then waits. If it's more than 30 seconds out it jumps. So it's an estimate that gets corrected, and files under about 4 MB go to the card by default.

**Not drawn:** a print started from the LCD, whose file isn't on the Pi by that name, shows status only. The model is the file on the Pi, so a file replaced after its print shows the new one until the next job. The start G-code's heater waits and `G29` aren't in the times, as a job's position is what moves the head on.

**No libraries:** raw WebGL 2, embedded in the binary with `go:embed`, about 34 KB, in `internal/server/web/`. It's a single page polling `/api/status` once a second, as polling recovers by itself from a server restart. A content security policy allows only its own files. `go test` doesn't run the JavaScript, so a change to it needs a look in a browser.

#### Trying it on the simulator

This needs no Pi and no printer, and can run while the real server is printing, as it's all on this machine. After `./build.sh`:

```sh
./sim-web.sh ../projects/claude-monitor-case/stand-retro-print_PETG.gcode
```

It starts a simulated printer on `127.0.0.1:18726`, uploads the file, starts a print and opens the page in your browser. Ctrl-C ends it.

| Option | What it does |
|---|---|
| `--minutes <n>` | How long the print takes, default 10 |
| `--sd` | Prints from the simulated card, to see the position arrive about every 30 seconds, instead of streaming |
| `--no-open` | Shows the address without opening a browser |
| `--lan` | Listens on this machine's network address instead of `127.0.0.1`, and shows the address to open on a phone. A firewall on this machine may need to allow port 18726 |
| no file | Just the idle page |

The script keeps everything in a temporary folder that it removes at the end, including a throwaway client key, with `XDG_CONFIG_HOME` pointing there. So your real login isn't used, and the plain `tramline` command still talks to the Pi. To use the client on the simulator, for instance to try `cancel`, copy the line it prints, which starts with that `XDG_CONFIG_HOME`.

**What it can't tell you:**

- How close the drawn head is to a real one. The simulator has no real motion, so this only shows that the page follows a position.
- Temperatures from `--sd`: the simulator doesn't run a card file's lines, so the heaters stay where the tram check left them. A stream does follow the file's.
- Whether "Time left" is right. It comes from the slicer's times, so in a print sped up to 10 minutes it reads hours.
- The head jumps rather than glides in a sped-up print, as it falls over 30 seconds behind the file's own pace. The closer `--minutes` is to the slicer's time, the more it looks like a real print.

Plain `serve --sim` still has a bed that fails its tram check, which is how to see the page's `needs you` state. `--sim-level` gives it a bed that passes, `--sim-line-delay` sets each numbered line's time, which paces a stream, and `--sim-print-time` sets how long a card print takes.

### Print flow

1. **Check state.** Refuse if a print is running.
2. **Power on.** Skip if already on. Wait for the serial device and an `M115` reply.
3. **Upload.** Skip if the same file is already on the card. This comes before heating, as the user agreed, because a large copy can outlast the 30-minute heater timer, which would stop it. In stream mode there's no copy.
4. **Confirm.** Claude asks the user that the bed is clear, before `print`, as the head crosses the bed during the check.
5. **Preheat.** Bed 80°C, nozzle 150°C, matching `../cura/start_gcode_PETG.txt` so PETG doesn't ooze. The wait at temperature before the first tram check is `tram_soak_seconds`, default 0, as the user's bed is a thin flexible plate made for PETG, not glass, decided 2026-11-03.
6. **Check tram,** in a loop until it passes. See Tram before printing.
7. **Start.** In SD mode, `M23` then `M24`. The start G-code then homes, runs `G29 P1`, and heats the nozzle.
8. **After the print.** Wait for the nozzle to drop below about 50°C, then cut power if requested.

### Print design

**Approved by the user 2026-11-03, and built in `internal/server/print.go` and `internal/marlin/print.go`. Simulator only, and the simulator was written from the same reading of the source, so it can share any misreading.** That includes `cool` cancelling a print, `estop` always cutting the plug, cancelling a print stuck for 30 minutes, refusing files above the temperature limits, and fixing `{machine_depth}`. The findings come from the firmware source at tag `20231202`, read in `dwin.cpp`, `queue.cpp`, `e_parser.cpp`, `cardreader.cpp`, `MarlinCore.cpp`, `M1001.cpp`, `marlinui.cpp` and `hotend_idle.cpp`. **Nothing here has run on the printer.** `libproui` and `prouiex` are closed source, so they can't be read.

| Firmware | Consequence |
|---|---|
| The start-print popup is only reached from the LCD's file menu (`onClickSDItem`, `gotoConfirmToPrint`). `M23` and `M24` don't pass through it | A remote start shouldn't wait at the LCD, and `C10` shouldn't be needed. To test |
| `M23` aborts a running print, and `M27` reports `SD printing byte` for an open file even before `M24` or while paused | Check for a print before `M23`. "Printing" doesn't prove progress, so compare the byte position |
| Commands run in order, so `M27`, `M104` and `M140` wait behind up to 15 queued lines. During `M190` and `M109` no `busy:` is sent, only temperature lines | `M27`'s 3-second limit will fail during a print, marking the link stale, and each resync adds another line to the queue. `cool` can't take effect until the wait ends |
| `M524`, `M112`, `M108` and `M410` are caught as bytes arrive, and a line number before them is allowed, so they can be numbered | They can jump ahead of any waiting command, except during a save |
| `M524` ends `M190` and `M109` waits, then `abortSDPrinting` clears the queue, stops moves, and turns heaters and fans off | Its `ok` can be lost with the queue, so confirm with `M27` and an echoed marker |
| After an abort: `EVENT_GCODE_SD_ABORT "G28XY"` and `M84` | Cancelling moves the head to home at about its current height, raising only to a fixed clearance |
| `M112`: heaters and steppers off, then halted until power is cycled | The next step is the plug. It's parsed only when the board's emergency parser is on, which is off during a save |
| Finishing: `M1001` prints `Done printing file`, and `HAS_LEDS_OFF_FLAG` looks off as no LEDs are configured, so it doesn't wait | The end of a print can be seen on the link |
| `gotoPrintDone` sets `wait_for_user` and a remote start doesn't clear it. `M108` clears it, which should also close the screen | Send `M108` before `M23`, or the next print has `HOTEND_IDLE_TIMEOUT` turned off |
| `HOTEND_IDLE_TIMEOUT`: 10 minutes without extruder movement at 170°C or above sets both heaters to 0, unless moves are queued or `wait_for_user` is set. `M86` reads and sets it, and the printer's saved values may differ | A safeguard for a hung Pi during streaming, already in the firmware |
| `M85 S<seconds>`: kills the printer after that long with no moves queued. Heat waits and moves reset it. Off by default, not saved | A faster safeguard for a hung Pi during streaming: set it at the start, and clear it at the end |
| `POWER_LOSS_RECOVERY` is built in, off by default, and `M413` reads it | If it's on, a plug cut mid-print may bring up a resume popup at next power-on. To check |

**Also found in the existing files:**

- An SD print runs whatever temperatures the file says, outside the server's limits. So `gcode.Measure` now refuses `M104` or `M109` above 260°C, and `M140` or `M190` above 100°C, at upload.
- CuraEngine CLI files had an unresolved `G1 X0 Y{machine_depth}` in their end G-code. It's expected to have moved X only, so the print wasn't presented, but that's from memory of the parser, which hasn't been read. `../cura/resolve_settings.py` now writes Y220, as the setting's own value of 235 is past this printer's Y limit of 231.

**Design:**

- **Job:** `PrintJob` beside `CopyJob`, with a `Mode` of `sd` or later `stream`, saved to `/var/lib/tramline/print.json`. `tramline print <file> --bed-clear` needs a verified copy on the card with the right `M20` size. It sends `M108`, `M23` with the size checked, `M24`, then `M27`.
- **Lock:** a print doesn't hold the operation lock, as tram doesn't between steps. A `printRunning()` check refuses `tram`, `preheat`, `copy`, `tram info`, `files rm` and `power off`, without sending `M27`.
- **Polling:** a print goroutine holds one `M27` at a time, outside the lock, every 30 seconds. It waits as long as temperature lines keep arriving, fails after 10 seconds of total silence, and gives up after 15 minutes.
- **Idle timer:** restarted only when the byte position has moved, so a paused or stuck print still ends after 30 minutes, with `M524`. A running print counts from `M24`, closing the 30-second gap in the plug cut.
- **`cool` during a print:** becomes a cancel. Heaters off with the print still moving is the worse result.
- **`cancel`:** numbered `M524` written straight to the port, then confirmed.
- **`estop`:** takes no lock. During a copy, the plug only, as `M112` would be ignored. Otherwise `M112` then the plug straight away, without waiting on each other. With no link, the plug only. The hotend fan stops too, which the safety rules accept for an emergency stop.
- **Numbering:** the link owns one line counter, started with `N0 M110 N0` by the first command after the handshake. Each write takes its number under a short write lock, which `Urgent` also uses to get past a waiting command. An `ok N<line>` for an earlier line isn't taken as the reply, and `Resend` re-sends from a history. A copy numbers its own lines from 0, and the count starts again after it. **Not numbered,** as the firmware's count isn't known at those moments: `M115` and `M29` in the handshake, the blank line that clears garbage, and the `M29` and marker that end a save after a failure. A marker the firmware refuses is what proves a save is still open.
- **Unprompted lines:** a callback passes `Done printing file` and `Error:` halts to the job.
- **Restart:** at connect, `M27` and `M27 C` adopt a running print, including one started from the LCD. The handshake must stop re-sending `M115` while temperatures are arriving, or `M190` would fill the queue and drop the link after 30 seconds.
- **Link drop:** the job shows "link lost, print presumed running", then "ended while disconnected, outcome unknown" if `M27` later says so. If the plug reads off, the job fails with power lost.
- **Streaming, stage 10:** done, see Streaming design.

**Simulator tests:** finishing, every refusal, a wrong size on the card, cancelling, including during a heater wait with `M27` stuck behind it and `M524`'s `ok` lost, `cool` as a cancel, a stuck print, `estop` with and without a working plug and during a copy, a restart with the print running and after it ended, prints started and stopped from the LCD, a lost link, the plug off, a firmware halt, numbering, resends, and the CLI's calls end to end.

**Read-only test on the real printer,** 2026-11-03 at 19:27 to 19:33 GMT, with `dev-20261103192526`. Logs are in `test-logs/2026-11-03-print/t1-info/`.

- **Numbered commands work:** `tram info` replied `ok N4 P15 B15` to `ok N10 P15 B15` with no resends or errors.
- **Power-loss recovery is off:** `M413 S0`, so a plug cut shouldn't bring up a resume popup.
- **Hotend idle timeout is on:** `M86 B0 E0 S600 T170`, matching the source: 10 minutes, 170°C and above, both heaters to 0.
- **Connected** about 11 seconds after `power on`, with temperatures. `power off`: USB dropped about 2.5 seconds later.
- **Pi watch:** 70 checks over 5 minutes of ping, login, `sync`, throttling and failed units, none bad.

**Cold print tests on the real printer,** 2026-11-03 at 19:34 to 19:44 GMT, with `dev-20261103192526`, using `dry-run.gcode`: no heat, no G-code movement. Logs are in `test-logs/2026-11-03-print/`. These settle Open question 2.

| Test | Result |
|---|---|
| Remote start | **No LCD confirmation:** it went straight to the print screen, reported by the user, so `C10` isn't needed |
| To the end | About 61 seconds. `Done printing file` arrived, and the job finished straight away. The LCD shows print done with a Continue button, which doesn't block anything |
| `M27` while printing | First reply took about 16 to 18 seconds, behind the queue. `M108` then `M24` worked, 0 resends |
| Starting with the print-done screen up | It closed, reported by the user |
| Cancel after 15 seconds | About 10 seconds to confirm. `//action:cancel`, `Print Aborted`, then homed to `X:0 Y:0 Z:5`, with heaters still 0. LCD shows print aborted with a Continue button. `M524`'s `ok` did arrive, so losing it is simulator only |
| `estop` after 15 seconds | `M112 Shutdown` and `Printer halted` about 20 ms after the request, plug read off 0.33 seconds later, USB dropped 2.9 seconds after the request. 0.4 seconds for the whole command |
| Pi watch after `estop` | 70 checks over 5 minutes, none bad |
| Power on after `estop` | Connected in 11 seconds. The card lists normally |

**Server restart during a cold print,** 2026-11-03 at 19:45 to 19:53 GMT, with `dry-run-long.gcode`. Logs are in `test-logs/2026-11-03-print/t5-restart/`.

- **The print and board survived both restarts:** no `start` line, messages carried on, the LCD didn't change. Reconnected about 15 seconds after the restart, as `M115` waits behind the queue.
- **The first restart failed to pick it up,** with `dev-20261103192526`: a 30-second limit on asking kept running out behind the queue, and each retry queued more. Fixed in `dev-20261103194900`, which asks once and waits, without holding anything else up. The simulator had missed this, as its queue answered too quickly.
- **The second restart passed,** with `dev-20261103194900`: picked up at 69% about 64 seconds after the restart, then cancelled.

**First real print,** 2026-11-03 at 19:54 to 20:04 GMT, with `dev-20261103194900`, `test-tile_PETG.gcode`. Logs are in `test-logs/2026-11-03-print/t6-tile/`. How the part came out is for the user to report.

| Stage | Time |
|---|---|
| Copy | 2.2 s, 1020 lines, 13.9 KB/s, 0 resends |
| Bed from 23 to 80°C, homing, `G29 P1`, `C108`, nozzle to 235°C | 7 min 11 s, from 19:54:24 to about 20:01:35 |
| Printing | 2 min 45 s, matching the slicer's estimate of 165 s |
| Total | 9 min 56 s |

- **No stall** at `G29 P1` or `C108`, and no resends.
- **`M27` waited through it all:** the first one was sent at the start and answered about 7 minutes later, with temperatures still arriving. The old 3-second limit couldn't have worked. Once printing, position moved every 30 seconds or so.
- **Targets followed the file:** 150, 235, then 230°C, and bed 80, then 75°C, then both 0. `Done printing file` came as the heaters went off, and the job finished at once.
- **`M84 X Y E`** answered `X not disabled. Shared with Z.`, and likewise for Y and E0, so on this printer it leaves them on until the 120-second stepper timeout. Harmless.

**Cancelling a real print,** 2026-11-03 at 20:10 to 20:16 GMT, with the same tile, in the first layer with the nozzle at 235°C. Logs are in `test-logs/2026-11-03-print/t7-tile-cancel/`.

- **Targets read 0** in the next status, about 5 seconds after the request. The cancel was confirmed 7.4 seconds after it, homed to `X:0 Y:0 Z:5.90`.
- **Stage 9 passed.** Reported by the user for the first print: adhesion good, no stringing, not foamy, as good as manual prints. The LCD showed the file name, and the mesh viewer opened and closed.
- **Heat from a warm bed:** 5 min 10 s from `print` to printing, bed starting at 52°C.

**User's requests after this test** are built, see Tram before printing.

**Tests, each asked for separately:** read-only `M413`, `M86`. A cold file of `G4` and `M118` lines, to finish, to cancel, to `estop`, and across a server restart. Then a small real print to finish and to cancel, at the profile's temperatures: bed 80 then 75°C, nozzle 150, 235, then 230°C.

### Streaming design, stage 10

**Approved by the user 2026-11-04, and built the same day in `internal/marlin/stream.go`, `internal/marlin/sd.go` and `internal/server/stream.go`. Passed on the real printer, see the results below.** It follows the lost-lines tests, see Lost lines: test B, which lost nothing at one line at a time. Those ran cold with card writes, so a stream during a print hasn't been measured.

- **When:** `print` streams a file that isn't verified on the card, which is what `mode auto` leaves for files over its 5-minute limit. Everything up to the start is the same as an SD print: `heating`, `soaking`, `checking`, `adjusting`, `starting`, with no override on the tram check. The job has `Mode: stream`, and `Pos` counts bytes sent.
- **Sending:** one line unanswered at a time. `sendLines` moves onto the link's own counter and history, so `Urgent` and a stream can't disagree on numbers, and copies keep window 120 through the same code. No `M28`. The file comes from the Pi through `gcode.Reader`, with the same checks as a copy.
- **A lost line with nothing after it:** this gives no error, and silence can't show it, as `M109`, `G29` and a full planner can legitimately hold an `ok` for minutes, and temperature lines keep arriving regardless. So after 5 seconds with no `ok`, the next line goes anyway: if a line was lost, the firmware rejects the new one and asks for it, and if not, it just waits in the queue. Up to 4 lines may build up this way. The file ends with a numbered `M400` and an `M118` marker, so the last real line is always followed by something.
- **Resends:** `sdSettle`'s 200 ms could empty the planner on short moves, so a stream restarts as soon as the `ok` after the `Resend` arrives, unless more lines are unanswered.
- **Hung Pi:** `M85 S300` after the tram check, cleared with `M85 S0` at the end, on a cancel and on a failure. It kills the printer after 5 minutes with no moves queued, which needs a power cycle. `M86` is read at the start, expected `S600 T170`, and a different value is logged. `M108` comes first, as with SD, because `wait_for_user` turns that timeout off.
- **Finishing:** the marker's echo after `M400`. A stream doesn't get `Done printing file`, and may not get the print-done screen.
- **Cancel:** stop sending, then urgent `M108` to end a heater wait and urgent `M410` to stop moves. Then queued `M104 S0`, `M140 S0`, `M107`, a relative lift of `Z5`, `M85 S0` and `M84`. There's no `G28XY`, so the head doesn't cross the part, and `M524` isn't used as it only acts on the card. `cool` remains a cancel, and `estop` is unchanged.
- **Idle timer and stuck prints:** an answered line counts as moving on, with the same 30-minute limit.
- **Restart or lost link:** unlike SD, the print can't carry on. The job ends as `failed`, with no resume. The next connection turns the heaters off, and otherwise `M85` and the existing timer and plug cut apply. Don't run `install.sh` during a stream.
- **Logging, in the same build:** each `M27` wait, lines that arrive during a wait and are now thrown away, temperatures every minute or so, and in-flight bytes and timing at each resend.
- **Unknowns to test:** what the LCD shows during a stream, whether its pause and stop work, whether `M85` behaves as read, and how long `ok` takes during real moves.

**Tests, each asked for separately:** the simulator first, done. Then a stream of `G4` and `M118` to the end, a cancel, and `M85` firing when sending stops. Their file is cold, but the tram check before it still heats to 80/150 and moves the head, and the heaters stay on after it, as nothing in that file turns them off, so `tramline cool` follows. A cancel lifts the head 5 mm. Use a new name, uploaded with `--pi-only`, as `dry-run.gcode` may be verified on the card and would print from there. `M85` needs sending to stop with the board still on, which the user can do with `sudo systemctl stop tramline`. Then the test tile hot, to the end and to a cancel. Then a long print.

**Decided by the user 2026-11-04:** `M85 S300`, which also means an LCD pause of over 5 minutes kills the print. The cancel's `Z5` lift with no homing, so the head stays above the part. No resume after a restart or lost link.

**As built, details the design left open:**

- **Nudges also stop at the window:** a line only goes early if at most 120 bytes would be unanswered, as well as at most 4 lines. The firmware doesn't read its 128-byte receive buffer during a heater wait or `G29`, so 4 long lines could overflow it.
- **A lost end:** with the file sent and no marker back, each nudge repeats the marker on a new line number, so a lost `M400` or marker gets rejected and asked for, up to 10 times. The first echo finishes the job.
- **After a `Resend`:** lines before the one asked for did arrive, so they stay unanswered and the usual one-at-a-time rule applies. The file restarts on the `ok` that follows, or at the next nudge if that `ok` is missing. It doesn't wait for rejections of later lines: the firmware empties its buffer before that `ok`, and its own rule of dropping a line it already has makes a late one harmless. An extra rejection just asks again.
- **A request for a line before the file** fails the job, as it means the board restarted or lost its place. A dead board fails it too: no reply and no temperatures for 10 seconds. Temperatures alone keep it waiting, up to the 30-minute limit.
- **Cancel's stop:** the line count starts again after `M108` and `M410`, so a `Resend` can't send lines of the file out again. Both repeat every 5 seconds until `M104 S0` is answered, in case one was damaged or a second heater wait was behind the first. The stop may take up to 5 minutes, behind a queued `G29`. Up to 4 queued lines still run after `M410`, which doesn't clear the queue.
- **A failure with the link still up** stops the printer the same way as a cancel. A halt or `estop` sends nothing further.
- **Restart or lost link:** the job fails. The heater timer keeps running, so the plug cut still applies, and `heat.printing` stays false, unlike an SD print. At the next connection, `M108`, heaters off and `M85 S0`, retried until they work, and nothing can heat, print or copy until then. The head isn't lifted, as its state isn't known.
- **What counts as verified:** any file whose last copy wasn't verified streams, which includes a failed copy under `mode sd`. A verified file always prints from the card, even under `mode stream`.
- **While a stream's job is active:** `files rm` and replacing the file by upload are refused.

**First stream on the real printer,** 2026-11-04 at 17:29 to 17:37 GMT, with `dev-20261104172605`, `stream-dry-run.gcode`, a copy of `dry-run.gcode` on the Pi only: 60 lines of `G4 S2` and `M118`, no moves in the file. Logs are in `test-logs/2026-11-04-stream/t1-end/`.

- **Result:** `finished`, 30 of 30 echoes, 2 seconds apart, 0 resends, 0 nudges, no errors, about 60 s from 17:36:13.6 to 17:37:13.9. `M86` read `S600 T170` as expected. `M85 S300` and `M85 S0` were answered, but `M85` firing is untested. No line took over 5 seconds, and there were no real moves, so this says nothing about `ok` during moves.
- **First real failed tram check:** range 0.055 mm, lower back-left, from 0.077, 0.048, 0.035 and 0.090. It parked, nothing started, and status gave the knob and direction. The user reports the LCD showed `Lower back-left CCW`. After the user's turn, `print --continue` rechecked: 0.037 mm, pass, and the file started, also the first real run of `--continue`. Heating took 4 min 19 s from 23 C.
- **After the file:** the heaters stayed on until `tramline cool`, as expected with this file.

**Cold cancel on the real printer,** 2026-11-04 at 17:38 to 17:39 GMT, same build and file. Logs are in `test-logs/2026-11-04-stream/t2-cancel/`. Tram check 0.029 mm, pass. Cancelled 15 s into the stream, confirmed 1.75 s after the request, `cancelled`, targets 0, 8 of 30 echoes, no resends or errors. Whether the head lifted, and the LCD after it, are for the user to report.

**LCD during a stream:** on the `G4` and `M118` file the user reports what looks like the idle screen, as if nothing is going on, so that file has no print screen and no pause or stop. From the source at tag `20231202`, not tested: `dwin.cpp` opens the print screen when the firmware's printing state changes, and `M75` starts the job timer and sets the title, so `M75` may bring it up. From memory of Marlin, not checked, `M190` and `M109` may start that timer by themselves through `PRINTJOB_TIMER_AUTOSTART`, so a real file may behave differently, which the tile test will show. On that screen, `ui.abort_print` only stops the timer and sends `//action:cancel`, and `ui.pause_print` injects `M25`, neither of which would stop the Pi sending. So **the LCD's stop and pause shouldn't be expected to stop a stream, unless Tramline learns to act on `//action:cancel`,** which isn't built. Until that's decided, a stream stops with `tramline cancel`, `tramline estop` or the printer's switch.

**`M85` test, not completed,** 2026-11-04 at 17:43 to 17:51 GMT, same build, `stream-dry-run-long.gcode`. Logs are in `test-logs/2026-11-04-stream/t3-m85/`. `M85 S300` went at 17:46:29, and the user stopped the service at 17:46:48. **`M85` didn't get to fire,** as the user restarted the service at 17:49:44 and it was cleared at 17:50:50, so firing is still untested.

- **Why it was stopped early, a false alarm:** at 17:48:47, 2 minutes after the stop, the noise dropped and it sounded like the nozzle fan stopping. The user then saw the hotend fan still spinning and the steppers moving freely, so it was `DEFAULT_STEPPER_TIMEOUT_SEC 120` releasing the steppers. That fits the source's config at tag `20231202`, with `E0_AUTO_FAN_PIN -1` and no controller fan, so the fan isn't under firmware control. A hung Pi leaves it running. Z releases too, by `DISABLE_IDLE_Z`.
- **Restart path, first real run:** the job failed with `the server restarted during a stream`, and `print: heaters and M85 off` followed at the next connection, with targets reading 0.
- **Slow reconnect, fixed afterwards, simulator only:** connecting took 61 s, as the first `M115`'s reply lost its `FIRMWARE_NAME` line, then the handshake waited out its 1-minute retry while temperatures arrived. So the heaters stayed on for 66 s after the restart. Now a reply with `Cap:` lines counts as an answer, and `M115` goes again at once.
- **LCD:** the idle screen with its menus and `Starting print` on the status line, with no time or progress, throughout.
- **Source, for the LCD:** `PRINTJOB_TIMER_AUTOSTART` and `PARK_HEAD_ON_PAUSE` are both defined in that config, so a real file's `M190` and `M109` may bring up the print screen, and its Pause may try to park the head while the Pi keeps sending.
- **Safer repeat:** a file starting with `M104 S0` and `M140 S0` has no heat on while waiting for `M85`.

**`M85` firing on the real printer,** 2026-11-04 at 17:54 to 18:03 GMT, with `dev-20261104175252`, `m85-test.gcode`, which turns the heaters off first. Logs are in `test-logs/2026-11-04-stream/t3b-m85/`. `M85 S300` went at about 17:57:20, and the user stopped the service at 17:57:35. **It fired:** the user reports the LCD showing printer killed at about 18:02 GMT, a time given to the minute, which fits the predicted 18:02:20 to 18:02:37. That can't show whether `G4` resets the timer. After the restart at 18:02:51 the job failed with the right reason, and the killed board doesn't connect, as expected. The power cycle afterwards is still to do.

**First real print by streaming,** 2026-11-04 at 18:05:28 to 18:14:31 GMT, with `dev-20261104175252`, `stream-tile_PETG.gcode`, the same bytes as `test-tile_PETG.gcode`, on the Pi only. Logs are in `test-logs/2026-11-04-stream/t4-tile/`. How the part came out, and the LCD, are for the user to report.

- **Result:** `finished`, 1020 lines, 0 resends, no errors, no kernel errors. Tram check 0.048 mm, pass, close to the limit.
- **Time:** printing from 18:11:46 to 18:14:31, 2 min 45 s, the same as the card and the slicer's 165 s, so no sign of starving the planner on this small file. 9 min 3 s in all.
- **Reported by the user:** the part looks the same as the one from the card. The print screen opened, so a real file does start it, unlike the `G4` and `M118` file. It shows elapsed time, with remaining time stuck at 0 and a progress bar that doesn't move. At the end it showed print completed with a Continue button.
- **Nudges:** 10 logged. 8 were at `G28`, `G29` and `M109`, as designed, 1 near the prime line, and 1 at the end's `G1 X0 Y220`, a long move behind a full planner. None caused a resend.
- **`ok` timing is only bounded, not measured:** nothing logs each line's wait, so all this shows is no wait over 5 seconds during printing bar the end. Adding a summary would answer it properly before a long print.
- **Now seen in the log, as asked:** `G29`'s progress, position reports, `M84`'s `not disabled` lines, and temperatures about once a minute: targets followed 150, 235, then 230, and bed 80 then 75.

**Cancelling a real stream,** 2026-11-04 at 18:21:30 to 18:28:03 GMT, with `dev-20261104181934`, the same tile. Logs are in `test-logs/2026-11-04-stream/t5-tile-cancel/`. Tram check 0.040 mm, pass.

- **Result:** cancelled 30 s into printing, at 29%, with the nozzle at 235 C. `cancelled` 2 s after the request, targets 0, no resends or errors. Whether the head lifted, and the LCD, are for the user to report.
- **Reported by the user:** the progress bar moved and remaining time showed, so `M73` works. The head lifted and stayed put, without crossing the part. Afterwards the LCD said print done, progress 100%, though it was cancelled at 29%, so that screen doesn't mean the print finished. `tramline status` is what to believe.
- **After `M410`:** the replies show `P14`, one move planned after it, so one queued line did run, as expected. No more than that.
- **`ok` times, the first real figures:** 330 lines, 203 under 50 ms, 107 under 500 ms, 10 under 5 s, 10 over 5 s. The longest, 2 min 12 s, was `G29`, then `M109`. Those long waits make the mean of 1.8 s meaningless. **The planner averaged 0.5 moves free,** so it was nearly full while printing, and the 10 replies showing it empty fit the start G-code, where the same log's earlier minutes show 9. So there's no sign of starving, on one small file of 30 seconds' printing.

**The LCD's Stop during a real stream,** 2026-11-04 at 18:32 to 18:37:49 GMT, with `dev-20261104181934`, the same tile, pressed by the user about 30 s into printing. Logs are in `test-logs/2026-11-04-stream/t6-lcd-stop/`. Tram check 0.033 mm, pass. `//action:cancel` arrived at 18:37:47.062, the file stopped at once, and the job ended `cancelled`, `stopped from the LCD`, 2.2 s later, targets 0. The user reports it worked. No `G28XY`, as read from the source, and 0 resends. `ok` times: 294 lines, 191 under 50 ms, 91 under 500 ms, planner averaging 0.6 moves free.

**The LCD's Pause during a stream isn't supported or tested:** the user chose on 2026-11-04 to skip that test, so don't press it during a stream. From the source it should park the head and wait, with the stream held behind it, and over 5 minutes paused `M85` would kill the print. To stop, use the LCD's Stop or `tramline cancel`.

**Long print by streaming,** 2026-11-04 at 18:42:56 to 19:46:01 GMT, with `dev-20261104181934`, `wavy-tower_PETG.gcode`, 182,028 lines, 6,034,232 bytes on the card's count, print job 16. Logs are in `test-logs/2026-11-04-stream/t8-long/`. Tram check 0.037 mm, pass.

- **Result:** `finished`, no errors, no link loss, and no kernel messages at all since 18:04:46, by the live capture in `t1-end/kernel.log` and by `journalctl -k`. 182,184 lines with an `ok`, including `M73`.
- **No real resend:** 0 `sd: printer asked for line`, so in-flight bytes and timing have nothing to show, and resends of numbered commands stay simulator only. At the copy's rate of 1 line in 5,800, about 31 would have been lost, so this adds to test B's result at one line at a time.
- **Time:** 63 min 5 s in all. About 57 min 20 s from the end of the prime lines, 18:48:39, to the end G-code, 19:45:59, against the slicer's 3479 s, 57 min 59 s, so about 39 s or 1% quicker. Those two times come from a position report and a one-off status line, so they're good to a few seconds.
- **Planner:** 0.0 moves free on average, and no reply showing it empty, in every minute of printing from 18:50 to 19:45, bar the damaged replies below. So it never starved, and the printer's moves set the pace rather than the link.
- **`ok` times by stage:** first layers, 18:50 to 18:55, 336 to 1,213 lines a minute, mean 50 to 179 ms, longest 1.17 s. The middle, 18:55 to 19:39, 3,456 to 4,130 lines a minute, up to 69 a second, mean 14.5 to 18 ms. The longest wait there was mostly 163 to 260 ms up to 19:12 and 325 to 462 ms from 19:13, a step that isn't explained, with no effect on the planner. Top layers, 19:39 to 19:46, 113 to 1,104 lines a minute, mean 55 to 540 ms, longest 1.64 s.
- **Whole stream:** mean 22.2 ms, 175,897 under 50 ms, 5,813 under 500 ms, 461 under 5 s, 13 over, the longest 1 min 26 s at `G29`. **The log's `planner free 1.6` and `empty at 18` are wrong:** two damaged replies account for the 1.6, and for 2 of the 18. The other 16 are 11 in the start G-code and 5 in the last half minute, as the planner ran out at the end.
- **Nudges:** 11. 10 in the start G-code, at `G28`, `G29` and `M109`, and 1 at the end's `G1 X0 Y220`, the same as the tile. None during printing, none caused a resend.
- **Three damaged replies, printer to Pi, cause unknown:** none of them cost a wait or a resend. At 19:19:11, `N99193 P0 B15` arrived without its `ok`. With no nudge, and a longest wait of 457 ms that minute, an `ok` must have arrived some other way, perhaps split from it onto its own line, which isn't logged. Between 19:30:25 and 19:32:25, two `ok` replies each had `P` of about the current line number: free-move totals of 144,059 to 144,445 in lines 142,264 to 146,121, then 148,180 to 148,562 in lines 146,121 to 149,945. That fits ` N` read as ` P`, though it's inferred, as `ok` replies aren't logged. Such a reply has no line number, so it answers the oldest line, the right one with a line at a time. All three were at the ` N` of the reply, which may mean something or may be chance, on three cases. Whether the same damage relates to the lost lines is unknown.
- **Temperatures:** nozzle within 229.7 to 230.2 C and bed at 75.0 C from 18:52 to the end G-code, at each once-a-minute log line.
- **Reported by the user:** it looks great from above, and pretty good overall, similar to earlier prints, with slight imperfections the user expects from PETG on this printer. The progress display tracked perfectly.

**A copy on the rewritten `sendLines`,** 2026-11-04 at 19:55:45 GMT, with `dev-20261104181934`, `copy-check.gcode`, the test tile's bytes under a new name. Logs are in `test-logs/2026-11-04-copy/`. **Passed:** 1020 lines, 30,208 bytes in 2.4 s, 12.6 KB/s at window 120, size checked with `M20`, 0 timeouts, no kernel messages. **One real resend, recovered:** `Line Number is not Last Line Number+1`, asking for line 564, with 3 lines and 114 bytes unanswered, up to line 566, the oldest sent 8 ms ago. So that log line has now run for real, on a copy. Resends of numbered commands during a print are still simulator only. `copy-check.gcode` remains on the Pi and the card as `COPYCHEA.GCO`.

**Added after the first real print, approved by the user 2026-11-04, and since run on the printer as noted above:**

- **The LCD's Stop:** `//action:cancel` during a running stream is now a normal cancel. An SD print ignores it here, as its own cancel says the same. From the source, `abort_sd_printing` is only set with a file open, so no `G28XY` should come of it during a stream.
- **Progress:** `M73 P<percent> R<minutes>` goes ahead of a line whenever either changes, by bytes for the bar and from Cura's `;TIME:` and `;TIME_ELAPSED:` for the minutes. About 100 extra lines plus one per layer, on the same count as the file's lines but not in `Pos`. The LCD may reset them when its print screen opens at `M190`, so the first ones may not show. `M73` is compiled in, by `SET_PROGRESS_PERCENT` and `SET_REMAINING_TIME` in the source's config.
- **`ok` times:** each minute and at the end, `stream: ok times` logs lines, mean, longest and which line, how many fell under 50 ms, 500 ms and 5 s, and the planner's average free moves, with how many replies showed it empty.

**Simulator changes, as it missed all of what a stream relies on:** `G1` was an unknown command, there was no planner, `M109` and `M190` didn't exist, `M108` and `M410` weren't urgent, and there was no `M85` or `M86`. It now has a 15-move planner that holds `ok` when full, `M400`, blocking heater waits that `M108` ends, urgent `M108` and `M410`, an `M85` kill, and `G4`.

**How the simulator still differs from the real queue, so its results can't settle these:**

- It checks a line's number when it comes to run it, where the real board checks as it reads, ahead of running what's queued. So a real rejection can come before an earlier line's `ok`. The simulator can't do that, so a scripted test covers it.
- It reads input at any time into one large queue, with no 128-byte limit, so it measures bytes waiting but can't overflow or lose them.
- Its `M85` resets on moves, `G4` and heater waits, by reading the source, so whether `G4` really resets it is unknown. `dry-run.gcode` is short enough not to matter. `dry-run-long.gcode` could be killed at 5 minutes if it doesn't.
- Its timing is made up: temperatures every 40 ms, moves of a fixed length, and `M27` answered at once unless a test stalls it, so nothing here says how long `ok` takes during real moves.
- `M410`'s effect on position, the Z lift afterwards, the LCD, and its pause and stop aren't modelled at all.

**Simulator tests:** `internal/marlin/stream_test.go`, `internal/server/stream_test.go` and a client test: to the end, order of commands, a failed tram check holding it back, a full planner, limits during a heater wait, a lost line at the start, middle and end of the file and at `M400` and the marker, a damaged line, no settle, numbering around `Urgent`, a rejection before an earlier `ok`, total silence, cancel, cancel in a heater wait, `cool`, `estop`, a stuck stream, a lost link, the plug off, a restart, a bad line mid-file, an `M85` kill, and the new logging. The copy tests pass unchanged on the shared `sendLines`, though a copy on the new code hasn't run on the printer either.

### Tram before printing

**Asked for by the user after stage 9: always check tram before a print, in a loop, with no override, and show the steps on the LCD. Design approved and built 2026-11-04 in `internal/server/prepare.go`. Only the pass path has run on the printer, see First real run and Stand print result.**

`POST /v1/print` makes its refusals and its card check, then returns straight away with the job in `heating`. The job goes `heating`, `soaking`, `checking`, then on a pass `starting` and `running`, or on a fail `adjusting`, where nothing runs until `tramline print --continue`, which goes back to `heating`. The limit is the wizard's 0.05 mm.

| State | Heaters | Head | Server restarts | `cancel` or `cool` |
|---|---|---|---|---|
| `heating`, `soaking` | On, 80/150 | Wherever it was | Becomes `adjusting`, with a note | Immediate: heaters off |
| `checking` | On | Moving | Becomes `adjusting`, "may be low" | Finishes the current `G28` or `G30`, parks, heaters off |
| `adjusting` | Held until the timer runs out, then off, and the job keeps waiting | Parked at X115 Y115 Z30 | Stays `adjusting` | Immediate: heaters off, head stays parked |
| `starting` | On | Parked | If `M27` shows this file printing, adopted as `running`. Otherwise `adjusting` | Waits for the start, then a normal `M524` cancel |

- **Heating:** `M140 S80` and `M104 S150`, then the temperature reports must stay within 1°C of both for 10 seconds, failing after 15 minutes. It doesn't use `M190`, so the link stays free.
- **Check:** one normal tram run at inset 25, so it's in `status` and the history. It always homes first, as the steppers time out during a wait and the user may have probed from the LCD. It parks on a pass too, as the file might fail to start.
- **Wizard during a wait:** the user may run the LCD's tram wizard while `adjusting` until it's green, then ask for the final check. Only Tramline's check starts the file. No code is needed for this, and it hasn't been tried.
- **Soak:** `tram_soak_seconds`, 0 to 1200, so it can't use up the heater timer. It runs before the first check, and again if `--continue` finds the heaters were off. Other rechecks skip it. All the hot readings so far followed a 5-minute soak, so a soak of 0 hasn't been tried.
- **Heater timer:** the idle timer keeps running through all these states, and `--continue` restarts it, as `preheat` does, so a check can't be cut short, decided by the user. A timer that runs out leaves the job waiting with the heaters off, and `--continue` heats again.
- **`--continue`:** also counts as saying the bed is clear, decided by the user, as they're at the printer. It's refused unless a job is `adjusting`, and with the plug off or no link, leaving the job waiting.
- **Targets changed** by the LCD or the timer during a check or heating: the job goes to `adjusting`, parked if it was checking.
- **Failures:** a failed command or a lost link ends the job as `failed`, not parked, heaters off if it can reach them, or else the idle timer and plug cut apply as usual. A firmware halt ends the job in any state.
- **Other commands:** `print`, `tram`, `preheat`, `copy`, `files rm` and `power off` are refused until the job ends. `estop` still acts at once. A print started from the LCD closes the job, and adoption follows that print.
- **Temperatures:** fixed at 80/150, matching the PETG start G-code, decided by the user. Another material or bed target would need this changed.
- **Knob direction:** `status` gives it as seen from above, lower counter-clockwise, raise clockwise, measured only on back-left.

**LCD, from reading the source, so it may differ, as `libproui` is closed:** `M117` only sets the status line at the bottom, and the title of the print screen is fixed to "Printing" by `drawPrintProcess`. So before `M24`, `M108` closes the print-done screen and `M117` shows `Heating bed`, `Checking tram`, for example `Lower back-left CCW`, and `Starting print`. A plain `M117` clears it if the job ends early. `M117` must come after `M140` and `M104`, as each writes its own message and sets a function that resets the line at temperature (`set_heating_message`, `M140_M190.cpp`), and `M117` cancels that function. After `M24` the firmware writes its own status: `M190` "Bed Heating", which is skipped when already hot, `G29`'s own leveling screen and a probing progress line, `M109` "Heating...". `M117 Heating nozzle` was added to the start G-code just before `M109`. By the source `M109` should replace it straight away, but on the stand the user reports it stayed up during the heat, so the source reading is wrong here. The title will still say "Printing" during the mesh and nozzle heat, and `M117` can't change that.

**First real run,** 2026-11-04 at 10:58 to 11:04 GMT, with `dev-20261104105333` and `dry-run.gcode`, from cold. Logs are in `test-logs/2026-11-04-tram-loop/`. Only the pass path has run on the printer. A failed check, `--continue`, the wizard during a wait, a soak above 0, and every failure and restart path are still simulator only.

- **Times:** heating 4 min 19 s from 22°C, check 39 s including 5 s to park, file started 40 ms after parking, file 60 s.
- **Check with no soak:** range 0.035 mm, pass, from average +0.011, -0.011, -0.018, +0.017. Very close to the soaked five-round averages of 2026-11-03, on one run.
- **No errors, resends or stale link** in the server's log.
- **LCD, reported by the user:** "Nozzle heating", then "Ender3V2 ready" while the bed was still heating, as `M117` went before `M140` and `M104`. During the check it showed the bed's coordinates at each corner, which is `G30` replacing `Checking tram`.

**Stand print start,** 2026-11-04 at 11:13 to 11:21 GMT, with `dev-20261104110522`, from a warm bed. Logs are in `test-logs/2026-11-04-stand/`.

- **LCD fix confirmed by the user:** `Heating bed` stayed up while heating, and `Checking tram` showed.
- **Times:** heating 3 min 24 s from bed 40°C, check 41 s, file started 40 ms after parking.
- **Check:** range 0.040 mm, pass, from average +0.016, -0.013, -0.021, +0.019.
- **Copy:** 92,794 lines in 217.6 s, 12.7 KB/s, size checked, 0 timeouts, but 16 resends, all `Line Number is not Last Line Number+1`, so whole lines seem to have been lost on the way, about 1 in 5,800. The resends worked. The cause is unknown, and it matters for streaming, where a resend happens during the print. See Lost lines.

**Stand print result,** 2026-11-04, print job 8, with `dev-20261104110522`. Logs are `server.log`, `kernel.log` and `power-off-kernel.log` in `test-logs/2026-11-04-stand/`.

- **Outcome:** `finished`, with 2,756,863 of 2,756,863 bytes read and `Done printing file` at 15:31:37 GMT. The user reports the part came out mostly good, with minor stringing at the back that they think comes from the model, not Tramline. From a photo, wisps run along the waist above the base and across an inside corner, which looks like PETG stringing on travel moves. Damaged data is unlikely: lines are checksummed and the size matched, though the card's copy hasn't been compared byte for byte.
- **Time:** 4 h 10 min 34 s from `M24`, 4 h 10 min 17 s by the firmware, against the slicer's 4 h 19 min 37 s. That's about 9 minutes shorter, even with about 2 min 50 s of mesh and heating that the slicer doesn't count. Timed from the end of the prime line, the print ran at 0.96 of the slicer's time at line 84231 and 0.95 at the end, so the speed-up was steady.
- **Idle timer:** the first expiry was 11:46:58, and the print ran 3 h 45 min past it with no `heaters off, idle timeout` and no `hasn't moved on`. This rests on those lines being absent, the job finishing and the part being good, as temperatures aren't logged.
- **Link and firmware:** no link loss, no firmware halt, no `couldn't read its position`, no service restart. The Pi didn't reboot, and the kernel log has no entries from 11:13 until the power off.
- **Limit of "no firmware error":** only halts are always caught. Lines arriving while an `M27` waits are thrown away, so only 3 of the file's 6 `G92` position reports were logged. A non-fatal message could have been missed the same way.
- **First `M27`:** its wait wasn't logged and `status.log` stops at 11:21:03. It took at most about 2 min 51 s, as position lines logged at 11:23:53 only appear with no command waiting. The two `G92` reports just before them were lost, so an `M27` was waiting at that point, though it may not have been the first.
- **Position lines:** Marlin reports its position on `G92`, so `X: Y: Z:` lines in the log are normal. The one at 15:11:34 matches line 84231 of the file exactly, at X75.28 Y103.15 Z24.68.
- **LCD, reported by the user:** `Heating nozzle` stayed up during the heat, and the print-done screen came up.
- **End of print:** the bed came forward, the first real use of the `Y220` fix.
- **Cooling:** the nozzle was at 31°C and the bed at 51°C 7.5 minutes after the end, and 25.8°C and 43°C after about 12 minutes.
- **Power off:** `power off` at 15:43:19, with the kernel log captured. A clean USB disconnect at 15:43:22, with two `urb stopped: -32` lines just before it, and no errors after. The Pi was fine about 70 seconds later, which is a short watch compared with the earlier hang's timing.

**Lost lines: offline investigation,** 2026-11-04, from existing logs, the file and source reading, with no printer. The cause is still unknown. The scripts were throwaway, and the numbers come from `test-logs/2026-11-04-stand/server.log`, `test-logs/2026-11-03-sd-window/server.log` and the G-code.

- **Rate:** about 17 events in about 112,000 lines at window 120. The stand's rate may not be worse than earlier copies: the 15,264-line file would expect 2.6 at 1 in 5,800, and seeing 1 or fewer has a 26% chance. The rate at one line at a time isn't known, as only 2,000 lines have run that way, too few to expect one.
- **What the error rules out:** the firmware checks a line's number before its checksum, in `get_serial_commands`, and a damaged line or a lost newline would mostly fail the checksum. All 16 of the stand's errors were line numbers, so random byte damage on the wire doesn't fit, as only about 1 byte in 6 is in the number. Test A then gave 4 checksum errors in 17, see below, so partial damage happens too, but line-number errors still dominate, at 29 of 33. A lost first byte would give a missing-number error, not seen.
- **Tramline's code, by reading only:** `sendLines` sends in strict order, with each line in one write, from one goroutine. The only other writers are `M524` and `M112`, which didn't run, `M29`, and a newline at connect. No path that skips a line was found. A kernel or USB loss can't be seen from here, and the kernel log shows no errors.
- **Same lines each time?** Unknown. It needs a repeat copy.
- **8 KB card boundaries:** at windows 240 and 480, 14 of 18 resends were at 86 to 114 bytes past an 8,192-byte boundary of the file, so that overflow is closely tied to the board's stall. At window 120 it isn't: the stand's events spread across the cycle, the earlier event was at 137 bytes past one, and the stand's lean towards the boundary is weak (p about 0.05 for that one period, nothing after allowing for trying many periods). So the 120-byte losses are probably not that overflow, though some card effect isn't ruled out.
- **Line shape:** lost lines are more often `G0` moves (11 of 16 against 42% of the file, p about 0.03) and shorter (mean 35.8 bytes against 38.8, p about 0.03, partly the same effect). This wasn't corrected for trying several tests, so treat it as a lead. A short line is more likely to fit at the end of the window, so the board's buffer may matter.
- **Timing:** irregular, 0.6 to 23.5 seconds apart. The events do not fall at the `M155 S2` temperature reports, which come at about 1.48 s in a 2-second cycle.
- **What it means for streaming:** a stream would run far slower than a copy, about 6 lines a second on average on the stand, against 427 in the copy, and with one or a few lines unanswered. So the copy's rate may not carry over. All 35 real resends have recovered. The risk is the pause: `sdSettle` is 200 ms, while the planner's 15 moves could last much less on short segments. The 200 ms needs revisiting for a stream.
- **CH340 packet size:** `/sys` shows 32 bytes on the bulk OUT endpoint (`ep_02`, read 2026-11-04). So a single lost USB packet can't remove a 41-byte line, though more than one could.

**Lost lines: test A,** 2026-11-04 at 16:24 to 16:27 GMT, with `dev-20261104110522`, cold, no heat or movement. Logs are in `test-logs/2026-11-04-lost-lines/a-window120/`. An identical copy of the stand's file, by `cmp`, went to the card as `lost-lines-test.gcode`, `LOSTLI58.GCO`, at window 120, using a new name because `files rm` also deletes the Pi's copy. Both are still there.

- **Result:** 92,794 lines in 215.6 s, size checked, 0 timeouts, 17 resends against the stand's 16. The rate repeats closely: about 1 in 5,500 to 5,800.
- **Not the file's content or position:** none of the 17 lines matches the stand's 16, the nearest being 167 lines away. So the cause is timing or something else that changes between runs, on a fixed numbering and fixed bytes. The only other difference is the file's place on the card.
- **Errors:** 13 line-number and 4 checksum (at lines 889, 35281, 52456, 68578), where the stand had 16 and 0. The kernel log shows no USB errors.
- **The earlier leads didn't repeat:** `G0` lines were 8 of 17, about the file's 42%, or 19 of 33 combined (p about 0.06). Test A's positions show no lean towards 8 KB boundaries.
- **Reasoning, not measured:** a pause in the board taking in bytes, starting between lines and lasting at least one line (1.6 ms at 40 bytes), could give mostly whole-line losses and a few partial ones. Firmware that's partly closed can't confirm this.

**Lost lines: test B,** 2026-11-04 at 16:45:39 to 16:53:05 GMT, cold, no heat or movement. Logs are in `test-logs/2026-11-04-lost-lines/b-window1/`. An identical file, as `lost-lines-test-b.gcode`, `LOSTLID6.GCO`, at window 1, one line at a time.

- **Result:** 92,794 lines in 446.2 s, 6.2 KB/s, 208 lines a second, size checked, **0 resends and 0 timeouts**, where window 120 gave 16 and 17. That can't be chance. The kernel log shows nothing new.
- **Not a steady rate over time:** it ran twice as long, so a loss per second would give about 33.
- **So the losses need lines overlapping on the way to the board.** At window 1, no bytes arrive while the board handles a line. That also counts against test A's idea of a pause just after an `ok`, as that would have lost lines here too, showing as timeouts.
- **The cause itself is still unknown:** bytes arriving during a card write or a fuller buffer both fit, and this can't tell them apart.
- **Speed needed by a stream:** the stand averages about 6 lines a second, at most 10.2 in any layer by the slicer's times, and at most about 81 in any 1 second by a rough estimate from move lengths, ignoring acceleration, which overstates it. Window 1 managed 208 here, with card writes.
- **Limits:** one run, cold, with no moves and with card writes, which isn't the same as a stream during a print.

**Remaining tests, if wanted, each needing the user's OK, cold:** window 80, mostly 2 lines, as window 60 would rarely fit a second line. Numbered no-ops with no card write, which needs a small build. Neither is needed if the stream sends one line at a time.

Both test files were removed from the Pi and the card afterwards, and the user agreed to stop the printer tests here.

**Simulator tests:** `prepare_test.go`, covering the order of commands, a failed check then a recheck then a pass, refusals while waiting, cancel and `cool` in each state, `estop` waiting and during a check, a failed probe, a firmware halt, targets changed, the timer running out then `--continue`, soaking, restarts in each state, an LCD print during a wait and a lost link. All tests were written with the code, and the simulator comes from the same reading of the source.

### Scheduled prints

**Asked for by the user 2026-11-05, built in `internal/server/schedule.go`. Tests and the simulator only so far.**

| Step | What happens |
|---|---|
| `schedule` | Refuses a time under 15 minutes or over 30 days away, or a missing file. Switches the plug on if needed, waits for the link, then starts a normal print job marked `pre_check` |
| First check | The same `heating`, `soaking`, `checking` and `adjusting` as any print, with `print --continue` and `cancel`. The schedule is `checking` |
| A pass | The job ends as `scheduled`, heaters off, the schedule becomes `waiting`, and auto-off cuts the plug once the nozzle is cool |
| Its time | Switches the plug on, waits up to 90 seconds for the link, and starts a normal print job. The schedule is `starting`, then `printing` |
| Second check, pass | The file starts, by the card or the stream as usual |
| Second check, fail | Cancelled at once, decided by the user, with no recheck and no `adjusting`, as nobody is there. Heaters off, auto-off |
| The print ends | Any way it ends, auto-off runs, decided by the user. Every print now does this, see CLI changes below |

Decisions:

- **Storage:** `schedules.json` in the data folder, written with `writeAtomic` like `print.json`, as there are only a handful of records. SQLite would need cgo or a large third-party module, against the standard-library rule. The 20 newest ended records are kept. If the file can't be read, `schedule` and `schedule list` refuse rather than overwriting it.
- **Time:** the client reads a full date in its own timezone and sends RFC 3339, so the Pi being on GMT can't shift it. A date is required, and "6 am tomorrow" is for the skill to turn into one. The server looks at the wall clock every 5 seconds, not a timer set in advance, as the Pi has no battery-backed clock. Its clock runs behind after a boot until it syncs, which can only make a print late, not early.
- **Bed clear, decided by the user:** several can wait, but each confirmation only holds until a print starts. `bed_used_at` is saved whenever a file starts, from here or found running from the LCD, and a schedule whose `bed_clear_at` is older cancels itself without touching the printer. So two scheduled prints only both run if the second was scheduled after the first's part came off. It's cancelled, not held, as nobody can say the bed is clear at that moment.
- **Late, decided by Claude as a default:** past its time by over 15 minutes, because the server was down, it's `missed` and doesn't start. A first check that hasn't passed by the scheduled time is cancelled.
- **Busy, decided by Claude as a default:** anything using the printer at that time, including a job waiting in `adjusting`, cancels the schedule and leaves that job alone.
- **Files:** `files rm` and uploading over a file are refused while a schedule that hasn't started printing needs it.
- **Auto-off is stricter than `power off`:** it needs a connected printer with known temperatures, targets at 0 and the nozzle below 50°C, as `cutPowerBlocker` lets a lost link through. It turns the heaters off once at the start, for a file with no end G-code. After that, a target above 0, or any job, means someone is using the printer, and it stops and leaves the plug on. So does `power on`, or scheduling or firing another. If it can't confirm cool within 30 minutes it gives up with the plug on, so the fan keeps running. `off_pending` is saved, so it carries on after a restart.
- **Restarts:** a waiting schedule simply carries on. The first check goes to `adjusting` with the usual note. A check at the scheduled time also goes to `adjusting` as usual, so `adoptPrint` can follow a file that did start, then once the printer has answered it's cancelled, with nobody there to continue it. A stream cut off by a restart is failed as usual, which the schedule picks up on its next look.
- **Nothing tells the user** when one is cancelled or fails: `schedule list` and the log are the only record, and the printer will be off.
- **Web page:** only knows the `scheduled` job state. It doesn't list scheduled prints.

**CLI changes, 2026-11-05, asked for by the user:**

- **Auto-off is on every print** Tramline starts, however it ended, including a `cancel` while `adjusting`, not just scheduled ones. The user's words were "all scheduled prints, not just scheduled", taken to mean every print. It doesn't apply to prints from the LCD, or a tram check when scheduling that was cancelled or failed. When another print has taken over a job, closing it doesn't start one (`closeJobPower`), as its first step turns the heaters off. Its saved state stays in `schedules.json`.
- **`print` switches the plug on,** as `schedule` does, after refusals that don't need the printer, so a mistake doesn't power it up.
- **`upload` only saves on the Pi,** and `--sd`, `--pi-only`, `--no-wait` and `--window` are gone from it. `copy` is the only route to the card.
- **`mode` and `settings.json` are gone,** with `GET` and `PUT /v1/settings` and `FilesResponse.Mode`. An old `settings.json` on the Pi is ignored. Earlier sections that mention `mode auto` describe what was tested then.
- **`--from pi|sd` is required** on `print` and `schedule`, and `from` in the API, and it's stored on a schedule. Neither falls back: `sd` is refused with no verified copy, and `pi` streams even with one, which ends the "new name with `--pi-only`" workaround. The job's own `mode` is still `stream` or `sd`.
- **`<file>`** replaces `<name>` in the help, meaning the file's name on the Pi. Commands that take it still accept a path and use its last part.
- **Web page:** `WebStatus.Scheduled` lists all that haven't ended, soonest first, then the last 5 ended, with the reason. Times show in the browser's timezone. The page has no login, so this newly shows those names and times to anyone on the LAN or tailnet. `sim-web.sh --schedule <minutes>` tries it.

**Simulator run of these changes,** 2026-11-05, with the real binaries. `print --from pi` from a plug that was off took 5 seconds to return, finished at 19:34:37, and the plug went off at 19:34:42. The nozzle had read 126°C just before. `--from sd` with no copy and a missing `--from` were refused. The page was checked in headless Firefox with one waiting and one cancelled schedule, without WebGL, on `dev-20261105182938`. The two later small changes to the page, no red for a requested cancel and no bar for a job that never printed, weren't looked at again. A `--from sd` print through the whole flow ran in the tests, not this run.

**Tests:** `schedule_test.go` has refusals, the whole flow, adjusting at the first check, a failed second check, the bed guard with one and two schedules, a busy printer, cancelling in each state, file protection, missed and within the grace, a plug failure, restarts in each state, auto-off's rule, and one with a simulated printer that really goes away when its plug is off.

**Simulator run with the real binaries and clock,** 2026-11-05, 18:56 to 19:13 CET, with `dev-20261105175455`, so a client on CET against a server process using the same machine's clock. The file streamed from the Pi side, and the plug started off. The time was entered as 19:13 CET and stored as 18:13 UTC. The refusals worked, the first check passed and the plug went off, and `files rm` was refused. A second schedule was made and cancelled during its check. The server was restarted while the first waited, and it kept it. The heaters came on at 19:13:05, the file started after its second check at 19:13:22, and the plug went off at 19:13:27, after `status` had shown the nozzle at 95°C at 19:13:23. The log wasn't kept, as it was in a scratch folder. The only bug it found was a `status` line left showing after a cancelled check, since fixed in `dev-20261105181359`. A card print and a failed second check ran in the tests, not this run. The web page's one new line hasn't been seen in a browser.

**Not proven on the real printer:** all of it. Worth testing first, each with the user's OK, with a cold or tiny file at a time 15 to 20 minutes away:

1. Real Hue plug and reconnect at the scheduled time, which has only had the simulated plug. The repeat power cycles matter here because of the Pi hang and the plug's self-off, so capture the kernel log.
2. Whether a board powered on by the plug behaves the same at 05:59 with nobody watching. The user still needs to review Hue rooms and routines before relying on this overnight, as a routine could switch the plug.
3. A failed second check, by turning a knob during the wait, and auto-off from a truly hot nozzle: the simulator moves temperatures at 40°C a second, so the wait for below 50°C has only lasted seconds.

### Tram check

**This matches the firmware wizard, as the user asked.** The rule below is what the LCD uses, with no custom thresholds:

1. Probe the four corners with `G30`, in the wizard's order: front-left, front-right, back-right, back-left.
2. Subtract the average from each reading.
3. **Pass if the highest and lowest differ by less than 0.05 mm.**
4. Otherwise, name the corner furthest from the average: "Lower" if above it, "Raise" if below.

`G30` is used because the wizard reports to the LCD only, but the rule and output are the same.

**Built 2026-11-03 in `internal/server/tram.go` and `internal/marlin/motion.go`. It has only run on the simulator and fakes.**

- **Run:** `G28`, then `G30` at each corner, then, if all went well, `G90`, raise to Z30, move to X115 Y115, `M400`. The request returns straight away, the run carries on if the client goes away, and `status` shows it. Each `G30` reply's X/Y must match what was asked for, within 0.1 mm.
- **Refused:** without `bed_clear`, with the plug off or unknown, not connected, temperatures unknown, an SD print running or unknown, or a run already going.
- **Limits:** silence 60 seconds, `G28` 3 minutes, `G30` 90 seconds, park commands 1 minute each. These are generous until there's real data, and the server logs each command's time and longest silence. Temperature reports don't count as activity.
- **Operation lock:** held per step, so `cool` and the idle check get a turn between steps. The firmware can't act on them during a move anyway, so `cool` can wait as long as that move's limit, and the client allows for that. `preheat` and `power off` are refused straight away during a run, so **the printer's own switch is the only stop until `estop` exists**.
- **Idle timer:** tramming doesn't extend it. If the heater targets change during a run, by the timer, `cool` or the LCD, it stops with no verdict and parks. A stuck step can hold up the idle check by up to 3 minutes, and then the existing plug cut logic applies unchanged.
- **Failures:** any failed command, or a dropped link, ends the run with no verdict and no parking, as the head's state isn't known. Readings so far stay in `status`. There's no automatic resume or power change. `Error:Printer halted` fails at once.
- **Late replies:** after a timeout, the old command's `ok` could be taken for the next command's. The link now marks itself stale, and sends `M118 sync-<n>` before the next command, discarding everything up to that echo. While the old move is still running, later commands fail with "still busy". This covers heating commands too.
- **Levelling:** `G30` leaves bed levelling off. The start G-code turns it back on with `G29 A`.
- **History:** every finished run is appended to `/var/lib/tramline/tram-history.jsonl`, because the Pi's journal doesn't survive a reboot.
- **Known gap:** tramming commands have no line numbers or checksums, as only transfers to the card do. The X/Y check only catches a corrupted `G30` after it has moved, and `G28` and park have no such check.

**`tram info` on the real printer,** 2026-11-03 at 16:33 to 16:39 GMT, with logs in `test-logs/2026-11-03-tram-info/`. It's the first real-printer run of any of this code, and nothing moved.

| Setting | Value | Meaning |
|---|---|---|
| `M118 tramline-check` | Echoed exactly, then `ok` | The resync's marker works on this board. The resync itself hasn't run here, as no command has timed out |
| `M851` | X-41.50 Y-7.00 Z-0.90 | Probe offset. From this, the probe should reach X up to 206.5, so 200 and 205 both fit |
| `C29` | L25 R205 F25 B205, 5x5, T=50 | Mesh limits, shown in `M503`. So 25/205 is the mesh, but the wizard may still use 30/200 |
| `C851` | S480 M0 | Probe Z speed 8 mm/s, multiple probing off |
| `M503` other | `M420 S0` (levelling off at boot), `M203 Z10`, `C125 X240 Y221 Z20` | Z limited to 10 mm/s, and the firmware's own park position |
| `M114` | All zero | Not homed since power-on |
| `G29 W` | Only `ok` | Printed nothing on this build, unlike the source read, so it's no use |

Connected about 10 seconds after `power on`, and `tram info` took about 1 second. USB dropped about 2 seconds after `power off`. The Pi watch ran 57 checks over 5 minutes, none bad, and the kernel log showed only the usual `urb stopped: -32` twice before the disconnect.

**First real `tram` run,** 2026-11-03 at 16:41 GMT, cold (bed 23.5°C, nozzle 22.6°C), with the user at the printer's switch. Logs are in `test-logs/2026-11-03-tram-first/`. One run only, so nothing here shows repeatability yet, and no knob has been changed.

| Corner | X, Y | Z | From average |
|---|---|---|---|
| Front-left | 30, 30 | +0.045 | +0.005 |
| Front-right | 200, 30 | +0.005 | -0.035 |
| Back-right | 200, 200 | +0.015 | -0.025 |
| Back-left | 30, 200 | +0.093 | +0.053 |

Range 0.088 mm against the 0.05 mm limit, so it failed, naming "lower back-left". Not yet compared with the wizard.

| Step | Took | Longest silence |
|---|---|---|
| `G28` | 19.53 s | 2.77 s |
| `G30`, each of 4 | 4.83 to 5.41 s | 2.19 to 2.52 s |
| Park, nearly all in `M400` | 5.03 s | 2 s |
| Whole run | About 45 s | |

So `busy:` does arrive about every 2 seconds on the real board, and `G30`'s reply parses as expected. The limits have lots of room, and are left alone until there's more data.

**Startup, same power-on:** the server's log now shows what the early `ok` follows: the board's settings and SD messages, ending with the baud rate line. It also printed `//action:notification Stored settings retrieved`, so some status messages do reach serial, despite `HOST_STATUS_NOTIFICATIONS` being off in the config read. `External Reset` appeared again.

**LCD wizard, compared,** 2026-11-03 at about 16:43 GMT, cold, run by the user from the menu and filmed. Readings were 2 minutes after the first `tram` run, with no knob changed. Video is the user's `IMG_1302.mov`, and `M114` afterwards is in `test-logs/2026-11-03-tram-first/`.

| Corner | Wizard, at 25/205 (LCD, 2 decimals) | Tramline, at 30/200 |
|---|---|---|
| Front-left | +0.01 | +0.005 |
| Front-right | -0.02 | -0.035 |
| Back-right | -0.03 | -0.025 |
| Back-left | +0.04 | +0.053 |
| Range | about 0.07 | 0.088 |
| Verdict | Lower Back Left | Lower back-left |

- **Probe points are 25 and 205.** The LCD's position bar read nozzle X66.5/246.5 and Y32/212 at the corners. Taking off the probe offset gives X25/205 and Y25/205, which is `C29`'s mesh limits. `M114` agreed at the last corner. So the source's 30/200 was wrong for this printer, and `DefaultInset` is now 25.
- **Order confirmed:** front-left, front-right, back-right, back-left, shown on the status line.
- **Rule:** same verdict and corner, and the wizard's relative values show it subtracts the average. These were at different points, so the differences of up to 0.015 mm could be from that or from noise. Rerun at `--inset 25` to tell.
- **Wizard's output:** the status line showed only corner names, not `X: Y: Z:`, and nothing reached the server's log. So the wizard's values are only on the LCD, to 2 decimals. It didn't home again, as it was already homed.

**Repeatability,** 2026-11-03 at 16:52 to 16:54 GMT, cold (bed 23°C, nozzle 22.5°C), `--inset 25 --repeat 5`, about 10 minutes after the wizard, no knob changed. Took 125 seconds. Logs are in `test-logs/2026-11-03-tram-repeat/`.

| Round | Front-left | Front-right | Back-right | Back-left | Range | Verdict |
|---|---|---|---|---|---|---|
| 1 | 0.060 | 0.030 | 0.022 | 0.080 | 0.058 | Fail, lower back-left |
| 2 | 0.050 | 0.033 | 0.027 | 0.077 | 0.050 | Fail, lower back-left |
| 3 | 0.065 | 0.040 | 0.027 | 0.082 | 0.055 | Fail, lower back-left |
| 4 | 0.065 | 0.040 | 0.025 | 0.085 | 0.060 | Fail, lower back-left |
| 5 | 0.065 | 0.043 | 0.025 | 0.087 | 0.062 | Fail, lower back-left |
| Spread | 0.015 | 0.013 | 0.005 | 0.010 | 0.012 | |

- **Noise near the limit is real.** The range moved by 0.012 mm with nothing touched, and round 2 failed only by landing exactly on the limit. A bed within about 0.01 mm of the limit could go either way between runs. No margin has been added, as that's the user's decision.
- **Possibly drift as well as noise.** Front-right and back-left mostly rose through the rounds. Five rounds are too few to separate drift from noise.
- **Resolution:** readings fall on steps of about 0.0025 mm, which is one Z step at 400 steps/mm.
- **Against the wizard at the same points:** same corner every round. Average relative values were FL +0.010, FR -0.014, BR -0.026, BL +0.031, against the wizard's +0.01, -0.02, -0.03, +0.04. The wizard's range of about 0.07 is above all five rounds' 0.050 to 0.062. It's only one wizard run, 10 minutes earlier, so whether that's noise, drift or a real difference such as stowing is unknown.
- **Timing:** `G28` 14.4 s from low down, against 19.5 s from a fresh power-on. `G30` 4.9 to 5.5 s. Longest silence in the whole run 2.57 s.

**The extra move after each probe,** noticed by the user and filmed during round 3 of that run (`probe.mov`). After each reading the head moves on to the probe point's coordinates using the nozzle, so about 41.5 mm left and 7 mm forward, then goes to the next corner. The LCD shows it: X66.50 Y32.00 to probe, X44.89 passing through toward X25 Y25, then X246.50 Y32.00 for the next. It's `do_blocking_move_to(old_pos)` in `G30.cpp`, where `old_pos` is set to the requested X/Y, and it matches `M851`'s offset. The wizard doesn't do this. It happens after the reading, at Z5.9, so it's harmless and costs about a second per corner. `G30` has no option to skip it. The LCD's `Bed X: Y: Z:` line showed 0.065, 0.040 and 0.027, matching the readings Tramline logged for that round.

**Knob direction,** 2026-11-03 at 16:59 GMT, cold, `--inset 25`, one run after the user turned the back-left knob "a bit clockwise". Logs are in `test-logs/2026-11-03-tram-knob/`.

| Corner | Before, average relative of 5 rounds | After |
|---|---|---|
| Front-left | +0.010 | -0.008 |
| Front-right | -0.014 | -0.055 |
| Back-right | -0.026 | -0.036 |
| Back-left | +0.031 | +0.100 |
| Range | 0.050 to 0.062 | 0.155 |

**Clockwise from above, turning it to the right, raised the back-left reading,** by about 0.069 mm relative to the average, well outside the noise, so counter-clockwise from above lowers it. Only back-left has been tested. The user then turned it back by eye, so the bed isn't exactly where it was, and needs probing again. Absolute Z changes after homing again, so compare relative values. Moving one corner by d only moves its relative value by 0.75d. If the screw moves 0.7 mm per turn, which is unconfirmed, that turn was about 0.09 mm, or 47 degrees.

**Turn-size hints:** offered and declined by the user, who goes by the numbers. The LCD's menu items can also probe one corner at a time, which the user finds handy when adjusting. Only the differences between corners matter to the rule, not closeness to 0.

**Tramming cold has only been used to test the tool.** The real check is at print temperature, per Print flow, and that's the first heating above 60°C on the real printer.

**First hot `tram`,** 2026-11-03 at 17:13 to 17:14 GMT, `--inset 25`, after the user turned the back-left knob back by eye. Logs are in `test-logs/2026-11-03-tram-hot/`.

- **First heating above 60°C on the real printer:** `preheat 80 150` reached nozzle 150°C and bed 79.5°C in 257 seconds. During the 5-minute soak the nozzle held 149.91 to 150.09°C and the bed 79.99 to 80.19°C.
- **Readings:** front-left +0.077, front-right +0.045, back-right +0.043, back-left +0.102. Range 0.059 mm, fail, lower back-left. From average: +0.010, -0.022, -0.024, +0.035. One run, so the range could easily be 0.01 mm different.
- **Cooling and power off:** `cool` to a nozzle of 49.5°C took 238 seconds from 150°C, with the bed still at 62°C. `power off` worked, and USB dropped about 3 seconds later. Pi watch, 5 minutes: 57 checks, none bad. The kernel log for the whole session, in `test-logs/2026-11-03-tram-first/kernel.log`, had no errors, only the usual `urb stopped: -32` twice before the disconnect.
- **Run:** took 41 seconds, with the same step timings as cold.
- **After the user turned the back-left knob slightly left, about 2 minutes later:** front-left +0.072, front-right +0.053, back-right +0.048, back-left +0.080. Range 0.032 mm, **pass**. From average: +0.009, -0.010, -0.015, +0.017. Back-left came down by about 0.018 mm from the average, so the adjustment moved it the way the knob test predicted. This is the first real pass, one run only.
- **Five rounds hot, `--repeat 5`, straight after, no knob changed:** all passed, with ranges of 0.040, 0.035, 0.032, 0.034 and 0.030 mm. Spread by corner: front-left 0.000, front-right 0.005, back-right 0.010, back-left 0.002 mm. Took 124 seconds. The right-hand corners crept up over the first rounds and then settled, so it may be drift rather than noise, and it's tighter than the cold run's 0.005 to 0.015 mm. That's one set of five rounds each, cold and hot, so it doesn't show that hot is always quieter.
- **LCD wizard, hot, straight after, run by the user:** "Corners leveled, Tolerance achieved!" Front-left +0.01, front-right -0.01, back-right -0.02, back-left +0.02, range about 0.04 mm. Tramline's five-round averages were +0.012, -0.012, -0.017, +0.017, range 0.034 mm. Every corner agrees within the LCD's 2-decimal rounding, and both pass. Photo was sent by the user in chat, and isn't in the logs.

Still to confirm on the real printer:

- **Noise near the limit.** 0.05 mm is tight, so readings close to it may flip between passes. Measure the probe's repeatability with `--repeat`, and report that instead of inventing a margin.
- **Timing and replies.** Whether `busy:` really arrives during `G28` and `G30`, real durations, and whether `M118` echoes as expected.
- **`G30` versus the wizard.** `G30` stows the probe at each point, and the wizard may not. If the numbers differ, that's the first thing to try, with `E0`.
- **Knob direction.** M4 screws move about 0.7 mm per turn, so 0.05 mm is about 26 degrees. Confirm direction on the first run.

Adjustment loop:

1. Probe and report relative heights, plus the corner to change.
2. Park the head.
3. The user adjusts and tells Claude it's done.
4. Probe again, and only continue once it passes.

The mesh from `G29 P1` still corrects remaining tilt on every print. With a limit this tight, the check may stop for adjustment fairly often, so log every reading. Real data will show whether that's a problem, and it's the user's decision if it is.

### Upload

**Built 2026-11-03 in `internal/gcode`, `internal/marlin/sd.go` and `internal/server/upload.go`. A small copy has passed on the real printer, see below. The firmware behaviour below comes from its source at tag `20231202`, whose build is close to this printer's from 17 Dec 2023. Only what's listed under the real-printer test is confirmed.**

**Real-printer test,** 2026-11-03 at 18:24 to 18:31 GMT, with `dev-20261103175429`. Logs are in `test-logs/2026-11-03-sd-upload/`.

| Item | Result |
|---|---|
| File | `sd-test.gcode`, generated `M118` lines only, 2,000 lines, 76,000 bytes on the card as `SDTEST28.GCO` |
| Saving on the Pi | 30 ms |
| Copy | 10.45 s with no resends or timeouts, so **192 lines a second, or 5.2 ms a line**, and 7.3 KB/s |
| `M20` size | Exactly the 76,000 bytes predicted, so the CR LF rule holds |
| `M28`, `M29`, `M20`, `M30` | All replied as the code expects. The card's listing afterwards matched the one before, and files in folders parse |
| Temperatures during the copy | Kept updating about every 2 seconds |
| `power off`, Pi watch for 5 minutes | USB dropped about 2 seconds later. 57 checks, none bad. Kernel log showed only the usual `urb stopped: -32` twice before the disconnect |

- **Not checked:** the copy's contents can't be read back over serial, so only its size is verified. The LCD wasn't reported on.
- **Not exercised:** resends and every failure path, as nothing went wrong. Also, this was one copy of one small file, of slightly longer lines than sliced G-code, so large-file speed is an extrapolation.
- **Where the time goes:** a test line takes about 1.9 ms to send and its `ok` about 0.7 ms, so about 2.6 ms is USB and firmware delay. Sending several lines at once removes most of the waiting, but the board's receive buffer size is unknown.

**Two modes, decided by the user.** Every file is saved on the Pi first, so the client machine can then go away. After that:

| Mode | What happens | Good for | Risk |
|---|---|---|---|
| `auto`, the default | SD if the copy would take no longer than the limit, otherwise stream | | |
| `sd` | Copied to the card, then printed from it | Survives a Pi hang, restart or lost link | A wait before printing |
| `stream`, **printing not built yet** | Stays on the Pi, and is sent line by line during the print | Starts at once | The print stops if the Pi or link fails, possibly with a hot nozzle sitting on the part |

`tramline mode` changes the default, which is kept on the server. `upload --sd` and `upload --pi-only` override it once. OctoPrint streams by default, and its optional SD upload uses the same `M28` method and is slow too, going by memory rather than testing.

**Flow:**

1. **Save.** `PUT /v1/files/<name>` with a SHA-256 header stores the file in `/var/lib/tramline/uploads/`, with a JSON record beside it. It works with the printer off, and doesn't touch the printer.
2. **Check.** Comments and blank lines are stripped, and a file is refused if a line has `*`, `\`, control or non-ASCII bytes, `M28`, `M29` or `M110`, or is over 80 characters. Largest upload 256 MB, and 1 GB of disk must stay free. All 18 sliced files scanned would pass, with a longest line of 40 characters.
3. **Copy.** Runs in the background. `N0 M110 N0`, `N1 M28 <name>`, the numbered lines, then a numbered `M29`. Since `dev-20261103184515`, up to 120 bytes may be unanswered at once, about 3 lines, see Window below. Stage 8's real test sent one line at a time.
4. **Verify.** `M20`'s size must exactly match the sum of each stripped line plus CR LF. That catches lost lines but not a same-length change on the card, which the checksums make unlikely.

**What the firmware does, and what it means here:**

| Firmware | Consequence |
|---|---|
| `M28` aborts a running SD print | `printerBlocker`'s `M27` runs straight before it, and anything unknown refuses |
| While saving, an unnumbered command gets an error, `Resend` and `ok`, and doesn't run. Only `M29` is accepted | `cool`, `M27` and `M118` can't run mid-copy. The link tries `M29` first if a save may be open |
| The save stays open if the link drops while the board has power | The handshake sends `M29` on the "No Checksum" error, otherwise it would never reconnect |
| The emergency parser is off during a save | `M112` won't work mid-copy. **`estop` must cut the plug or send `M29` first** |
| A repeated line number gets no reply at all | After a timeout, send the *next* line. The firmware takes it, or asks for the missing one |
| `M28` gives a plain `ok` with no card mounted | `Writing to file:` must be seen |
| `;` starts a comment before the checksum is read | `;TIME:` can't be sent as written, so comments go, and the LCD may lack time estimates. `\;TIME:` looks possible from the source, and hasn't been tried |
| `M29` anywhere in a line closes the file, and the first `*` ends the saved text | Such files are refused |
| `M28` empties an existing file, and there's no rename | Partial files stay under the real name. A name already on the card that didn't come from here is refused |

**Operation lock:** the copy holds it from start to finish, as nothing else could run then. `tram`, `preheat`, `tram info`, `power` and `files rm` are refused straight away. `status` still works, and `files` leaves out the card. A copy is refused during a tram run or a print.

**Idle timer:** a copy doesn't extend it. `cool` and an expired timer stop the copy with `M29` and then take the lock, so heaters always win. If `M29` fails, the existing plug cut logic applies. During a copy the idle check skips `M27` and relies on temperature reports continuing, which they did on the real printer.

**Failures:** a dead board fails after 3 timeouts in a row at 5 seconds each, or 10 resends with no progress. Any failure deletes the partial file with `M30` if the link is up. If it isn't, the record says a partial file may remain, as the LCD could still start it. There's no automatic retry or power change. A record left as running after a restart reads as failed.

**Window,** added 2026-11-03 at the user's request, tested on the real printer, see below. Each `ok` frees its line's share of the window. The firmware's `stm32_serialbuffer.py` gives this board a 128-byte receive buffer, assuming the official build used the default, so 120 bytes can't overflow it even if a card write stalls the board. `ok N<line>` answers everything up to that line, and a plain `ok` answers the oldest. Ending a save early sends `M29` and then waits for an `M118` marker's echo, because an `ok` could be for an earlier line, and the firmware refuses the marker during a save. After `Resend`, sending stops until replies have been quiet for 200 ms, then restarts from the line asked for. After silence, it sends on, as with one line at a time. `copy --window <bytes>` overrides it for measuring: 1 is one line at a time, and the most is 2048. Past 128 bytes, an overflow is possible, and would be caught by the checksums and resent.

**Retrying:** run `tramline upload <file>` again, or `tramline copy <name>` from any client. The file comes from the Pi, and a verified copy whose size still matches is skipped. That relies on the record and `M20`'s size, so a swapped card with the same name and size would fool it.

**Names:** 8.3 for now, such as `PIDECK3F.GCO`: up to 6 letters and digits from the name, then 2 hex digits from its hash. Long names are enabled in the firmware but untested.

**Binary transfer:** the firmware reports `BINARY_FILE_TRANSFER`, but the wiki describes it for firmware upgrades. Treat it as a later speedup.

An SD card must stay in the printer.

**Window test on the real printer,** 2026-11-03 at 18:47 to 18:55 GMT, with `dev-20261103184515`. Logs are in `test-logs/2026-11-03-sd-window/`. Every copy's size matched on `M20`, and the card's listing afterwards matched the one before.

| File | Window | Time | Speed | Lines a second | Resends |
|---|---|---|---|---|---|
| 2,000 `M118` lines | 1 byte, one line at a time | 10.47 s | 7.3 KB/s | 191 | 0 |
| Same | 120 bytes | 5.42 s | 14.0 KB/s | 369 | 0 |
| Same | 240 bytes | 6.30 s | 12.1 KB/s | 317 | 10 |
| Same | 480 bytes | 5.86 s | 13.0 KB/s | 341 | 8 |
| `test-esp-jig_PETG.gcode`, 15,264 lines | 120 bytes | 31.28 s | 14.0 KB/s | 488 | 1 |

- **120 bytes stays the default.** It was the fastest, and going past the buffer only caused resends. It's one run of each, so small differences mean little, but a window much above 120 shows no gain.
- **Speed limit:** the sliced file used about 19.5 of the link's 25 KB/s, so little more can be gained on this link.
- **Resends past the buffer:** at 240 and 480 they came about every 216 lines, or about 8 KB of file. That fits the board stalling every 8 KB on the card and overflowing its receive buffer, which supports the 128-byte figure. The stall's cause is a guess.
- **The one resend at 120 bytes,** at line 1343 of the sliced file, isn't explained. An overflow shouldn't be possible at that window, and it happened once where the file crossed about 53 of those 8 KB points. It may be a damaged line on the wire. The firmware's error text wasn't logged then, and now is. The copy recovered and its size matched.
- **`ok N2 P15 B15`** confirmed as the reply format on the real board.
- **Resends worked on real hardware,** 19 times, always with the right size afterwards. Size is all that can be checked.

**Copy times,** estimated from this test. The low figure scales by lines at 488 a second, the high one by size at 14 KB/s, using one file's measurement:

| File | Lines | On the card | Estimated copy |
|---|---|---|---|
| `test-esp-jig_PETG.gcode` | 15,264 | 0.44 MB | 31 s, measured |
| `pi-deck-slide_PETG.gcode` | 130,461 | 4.2 MB | about 4.5 to 5 min |
| `stand-print_PETG.gcode` | 389,414 | 12.7 MB | about 13 to 15 min |
| `stargate_gate_PETG.gcode` | 885,702 | 28.6 MB | about 30 to 34 min |

**Choosing a mode:** `auto`, the default since the user chose it, copies a file to SD unless the estimate at 14 KB/s is over the limit, 5 minutes unless changed with `tramline mode auto --limit <minutes>`. So files up to about 4.2 MB go to the card. Larger ones stay on the Pi for streaming, which **isn't built yet**, so until then they can't be printed that way. `tramline copy <name>` or `upload --sd` copies them anyway. The choice is made by `upload` from the size alone, and nothing checks the estimate against the real time.

**Before streaming is used for long prints:** a hung Pi would stop a print with the nozzle possibly still hot on the part, and that Pi has hung once. Streaming needs line numbers on every line, and a safeguard to look for in the firmware, such as host-silence handling, which hasn't been checked.

**Simulator tests:** corrupted, lost and duplicate lines, lost replies, `M29` lost or corrupted, write errors, no card, a silent board, link drops, reconnecting into an open save, power loss mid-copy, cancelling, `cool` and the timer stopping a copy, every refusal, and the CLI end to end with a real sliced file. The simulator was written from the firmware source, so it can share any misreading of it.

**LCD during a copy:** it showed the file name, reported by the user, as the source's `ui.set_status` suggests.

**Still to confirm on the real printer:** the speed of a large file, and `M29` at reconnect.

### Hue plug

The `tramline` server on the Pi handles all Hue communication directly, as the user asked. Clients only ever see `power on` and `power off`.

Bridge found 2026-11-02 through Hue's discovery service, then confirmed with its unauthenticated `/api/config`:

| Field | Value |
|---|---|
| Address | `192.168.1.128` |
| Bridge ID | `001788FFFE123456` |
| Model | `BSB002` |
| API version | `1.78.0` |

Tested on the real bridge and plug on 2026-11-02, with logs in `test-logs/2026-11-02-hue-plug/`. Times are GMT.

| Test | Result |
|---|---|
| Bridge certificate | Common name is `001788FFFE123456`, issued by `root-bridge`, and pairing accepted it |
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

### Safety rules

- Ask for bed-clear confirmation before every print, since there's no camera and a leftover part could be hit. The server refuses `print` without it.
- Turn heaters off after 30 idle minutes. See Heating below. Simulator only so far.
- Park the head before knob adjustments, and remind the user that the bed is hot.
- `power off` is refused while the heaters are on, the nozzle is at 50°C or above, or a connected printer's temperatures aren't known yet. Done.
- Never cut power while the nozzle is above about 50°C, so the hotend fan can keep cooling it, except for an emergency stop.
- Never cut power during a print, except for an emergency stop.
- Never send heat commands when the plug is off, as a failed heat-up could trigger a thermal error.
- Enforce temperature limits on the server: bed 100°C and nozzle 260°C, decided by the user. Anything above them is rejected. A stock hotend's PTFE liner starts to degrade at around 250°C, so the limit is above what this hotend should run at for long.
- Keep these rules in the server, so they hold for every client and if a client disconnects. A lost client must not leave heaters on indefinitely, so the idle timeout applies.
- Rely on the firmware's `THERMAL_PROTECTION`, and don't try to replace it in software.
- Have the service recover cleanly after a restart, by reading the printer's state and not assuming anything.

### Heating

Built 2026-11-03 in `internal/server/heat.go` and `internal/marlin/printer.go`.

Tested on the real printer on 2026-11-03 at 15:14 to 15:22 GMT, with the user watching. Logs are in `test-logs/2026-11-03-heating/`.

| Test | Result |
|---|---|
| `preheat 40 60` | Accepted, with the targets read back. Nozzle 60.0°C and bed 39.8°C after about 59 seconds |
| `power off` while heating | Refused, "the heaters are on". Plug stayed on |
| `cool` | Targets 0 straight away |
| `power off` just after `cool` | Refused, "the nozzle is 60 C" |
| Nozzle cooling | 60°C to 49.2°C in about 48 seconds. Bed still rose to 40.4°C after `cool`, as it lags |
| `power off` at 49.2°C | Worked, USB dropped about 2.5 seconds later |
| Pi watch, about 5 minutes after | 57 checks of ping, login, `sync`, throttling and failed units, none bad. Kernel log showed no errors, only the usual `urb stopped: -32` twice before the disconnect |

`M27` was accepted without an error, so its idle reply matches what the code recognises. The reply while printing hasn't been seen. The temperatures used were low, so high-temperature behaviour is still untested.

**Idle timeout,** tested on the real printer on 2026-11-03 at 15:24 to 16:00 GMT, with `dev-20261103151749`. Logs are in `test-logs/2026-11-03-idle-timeout/`.

| Test | Result |
|---|---|
| `preheat 40 60` at 15:24:20 | Server gave 15:54:20 as the shutoff |
| Polling `status` every 2 seconds for 30 minutes | 849 samples, all with the same shutoff time, so polling didn't extend it. No failed samples or warnings |
| Shutoff | Server logged `heaters off, idle timeout` at 15:54:23.8, 3.7 seconds after the limit, with no client command |
| `power off` at 49.4°C | Worked, about 50 seconds after the shutoff |
| Holding temperature, 16:30 local to shutoff | Nozzle 59.91 to 60.18°C, bed 39.92 to 40.17°C |
| Pi watch, about 5 minutes after | 62 checks of ping, login, `sync`, throttling and failed units, none bad. Kernel log showed no errors, only the usual `urb stopped: -32` twice before the disconnect |

**Late `M115` reply, fixed 2026-11-03:** waiting 10 seconds didn't remove the 45 log lines, because an early `ok` ended the first `M115` wait just before the reply arrived. The handshake now ends only on an `ok` after the firmware line. On the real board that gave no `Cap:` lines and connected at 15:24:10.4, about 5.2 seconds after the startup text. That's only about 0.5 seconds faster than before. The early `ok` follows the board's own startup output, ending `echo: Serial 0 baud rate set to 250000`, with no `Unknown command`, so it isn't the reply to the garbage bytes. Which command it answers is unknown.

**Boot banner seen:** on this power-on the server caught the board's startup text, including `start` and `External Reset`, about 0.8 seconds after USB appeared. So the earlier unconfirmed note that `start` is sent before USB is up doesn't always hold. It's unknown why it said `External Reset` for a power-on, or whether opening the port played a part.

- **`preheat`** needs the plug to read on and the printer to be connected with known temperatures and no SD print running. Anything unknown refuses. It sends `M140`, `M104` and `M105`, and checks the reported targets. If that fails, it tries to turn both heaters off.
- **`cool`** always sends the off commands if connected, even during an SD print, as it's a safety command. With no connection, it succeeds only if the plug is confirmed off.
- **Idle timeout:** decided by the user, only a new `preheat` restarts it, so a polling script can't keep the heaters on. The check runs every 5 seconds and goes by the targets the printer reports, so heaters set from the LCD, or found on after a server restart, also get 30 minutes. A running SD print, checked with `M27` every 30 seconds, keeps restarting the timer.
- **Last resort,** decided by the user: if the timer has expired and the heaters can't be turned off, because the link is down or the board isn't answering, the plug is cut 1 minute later. The hotend fan stops too, which is accepted as the lesser risk. If the last `M27` showed an SD print running, the plug is left on and `status` shows a warning, because an SD print survives losing the link. A lost link with a print that started less than 30 seconds earlier could be missed.

**What the timeout can't cover:**

- If the Pi hangs, nothing turns the heaters off, and only the firmware's thermal protection remains.
- `systemctl stop` leaves the heaters on with no timer. After a crash or restart, the timer starts again at a full 30 minutes once the printer is connected.
- If the server restarts while the USB link is also down, it doesn't know the heaters were on.
- The plug cut has only run against a fake plug. Testing it for real means pulling the USB cable while heating.

## Test plan

Each stage must pass before moving on. **Hue comes first, so Claude can power the printer for later tests without the user being at the plug. Claude still asks the user before every test that involves the printer.**

0. Controlled power-off with the Pi connected. Passed three times on 2026-11-02, see above. Keep capturing kernel logs during later power testing.
1. Build against a simulator and fake bridge, including `busy:`, garbage bytes and disconnects, resends and rejected certificates. Done.
2. User installs the update and allows this machine's fingerprint. Log in. Done 2026-11-02.
3. Pair with the real bridge, find the plug's UUID, and put it in the config. Done 2026-11-02.
4. Plug control from `tramline power`, with the printer idle, and confirm the plug state. Passed 2026-11-02, three on and three off.
5. Read-only printer status through the real USB adapter, including power cycling and reconnecting, and whether reconnecting restarts the board. Passed 2026-11-02, with the printer idle.
6. Heating, with the user watching. Passed 2026-11-03 at low temperatures: heat, cool, power-off refusals and the 30-minute timeout.
7. `G30` probing, with the user watching, compared against the LCD wizard, including probe points and repeatability. Passed 2026-11-03, hot and cold, and the wizard agrees in both cases.
8. Small SD upload, verified with `M20`, and timed for a real speed figure. Passed 2026-11-03: a sliced file of 15,264 lines in 31 s at 14 KB/s, with resends working. Other failure handling is still simulator only.
9. Start a small test print. Check for an LCD confirmation prompt, and confirm cancelling works. Passed 2026-11-03: no prompt, a good print, and cancelling works hot. Test files in `test-logs/2026-11-03-print/`.
10. Printing by streaming. Passed 2026-11-04, including a 63-minute job, cancels, the LCD's Stop, `M85` firing and a restart. A link dropping mid-print is simulator only.
11. Full flow, end to end.

## Open questions

1. What caused the one-off Pi hang, and why did the Hue plug switch off by itself once?
2. ~~Does a remotely started SD print wait for confirmation on the LCD?~~ No, tested 2026-11-03 with a cold file. See Print design.
3. Is `G30` available and behaving the same way on the Dec 2023 build?
4. ~~What are this printer's tramming points?~~ 25 and 205, the mesh limits in `M503`'s `C29` line. See Tram check.
5. ~~What's the real upload speed?~~ 14 KB/s with a 120-byte window, on one small sliced file. See Upload.
7. What caused the one resend at a safe window? Every command will get a line number and checksum either way, decided by the user.
6. Should the firmware be updated, or left on the current build?

## To do outside the code

- The user will review Hue rooms and routines for "Light plug" later.

## Ideas parked

**Motorised knobs.** Pi-controlled motors could turn the knobs without the user. It's feasible, but advised against for now because the mesh already covers normal tilt and someone still has to remove prints. The hard parts:

- knobs move along their screws as they turn, so couplings must allow for this
- under-bed clearance across the Y travel hasn't been measured
- PETG softens near the 80°C bed temperature
- small printed gears are a known weak spot on this printer
- extra moving mass and flexing cables on the Y axis
- no position feedback, so software limits are essential

Log real drift first, then try one motor on one corner if it's justified. Solid bed spacers may be a simpler fix.

**Other later ideas:** binary upload, a camera, an MCP server wrapping the API.

## Related files

- `../cura/`: slicing scripts, PETG profile and start G-code
- `../cura/start_gcode_PETG.txt`: the start sequence referred to above, which keeps `G29 P1`, `G29 A` and `C108`
