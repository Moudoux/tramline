# Firmware findings

Part of the Tramline engineering notes, see the [index](README.md). Other sections are named in the text, as in "see Web page": the index says which file each is in.

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
