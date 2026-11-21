# Tramline engineering notes

The record of Tramline's design, firmware findings, test results and open issues. For what it is and how to use it, see the [README](../README.md).

**A read-only web page, `tramline web`, is installed on the Pi, see Web page. Status: running on the Pi with mutual TLS, paired with the real Hue Bridge, and `tramline power` works on the real plug. Stage 5, read-only printer status, has passed on the real printer. Stage 6, heating, has passed on the real printer, including the 30-minute idle timeout at low temperatures. `tramline tram` works on the real printer. Stage 7 has passed on the real printer. Stage 8, SD upload, has passed on the real printer, at 14 KB/s with a sliced file. Stage 9, `print`, `cancel` and `estop`, has passed on the real printer with a cold file and a small real print. Tram before every print is in use, with only its pass path run on the real printer. The first long print, 4 h 10 min, finished cleanly on 2026-11-04. Stage 10, printing by streaming, has passed on the real printer, including a 63-minute job that finished cleanly on 2026-11-04 with no resends, see Streaming design.** Last updated 2026-11-06.

| Part | State |
|---|---|
| Container build, dependency check | Done |
| Serial port, Marlin protocol, reconnecting | Done, tested on a pseudo-terminal and simulator. On the real printer it has identified the firmware, read temperatures and reconnected after power cycles and a server restart. It has also heated and moved it. It has also copied a file to the card with line numbers and checksums. Resends have only run on the simulator |
| HTTPS with mutual TLS and allow list | Done |
| `init`, `allow`, `serve`, `keygen`, `login`, `status` | Done |
| Hue pairing, plug listing, `power on\|off` | Done, tested on the real bridge and plug, including the refusal to power off while hot |
| systemd unit, installed by the `.deb` (`deploy/install.sh` before it) | Done, installed and running |
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
| Desktop GUI, `tramline gui` | Built 2026-11-05, with everything the client's commands do and uploads from the browser. `go test` and headless Chrome on the simulator only, not yet used with the real Pi. See Desktop GUI |
| Mesh heights, `tramline mesh` | Built 2026-11-06: `M420 V1 T1`, read-only in the firmware's source, in the CLI and the GUI as text. `go test` and headless Chrome on the simulator only, installed 2026-11-06. **On the real printer 2026-11-06: the stored mesh after power-on, and a print's fresh mesh just after it ended. Mesh lines arrive during a card print too.** Read by itself after each print that probed a mesh, and drawn in the GUI as a coloured surface: built 2026-11-06, simulator only, not installed. See Built: mesh heights, and Built: mesh surface and the read after each print |
| Probe view, the head live during a tram check and the mesh | Built 2026-11-05 for the GUI. Tram check: phase, target and step time in status, and the 3D view's probe points, moving head and reading pins. Mesh: each "Probing mesh point N/25." into the print job, and the 3D view's 5×5 grid filling in with the head at an estimated point, by UBL's order, which fits the real log's timing. `go test` and headless Chrome on the simulator only, not installed. On 2026-11-06 a card print showed the mesh lines arrive from the card too, and the user's video showed the real head following the assumed order. See Probe view |
| Checksums in `files`, `status` and both web pages | Built 2026-11-21: the Pi's files read again when listed, checked before a stream and during a copy, the card's sum from the copy. `go test` and headless Chrome on the simulator only, not installed. See Checksums |
| Packages: PKGBUILD for Arch, arm64 `.deb` for the Pi, installed by arch-config | Built 2026-11-21. The Arch package 0.1.0-1 is installed on the desktop. The `.deb` 0.1.0 is released as `tramline-v0.1.0` and installed on the Pi through arch-config, 2026-11-21. See Packages |
| `tramline update` | Not started |

## Files

Read [next-session.md](next-session.md) first. Paths in the notes, such as `internal/server/web.go`, `test-logs/` or `../cura/`, are relative to the `tramline/` folder, not this one. Read the others only for the topic in hand.

| File | What is in it |
|---|---|
| [next-session.md](next-session.md) | Current state, what to do next, what is not proven on the real printer, open issues, rules |
| [goal-and-hardware.md](goal-and-hardware.md) | Goal, hardware, verified connection, M115 capabilities, the USB cable |
| [firmware.md](firmware.md) | Tramming, the mesh (which command prints it and which mesh a read finds), serial and host behaviour, custom codes |
| [architecture.md](architecture.md) | Language, build and dependencies, code structure, elevated commands, the secrets rule |
| [security.md](security.md) | Mutual TLS, allow list, listening addresses, what it does not solve |
| [install-and-config.md](install-and-config.md) | Install and updates, config, listening by interface (LAN and Tailscale) |
| [api.md](api.md) | Every CLI command and its API endpoint |
| [web-page.md](web-page.md) | tramline web, the 3D view, how it shares the port, trying it on the simulator |
| [probe-view.md](probe-view.md) | Live view of the head during the tram check and the mesh probing (both built, simulator only): what the firmware reports, UBL's point order and its fit to the real log, the plan, what's built, and a warning about testing on the simulator |
| [gui.md](gui.md) | tramline gui: the local web GUI with everything the CLI does, its sign-in, routes, page, tests and what isn't proven |
| [printing.md](printing.md) | Print flow and print design, with firmware findings and results |
| [streaming.md](streaming.md) | Printing by streaming, lost lines, the long print |
| [tram-before-printing.md](tram-before-printing.md) | The tram check before every print, the wizard, print --continue |
| [scheduled-prints.md](scheduled-prints.md) | schedule, auto-off, --from pi|sd, CLI changes |
| [tram-check.md](tram-check.md) | tramline tram: probing, limits, knob direction, history |
| [upload.md](upload.md) | upload, copy, files, resends, SD speed, checksums |
| [hue-plug.md](hue-plug.md) | Pairing, plug UUID, power control |
| [safety-and-heating.md](safety-and-heating.md) | Server-enforced safety rules, preheat, cool, idle timeout |
| [testing-and-questions.md](testing-and-questions.md) | Test plan, open questions, to do outside the code |
| [ideas-and-related.md](ideas-and-related.md) | Motorised knobs, later ideas, related files |

## Where each section is

The text refers to sections by name, as in "see Stand print result". Sections marked · are inside the one above, and · · marks a bold paragraph title.

| Section | File |
|---|---|
| · · Newest, 2026-11-05, late | [next-session.md](next-session.md) |
| · · Newest, 2026-11-05 | [next-session.md](next-session.md) |
| · · Installed and committed | [next-session.md](next-session.md) |
| · · Current state, 2026-11-04, evening | [next-session.md](next-session.md) |
| · · Next | [next-session.md](next-session.md) |
| · · Installed versus source | [next-session.md](next-session.md) |
| · · Print design | [next-session.md](next-session.md) |
| · · Bed state | [next-session.md](next-session.md) |
| · · Rules | [next-session.md](next-session.md) |
| Goal | [goal-and-hardware.md](goal-and-hardware.md) |
| Hardware | [goal-and-hardware.md](goal-and-hardware.md) |
| Verified: connection | [goal-and-hardware.md](goal-and-hardware.md) |
| · Read-only status through the server | [goal-and-hardware.md](goal-and-hardware.md) |
| · M115 capabilities | [goal-and-hardware.md](goal-and-hardware.md) |
| The USB cable | [goal-and-hardware.md](goal-and-hardware.md) |
| · Tramming | [firmware.md](firmware.md) |
| · Mesh | [firmware.md](firmware.md) |
| · Serial and host behaviour | [firmware.md](firmware.md) |
| · Custom codes | [firmware.md](firmware.md) |
| · · Possible blocker | [firmware.md](firmware.md) |
| · Language: Go | [architecture.md](architecture.md) |
| · Build and dependencies | [architecture.md](architecture.md) |
| · Structure | [architecture.md](architecture.md) |
| · Elevated commands on the Pi | [architecture.md](architecture.md) |
| · Secrets rule | [architecture.md](architecture.md) |
| · · What this doesn't solve | [security.md](security.md) |
| Install and updates | [install-and-config.md](install-and-config.md) |
| Config | [install-and-config.md](install-and-config.md) |
| Listening by interface | [install-and-config.md](install-and-config.md) |
| · · On a phone | [web-page.md](web-page.md) |
| · · How it shares the port | [web-page.md](web-page.md) |
| · · It can't affect a print | [web-page.md](web-page.md) |
| · · Not drawn | [web-page.md](web-page.md) |
| · · No libraries | [web-page.md](web-page.md) |
| · Trying it on the simulator | [web-page.md](web-page.md) |
| Probe view | [probe-view.md](probe-view.md) |
| · What's known | [probe-view.md](probe-view.md) |
| · Plan | [probe-view.md](probe-view.md) |
| · Built: tram check | [probe-view.md](probe-view.md) |
| · Built: mesh | [probe-view.md](probe-view.md) |
| · Built: mesh heights | [probe-view.md](probe-view.md) |
| · Built: mesh surface and the read after each print | [probe-view.md](probe-view.md) |
| · Proposed printer tests | [probe-view.md](probe-view.md) |
| · · Rules | [probe-view.md](probe-view.md) |
| Desktop GUI, `tramline gui` | [gui.md](gui.md) |
| · · Shape | [gui.md](gui.md) |
| · Code | [gui.md](gui.md) |
| · Authenticating the browser | [gui.md](gui.md) |
| · How requests reach the Pi | [gui.md](gui.md) |
| · The page | [gui.md](gui.md) |
| · Trying it on the simulator | [gui.md](gui.md) |
| · · Checking the page without a screen | [gui.md](gui.md) |
| · Tested | [gui.md](gui.md) |
| · Not proven | [gui.md](gui.md) |
| Print flow | [printing.md](printing.md) |
| Print design | [printing.md](printing.md) |
| · · Simulator tests | [printing.md](printing.md) |
| · · Read-only test on the real printer | [printing.md](printing.md) |
| · · Cold print tests on the real printer | [printing.md](printing.md) |
| · · Server restart during a cold print | [printing.md](printing.md) |
| · · First real print | [printing.md](printing.md) |
| · · Cancelling a real print | [printing.md](printing.md) |
| · · Tests, each asked for separately | [printing.md](printing.md) |
| · · Tests, each asked for separately | [streaming.md](streaming.md) |
| · · Decided by the user 2026-11-04 | [streaming.md](streaming.md) |
| · · First stream on the real printer | [streaming.md](streaming.md) |
| · · Cold cancel on the real printer | [streaming.md](streaming.md) |
| · · LCD during a stream | [streaming.md](streaming.md) |
| · · First real print by streaming | [streaming.md](streaming.md) |
| · · Cancelling a real stream | [streaming.md](streaming.md) |
| · · The LCD's Stop during a real stream | [streaming.md](streaming.md) |
| · · Long print by streaming | [streaming.md](streaming.md) |
| · · A copy on the rewritten `sendLines` | [streaming.md](streaming.md) |
| · · Simulator tests | [streaming.md](streaming.md) |
| · · First real run | [tram-before-printing.md](tram-before-printing.md) |
| · · Stand print start | [tram-before-printing.md](tram-before-printing.md) |
| · · Stand print result | [tram-before-printing.md](tram-before-printing.md) |
| · · Lost lines: offline investigation | [tram-before-printing.md](tram-before-printing.md) |
| · · Lost lines: test A | [tram-before-printing.md](tram-before-printing.md) |
| · · Lost lines: test B | [tram-before-printing.md](tram-before-printing.md) |
| · · Simulator tests | [tram-before-printing.md](tram-before-printing.md) |
| · · Simulator run of these changes | [scheduled-prints.md](scheduled-prints.md) |
| · · Tests | [scheduled-prints.md](scheduled-prints.md) |
| · · Simulator run with the real binaries and clock | [scheduled-prints.md](scheduled-prints.md) |
| · · Not proven on the real printer | [scheduled-prints.md](scheduled-prints.md) |
| · · First real `tram` run | [tram-check.md](tram-check.md) |
| · · Startup, same power-on | [tram-check.md](tram-check.md) |
| · · LCD wizard, compared | [tram-check.md](tram-check.md) |
| · · Repeatability | [tram-check.md](tram-check.md) |
| · · The extra move after each probe | [tram-check.md](tram-check.md) |
| · · Knob direction | [tram-check.md](tram-check.md) |
| · · Turn-size hints | [tram-check.md](tram-check.md) |
| · · First hot `tram` | [tram-check.md](tram-check.md) |
| · · Real-printer test | [upload.md](upload.md) |
| · · Operation lock | [upload.md](upload.md) |
| · · Idle timer | [upload.md](upload.md) |
| · · Failures | [upload.md](upload.md) |
| · · Window | [upload.md](upload.md) |
| · · Retrying | [upload.md](upload.md) |
| · · Names | [upload.md](upload.md) |
| · · Binary transfer | [upload.md](upload.md) |
| · · Window test on the real printer | [upload.md](upload.md) |
| · · Copy times | [upload.md](upload.md) |
| · · Choosing a mode | [upload.md](upload.md) |
| · · Before streaming is used for long prints | [upload.md](upload.md) |
| · · Simulator tests | [upload.md](upload.md) |
| · · LCD during a copy | [upload.md](upload.md) |
| · · Still to confirm on the real printer | [upload.md](upload.md) |
| Checksums | [upload.md](upload.md) |
| Safety rules | [safety-and-heating.md](safety-and-heating.md) |
| Heating | [safety-and-heating.md](safety-and-heating.md) |
| · · Idle timeout | [safety-and-heating.md](safety-and-heating.md) |
| · · Late `M115` reply, fixed 2026-11-03 | [safety-and-heating.md](safety-and-heating.md) |
| · · Boot banner seen | [safety-and-heating.md](safety-and-heating.md) |
| Test plan | [testing-and-questions.md](testing-and-questions.md) |
| Open questions | [testing-and-questions.md](testing-and-questions.md) |
| To do outside the code | [testing-and-questions.md](testing-and-questions.md) |
| Ideas parked | [ideas-and-related.md](ideas-and-related.md) |
| · · Other later ideas | [ideas-and-related.md](ideas-and-related.md) |
| Related files | [ideas-and-related.md](ideas-and-related.md) |
